// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// The restore playbook, RESTORE.md: an ordered checklist for recreating the
// exported cluster in another cluster, with the commands filled in from the
// snapshot. Each step is written only when it applies. SetDrift runs none
// of the commands; the ones for the source cluster are the only writes to
// it and the user runs them.

const (
	restoreFile = "RESTORE.md"
	restoreDir  = "restore"
	// autoModeEBSDriver is EKS Auto Mode's EBS CSI driver, which only
	// attaches volumes tagged eks:eks-cluster-name=<cluster>.
	autoModeEBSDriver = "ebs.csi.eks.amazonaws.com"
	irsaAnnotation    = "eks.amazonaws.com/role-arn"
)

// playbook accumulates RESTORE.md: what comes before the first step, then
// the numbered steps, numbered when rendered, so a step can be inserted
// before others once its content is known.
type playbook struct {
	head  bytes.Buffer
	steps []*step
}

// step is one numbered section of a playbook.
type step struct {
	title string
	body  bytes.Buffer
}

// printf writes to the current step, or before the first.
func (p *playbook) printf(format string, args ...any) {
	w := &p.head
	if n := len(p.steps); n > 0 {
		w = &p.steps[n-1].body
	}
	fmt.Fprintf(w, format, args...)
}

// section starts a numbered step.
func (p *playbook) section(title string) {
	p.steps = append(p.steps, &step{title: title})
}

// titles are the steps' titles, in order.
func (p *playbook) titles() []string {
	out := make([]string, len(p.steps))
	for i, s := range p.steps {
		out[i] = s.title
	}
	return out
}

// render returns the playbook with its steps numbered.
func (p *playbook) render() []byte {
	var b bytes.Buffer
	b.Write(p.head.Bytes())
	for i, s := range p.steps {
		fmt.Fprintf(&b, "\n## %d. %s\n\n", i+1, s.title)
		b.Write(s.body.Bytes())
	}
	return b.Bytes()
}

// code writes a shell block.
func (p *playbook) code(lines ...string) {
	p.printf("```sh\n%s\n```\n\n", strings.Join(lines, "\n"))
}

// restoreData is what writeRestore gathers before writing.
type restoreData struct {
	s       *snapshotv1alpha1.Snapshot
	written []writtenFile
	remotes []remoteSecret
	region  string
	// source is the source cluster's name for aws commands.
	source string
	eks    bool
	// helmRepos is the --helm-repos file, or nil
	helmRepos *helmsource.Repos
	// secretService is the ExternalSecrets' AWS service, SecretsManager or ParameterStore
	secretService string
	// secrets are the export's ExternalSecrets settings, resolved for the platform
	secretSettings *ExternalSecrets
	// cls is the source's platform; platDir its directory in the export
	// (platform/aws/eks), empty when it is not known.
	cls     *platform.Classification
	platDir string
	// platformObjects is how many objects only the platform set has.
	platformObjects int
	// manifests are the resources' manifests, decoded once
	manifests manifests
	// The platform's playbooks, for a new cluster on the same platform:
	// awsp is platform/aws/RESTORE.md (AWS-wide: volumes, secrets, IDs),
	// eksp platform/aws/eks/RESTORE.md (EKS settings and identity). Nil
	// when the source is not on AWS, or not on EKS. RESTORE.md itself has
	// only the steps that work on any cluster.
	awsp, eksp *playbook
}

// classification is the snapshot's platform; a snapshot from before the
// classifier, or a test's, may only say "eks".
func classification(s *snapshotv1alpha1.Snapshot) *platform.Classification {
	c := s.GetCluster()
	if c.GetClassification() != nil {
		return platform.FromProto(c.GetClassification())
	}
	if c.GetPlatform() == string(platform.EKS) || s.GetInventory().GetAws() != nil {
		return platform.Decide([]platform.Signal{{Source: "snapshot", Detail: "platform eks",
			Distribution: platform.EKS, Infrastructure: platform.AWS, Weight: platform.Strong}})
	}
	return &platform.Classification{}
}

// writeRestore writes RESTORE.md and restore/volumes/ for the export.
func (e Emitter) writeRestore(out *outDir, s *snapshotv1alpha1.Snapshot, written []writtenFile, remotes []remoteSecret, platformObjects int) error {
	d := &restoreData{s: s, written: written, remotes: remotes, helmRepos: e.HelmRepos, platformObjects: platformObjects}
	if e.ExternalSecrets != nil {
		d.secretSettings = e.ExternalSecrets.resolved(classification(s))
		d.secretService = d.secretSettings.Service
	}
	if r, ok := regionOf(s); ok {
		d.region = r
	} else {
		d.region = cmp.Or(e.Region, regionPlaceholder)
	}
	d.source = s.GetInventory().GetAws().GetClusterName()
	if d.source == "" {
		d.source = s.GetCluster().GetName()
	}
	d.cls, d.platDir = exportPlatform(s)
	d.eks = d.cls.Is(platform.EKS) || s.GetInventory().GetAws() != nil

	srcContext := e.SourceContext
	if srcContext == "" {
		srcContext = "<source-context>"
	}
	p := &playbook{}
	c := s.GetCluster()
	p.printf("# Restoring %s to another cluster\n\n", comment(c.GetName()))
	p.printf("Generated by SetDrift %s from a snapshot of `%s`", s.GetProducer().GetVersion(), cell(c.GetName()))
	var about []string
	if v := c.GetKubernetesVersion(); v != "" {
		about = append(about, "Kubernetes "+v)
	}
	if d.platDir != "" {
		about = append(about, d.cls.Summary())
	}
	if len(about) > 0 {
		p.printf(" (%s)", strings.Join(about, ", "))
	}
	if t := s.GetCapturedAt(); t != nil {
		p.printf(", taken %s", t.AsTime().UTC().Format("2006-01-02 15:04 UTC"))
	}
	p.printf(".\n\n")
	p.printf(`Follow the steps in order. Commands run from this directory. SetDrift ran
none of them; the ones marked *source cluster* are the only ones that change
it. Set these first:

`)
	vars := []string{
		fmt.Sprintf("SRC=%s   # kubeconfig context of %s", text.ShellQuote(srcContext), comment(c.GetName())),
		"DST=<target-context>   # kubeconfig context of the new cluster",
	}
	p.code(vars...)

	// The platform's own playbooks, with their own settings.
	if strings.HasPrefix(d.platDir, awsDir) {
		d.awsp = platformPlaybook(c.GetName(), "AWS", "the AWS side of the restore that applies to any cluster on AWS: "+
			"volumes copied from EBS snapshots, secrets in AWS's secret managers, and the AWS resources the manifests name",
			append(vars, "REGION="+text.ShellQuote(d.region)))
		if d.eks {
			d.eksp = platformPlaybook(c.GetName(), "EKS", "the EKS settings that are not Kubernetes objects: add-ons, access entries, "+
				"and the IAM roles service accounts use (IRSA and Pod Identity)",
				append(vars, "DST_CLUSTER=<target-eks-cluster-name>", "REGION="+text.ShellQuote(d.region)))
		}
	}

	d.prepare(p)
	d.platform(p)
	d.crds(p)
	d.controllers(p)
	// Where the platform's step goes: filled in at the end, once it is known.
	platformAt := len(p.steps)
	d.identity(p)
	if err := d.storageClasses(p, out); err != nil {
		return err
	}
	volumes, err := d.volumes(p, out)
	if err != nil {
		return err
	}
	d.apply(p, volumes)
	// After the apply, which creates the namespaces; Pods that need these
	// values wait for them.
	d.secrets(p)
	d.after(p)
	d.verify(p)
	if d.manifests.err != nil {
		return d.manifests.err
	}
	if d.awsp != nil {
		// A numbered step of its own, so the steps after it move up by one.
		var pointer playbook
		d.platformSteps(&pointer)
		p.steps = slices.Insert(p.steps, platformAt, pointer.steps...)
		if err := out.write(filepath.Join(filepath.Dir(d.platDir), restoreFile), d.awsp.done()); err != nil {
			return err
		}
		if d.eksp != nil {
			if err := out.write(filepath.Join(d.platDir, restoreFile), d.eksp.done()); err != nil {
				return err
			}
		}
	}
	return out.write(restoreFile, p.render())
}

// builtinGroups are the API groups Kubernetes itself serves.
var builtinGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "autoscaling": true, "policy": true, "networking.k8s.io": true,
	"rbac.authorization.k8s.io": true, "storage.k8s.io": true, "admissionregistration.k8s.io": true,
	"apiextensions.k8s.io": true, "scheduling.k8s.io": true, "coordination.k8s.io": true, "discovery.k8s.io": true,
	"node.k8s.io": true, "flowcontrol.apiserver.k8s.io": true, "certificates.k8s.io": true, "resource.k8s.io": true,
	"events.k8s.io": true, "apiregistration.k8s.io": true, "internal.apiserver.k8s.io": true,
	"storagemigration.k8s.io": true,
}

// missingAPIs returns, by API group, the kinds of written custom resources
// whose CustomResourceDefinition is nowhere in the snapshot, neither
// exported nor the platform's.
func missingAPIs(s *snapshotv1alpha1.Snapshot, written []writtenFile, man *manifests) map[string][]string {
	defined := map[string]bool{} // group/kind
	for _, r := range s.GetResources() {
		if isCRD(r) {
			m := man.of(r)
			defined[str(m, "spec", "group")+"/"+str(m, "spec", "names", "kind")] = true
		}
	}
	out := map[string][]string{}
	for _, w := range written {
		gk := resourceGroupKind(w.resource)
		if builtinGroups[gk.Group] || defined[gk.Group+"/"+gk.Kind] || slices.Contains(out[gk.Group], gk.Kind) {
			continue
		}
		out[gk.Group] = append(out[gk.Group], gk.Kind)
	}
	for g := range out {
		sort.Strings(out[g])
	}
	return out
}

// platformPlaybook starts one of the platform's playbooks.
func platformPlaybook(cluster, name, what string, vars []string) *playbook {
	p := &playbook{}
	p.printf("# Restoring %s: %s steps\n\n", comment(cluster), name)
	p.printf("For a new cluster on %s too: %s. RESTORE.md in the cluster's directory\n"+
		"says when to follow these; on another platform, skip them. Commands run from\n"+
		"the cluster's directory (clusters/%s), like RESTORE.md's. Set these first:\n\n", name, what, SafeName(cluster))
	p.code(vars...)
	return p
}

// done returns the playbook, or a line saying there is nothing to do.
func (p *playbook) done() []byte {
	if len(p.steps) == 0 {
		p.printf("Nothing to do here for this cluster.\n")
	}
	return p.render()
}

// platformSteps writes the step of RESTORE.md that points to the platform's playbooks.
func (d *restoreData) platformSteps(p *playbook) {
	name := "AWS"
	if d.eks {
		name = "EKS"
	}
	p.section(name + " settings")
	p.printf("The source ran on %s. **If the new cluster is %s too**, follow these now; on\n"+
		"another platform skip them: the steps here cover what Kubernetes needs, and\n"+
		"its own identity, storage and secret services take their place.\n\n", d.cls.Summary(), name)
	if d.eksp != nil && len(d.eksp.steps) > 0 {
		p.printf("- [%s/RESTORE.md](%s/RESTORE.md): %s\n", d.platDir, d.platDir, lowerList(d.eksp.titles()))
	}
	cloud := filepath.ToSlash(filepath.Dir(d.platDir))
	if len(d.awsp.steps) > 0 {
		p.printf("- [%s/RESTORE.md](%s/RESTORE.md): %s\n", cloud, cloud, lowerList(d.awsp.titles()))
	}
	p.printf("\n")
	p.printf("Then apply the %s set, `%s/`, instead of the generic one where the steps\n"+
		"below say so: it is the generic set plus the objects that need %s.\n", name, d.platDir, name)
}

// lowerList joins section titles into a sentence's list: "a, b and c",
// lower-cased except for names (EBS, IAM).
func lowerList(titles []string) string {
	out := make([]string, len(titles))
	for i, t := range titles {
		first, rest, _ := strings.Cut(t, " ")
		if strings.ToUpper(first) != first {
			first = strings.ToLower(first)
		}
		out[i] = strings.TrimSpace(first + " " + rest)
	}
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

// prepare: checks of the target cluster.
func (d *restoreData) prepare(p *playbook) {
	p.section("Check the target cluster")
	p.code(`kubectl --context "$DST" version`, `kubectl --context "$DST" get nodes`)
	if v := d.s.GetCluster().GetKubernetesVersion(); v != "" {
		p.printf("The source runs Kubernetes %s. Use the same minor version or a newer one;\n"+
			"`kubectl apply` reports resources whose API a newer version removed.\n", v)
	}
}

// platform: what the platform recreates, and AWS-side cluster settings.
func (d *restoreData) platform(p *playbook) {
	d.platformCreated(p)
	q := d.eksp
	if q == nil {
		return
	}
	q.section("Cluster settings outside Kubernetes")
	aws := d.s.GetInventory().GetAws()
	if aws == nil {
		q.printf("Add-ons and access entries are EKS settings, not Kubernetes objects. Export\n" +
			"with `--aws` to have them listed here, or list them on the source:\n\n")
		q.code(
			fmt.Sprintf(`aws eks list-addons --region "$REGION" --cluster-name %s`, text.ShellQuote(d.source)),
			fmt.Sprintf(`aws eks list-access-entries --region "$REGION" --cluster-name %s`, text.ShellQuote(d.source)),
		)
		return
	}
	if len(aws.Addons) > 0 {
		d.addonSteps(q, aws.Addons)
	}
	accessEntrySteps(q, aws.AccessEntries)
	if len(aws.Errors) > 0 {
		q.printf("Not read from AWS (the playbook may be missing these):\n\n")
		for _, e := range aws.Errors {
			q.printf("- %s\n", e)
		}
		q.printf("\n")
	}
}

// platformCreated says what the platform creates in every cluster, which
// the export leaves out.
func (d *restoreData) platformCreated(p *playbook) {
	kinds := map[string]int{}
	var by []string // who of the platform created them: EKS, kubeadm, kind
	n := 0
	for _, r := range d.s.GetResources() {
		if Platform(r) {
			kinds[r.Kind]++
			n++
			if m := PlatformName(r.GetProvenance().GetManager()); !slices.Contains(by, m) {
				by = append(by, m)
			}
		}
	}
	sort.Strings(by)
	if n > 0 {
		p.section("What the platform creates")
		var parts []string
		for _, k := range sortedMapKeys(kinds) {
			parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
		}
		verbs := "creates and manages"
		if len(by) > 1 {
			verbs = "create and manage"
		}
		p.printf("%s that %s %s in every cluster %s left out of this\n"+
			"export (%s). The new cluster gets its own; the snapshot keeps the\n"+
			"originals. Nothing to do.\n\n", text.Plural(n, "object", "objects"), strings.Join(by, " and "), verbs,
			text.Choose(n == 1, "was", "were"), strings.Join(parts, ", "))
	}
}

// addonSteps writes the commands that install the same add-on versions.
// Configuration values are read from the source add-on when the command
// runs, never written into the playbook: they can hold secrets (a vendor
// add-on's API key) that a pattern would miss, and the playbook goes into
// Git.
func (d *restoreData) addonSteps(q *playbook, addons []*snapshotv1alpha1.Addon) {
	q.printf("**Add-ons.** Install the same versions:\n\n")
	var lines []string
	configured := false
	for _, a := range addons {
		l := fmt.Sprintf(`aws eks create-addon --region "$REGION" --cluster-name "$DST_CLUSTER" --addon-name %s --addon-version %s`,
			text.ShellQuote(a.Name), text.ShellQuote(a.Version))
		if a.ServiceAccountRoleArn != "" {
			l += " --service-account-role-arn " + text.ShellQuote(a.ServiceAccountRoleArn)
		}
		if a.ConfigurationValues != "" {
			configured = true
			lines = append(lines, fmt.Sprintf(`CONFIG=$(aws eks describe-addon --region "$REGION" --cluster-name %s --addon-name %s --query addon.configurationValues --output text)   # source cluster, read-only`,
				text.ShellQuote(d.source), text.ShellQuote(a.Name)))
			l += ` --configuration-values "$CONFIG"`
		}
		lines = append(lines, l)
	}
	q.code(lines...)
	if configured {
		q.printf("An add-on's configuration values are read from the source add-on into\n" +
			"CONFIG just before its create-addon (`describe-addon`), so they never go\n" +
			"into this file: they can hold secrets. Run these while the source cluster\n" +
			"still exists.\n\n")
	}
}

// accessEntrySteps writes the commands that give the source's principals
// access to the new cluster.
func accessEntrySteps(q *playbook, all []*snapshotv1alpha1.AccessEntry) {
	var entries []*snapshotv1alpha1.AccessEntry
	for _, a := range all {
		// EKS creates the node role's entry (EC2 types) and its own
		// service-linked role's for the new cluster.
		if (a.Type == "STANDARD" || a.Type == "") && !strings.Contains(a.PrincipalArn, ":role/aws-service-role/") {
			entries = append(entries, a)
		}
	}
	if len(entries) > 0 {
		q.printf("**Access entries.** Who could reach the source cluster; give the same\n" +
			"principals access to the new one (the creator of the new cluster may\n" +
			"already have an entry):\n\n")
		var lines []string
		for _, a := range entries {
			l := fmt.Sprintf(`aws eks create-access-entry --region "$REGION" --cluster-name "$DST_CLUSTER" --principal-arn %s`, text.ShellQuote(a.PrincipalArn))
			if len(a.KubernetesGroups) > 0 {
				l += " --kubernetes-groups " + strings.Join(text.ShellQuoteAll(a.KubernetesGroups), " ")
			}
			lines = append(lines, l)
			for _, pol := range a.Policies {
				scope := "type=cluster"
				if pol.Scope == "namespace" {
					scope = "type=namespace,namespaces=" + strings.Join(pol.Namespaces, ",")
				}
				lines = append(lines, fmt.Sprintf(`aws eks associate-access-policy --region "$REGION" --cluster-name "$DST_CLUSTER" --principal-arn %s --policy-arn %s --access-scope %s`,
					text.ShellQuote(a.PrincipalArn), text.ShellQuote(pol.PolicyArn), text.ShellQuote(scope)))
			}
		}
		q.code(lines...)
	}
}

// crds: definitions to create before anything that uses them, and the APIs
// whose definitions the export does not have.
func (d *restoreData) crds(p *playbook) {
	var crds []writtenFile
	for _, w := range d.written {
		if isCRD(w.resource) {
			crds = append(crds, w)
		}
	}
	missing := missingAPIs(d.s, d.written, &d.manifests)
	if len(crds) == 0 && len(missing) == 0 {
		return
	}
	p.section("CustomResourceDefinitions")
	if len(missing) > 0 {
		p.printf("**Install these first.** The export has custom resources of APIs whose\n" +
			"definitions it does not have (they were not read, or something outside the\n" +
			"cluster's objects installs them, e.g. an operator's own install): install\n" +
			"what provides each before applying, or applying them fails.\n\n")
		p.printf("| API group | Kinds |\n|---|---|\n")
		for _, g := range sortedMapKeys(missing) {
			p.printf("| `%s` | %s |\n", cell(g), cell(strings.Join(missing[g], ", ")))
		}
		p.printf("\n")
	}
	if len(crds) == 0 {
		return
	}
	p.printf("kubectl rejects a custom resource whose definition does not exist yet, so\n" +
		"create the definitions first. Each line below creates one only if the target\n" +
		"cluster does not have it yet. Those a Helm release installs are left to it\n" +
		"(next step): Helm refuses to install a release whose CRDs exist already.\n\n")
	p.printf("| CRD | Installed by |\n|---|---|\n")
	for _, w := range crds {
		p.printf("| `%s` | %s |\n", cell(w.resource.Name), cell(manager(w.resource)))
	}
	p.printf("\n")
	var lines []string
	for _, w := range crds {
		if helmRelease(w.resource) != "" {
			continue
		}
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" get crd %s >/dev/null 2>&1 || kubectl --context "$DST" apply --server-side -f %s`,
			text.ShellQuote(w.resource.Name), text.ShellQuote(w.path)))
		// Right after the create, the CRD may have no status yet and kubectl wait fails instead of waiting: once more.
		wait := fmt.Sprintf(`kubectl --context "$DST" wait --for condition=established --timeout=120s crd/%s`, text.ShellQuote(w.resource.Name))
		lines = append(lines, wait+" || { sleep 5; "+wait+"; }")
	}
	if len(lines) > 0 {
		p.code(lines...)
	}
}

// hasFinding reports whether r has a finding with code.
func hasFinding(r *snapshotv1alpha1.Resource, code string) bool {
	for _, f := range r.GetFindings() {
		if f.GetCode() == code {
			return true
		}
	}
	return false
}

// apply: the export itself.
func (d *restoreData) apply(p *playbook, volumes bool) {
	p.section("Apply the export")
	what := "Everything that can be applied"
	if volumes {
		what += ", after the volumes above"
	}
	p.printf("%s: CRDs, cluster-scoped objects, then each namespace.\n\n", what)
	p.code(`kubectl --context "$DST" apply --server-side -k .`)
	if d.platformObjects > 0 {
		name := PlatformName(string(d.cls.Distribution))
		if d.cls.Distribution == platform.Unknown {
			name = d.cls.Infrastructure.Name()
		}
		p.printf("**If the new cluster is %s too**, apply the %s set instead: the same, plus\n"+
			"%s that only work there (custom resources of APIs %s provides):\n\n",
			name, name, text.Plural(d.platformObjects, "object", "objects"), name)
		p.code(fmt.Sprintf(`kubectl --context "$DST" apply --server-side -k %s`, d.platDir))
	}
}

// after: what differs from the source once applied.
func (d *restoreData) after(p *playbook) {
	runOnce := map[string][]string{}
	for _, w := range d.written {
		for _, f := range w.resource.Findings {
			switch f.Code {
			case "RUN_ONCE_EXPORTED", "BARE_POD":
				ref := w.resource.Kind + " " + w.resource.Namespace + "/" + w.resource.Name
				runOnce[ref] = append(runOnce[ref], f.Code)
			}
		}
	}
	var ranOnce []*snapshotv1alpha1.Resource
	for _, r := range d.s.GetResources() {
		if RanOnce(r) {
			ranOnce = append(ranOnce, r)
		}
	}
	lbs := d.s.GetInventory().GetLoadBalancers()
	if len(runOnce) == 0 && len(lbs) == 0 && len(ranOnce) == 0 {
		return
	}
	p.section("After applying")
	if len(lbs) > 0 {
		p.printf("**Load balancers.** The new cluster's load balancers get new addresses.\n" +
			"Point DNS records (unless external-dns manages them) and clients at the new\n" +
			"ones:\n\n")
		p.printf("| Object | Old address | New address |\n|---|---|---|\n")
		for _, lb := range lbs {
			addr := strings.Join(append(slices.Clone(lb.Hostnames), lb.Ips...), ", ")
			res := "svc"
			if lb.Kind == "Ingress" {
				res = "ingress"
			}
			p.printf("| %s %s/%s | `%s` | `kubectl --context \"$DST\" -n %s get %s %s -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{.status.loadBalancer.ingress[0].ip}'` |\n",
				cell(lb.Kind), cell(lb.Namespace), cell(lb.Name), cell(addr), cell(text.ShellQuote(lb.Namespace)), res, cell(text.ShellQuote(lb.Name)))
		}
		p.printf("\n")
	}
	if len(runOnce) > 0 {
		p.printf("**Run again.** These ran, or were running, in the source and start again\n" +
			"in the new cluster. Delete them from the export first if they must not\n" +
			"(e.g. a migration):\n\n")
		for _, ref := range sortedMapKeys(runOnce) {
			p.printf("- %s\n", ref)
		}
		p.printf("\n")
	}
	if len(ranOnce) > 0 {
		p.printf("**Ran once in the source.** These Jobs had finished, so the export leaves\n" +
			"them out: applying one runs it again. If the new cluster needs one (a schema\n" +
			"or a seed it has not got), run it by hand from its manifest in the snapshot\n" +
			"(`setdrift snapshot json`); the others need nothing:\n\n")
		p.printf("| Job | Result | Image | Command |\n|---|---|---|---|\n")
		for _, r := range ranOnce {
			result, image, command := jobSummary(r, d.manifests.of(r))
			p.printf("| %s/%s | %s | `%s` | `%s` |\n", cell(r.Namespace), cell(r.Name), cell(result), image, command)
		}
		p.printf("\n")
	}
}

// jobSummary is a finished Job's result (Complete or Failed), image and
// command, for RESTORE.md.
func jobSummary(r *snapshotv1alpha1.Resource, m map[string]any) (result, image, command string) {
	// the manifest has no status by now: lifecycle recorded the outcome in its finding, "finished Job (Complete): ..."
	result = "finished"
	for _, f := range r.GetFindings() {
		if f.GetCode() == lifecycle.RanOnce {
			if _, rest, ok := strings.Cut(f.GetMessage(), "("); ok {
				result, _, _ = strings.Cut(rest, ")")
			}
		}
	}
	var images, commands []string
	containers, _, _ := unstructured.NestedSlice(m, "spec", "template", "spec", "containers")
	for _, item := range containers {
		c, _ := item.(map[string]any)
		images = append(images, str(c, "image"))
		if cmd := strings.Join(append(strSlice(c, "command"), strSlice(c, "args")...), " "); cmd != "" {
			commands = append(commands, cmd)
		}
	}
	command = strings.Join(commands, "; ")
	if command == "" {
		command = "(the image's own)"
	}
	return result, strings.ReplaceAll(strings.Join(images, ", "), "|", `\|`), strings.ReplaceAll(command, "|", `\|`)
}

// verify: compare the new cluster with the export.
func (d *restoreData) verify(p *playbook) {
	p.section("Check the result")
	p.printf("`setdrift verify` dry-runs the export against the new cluster and reports\n" +
		"what differs; nothing should be left except explained differences.\n\n")
	p.code(`setdrift verify --context "$DST" .`,
		`kubectl --context "$DST" get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded`)
}

// sortedMapKeys returns m's keys, sorted.
func sortedMapKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// sortedCopy returns a sorted copy of ss.
func sortedCopy(ss []string) []string { return slices.Sorted(slices.Values(ss)) }
