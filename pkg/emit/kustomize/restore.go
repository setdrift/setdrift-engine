// writes the restore playbook, RESTORE.md, an ordered checklist for recreating the exported cluster in another cluster with the commands filled in from the snapshot
// each step is written only when it applies, the step builders live here and in the other restore_*.go files
// setdrift runs none of the commands, the ones for the source cluster are the only writes to it and the user runs them

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

// the names and annotations the playbook writes and looks for
const (
	// the playbook's file name, in the cluster's directory and in each platform directory
	restoreFile = "RESTORE.md"
	// the directory the playbook's helper files go to, like restore/volumes/
	restoreDir = "restore"
	// autoModeEBSDriver is EKS Auto Mode's EBS CSI driver, it only attaches volumes tagged eks:eks-cluster-name=<cluster>
	autoModeEBSDriver = "ebs.csi.eks.amazonaws.com"
	// the service account annotation that names an IRSA role
	irsaAnnotation = "eks.amazonaws.com/role-arn"
)

// playbook collects RESTORE.md, what comes before the first step and then the steps
// the steps get their numbers only when rendered, so a step can be inserted before others once its content is known
type playbook struct {
	// everything before the first step, the title, the intro and the settings
	head  bytes.Buffer
	steps []*step
}

// step is one numbered section of a playbook
type step struct {
	title string
	body  bytes.Buffer
}

// printf writes to the current step, or to the head when no step has started yet
func (p *playbook) printf(format string, args ...any) {
	w := &p.head
	// checking if a step has started, if so the text goes in the last one
	if n := len(p.steps); n > 0 {
		w = &p.steps[n-1].body
	}
	fmt.Fprintf(w, format, args...)
}

// section starts a numbered step, what printf writes after this goes in it
func (p *playbook) section(title string) {
	p.steps = append(p.steps, &step{title: title})
}

// titles gives back the steps' titles in order, for the list that points to a platform playbook
func (p *playbook) titles() []string {
	out := make([]string, len(p.steps))
	// copying out each step's title
	for i, s := range p.steps {
		out[i] = s.title
	}
	return out
}

// render gives back the playbook as markdown, with its steps numbered
func (p *playbook) render() []byte {
	var b bytes.Buffer
	b.Write(p.head.Bytes())
	// writing each step under a numbered heading
	for i, s := range p.steps {
		fmt.Fprintf(&b, "\n## %d. %s\n\n", i+1, s.title)
		b.Write(s.body.Bytes())
	}
	return b.Bytes()
}

// code writes the lines as one shell block
func (p *playbook) code(lines ...string) {
	// invoking printf to write the fenced block, its defined above
	p.printf("```sh\n%s\n```\n\n", strings.Join(lines, "\n"))
}

// restoreData is what writeRestore gathers before writing, so each step builder reads it from one place
type restoreData struct {
	s *snapshotv1alpha1.Snapshot
	// the files the export wrote, with the resource each one holds
	written []writtenFile
	// the secrets moved to an external store, for the ExternalSecrets steps
	remotes []remoteSecret
	// the aws region the commands use, the snapshot's or a placeholder
	region string
	// source is the source cluster's name for aws commands
	source string
	// tells if the source runs on EKS
	eks bool
	// helmRepos is the --helm-repos file, or nil
	helmRepos *helmsource.Repos
	// secretService is the ExternalSecrets' aws service, SecretsManager or ParameterStore
	secretService string
	// cls is the source's platform, platDir its directory in the export (platform/aws/eks), empty when its not known
	cls     *platform.Classification
	platDir string
	// platformObjects is how many objects only the platform set has
	platformObjects int
	// manifests are the resources' manifests, decoded once
	manifests manifests
	// held are the cloud-resource operator objects export held back in cloud-resources/
	held []heldResource
	// the platform's playbooks, for a new cluster on the same platform, RESTORE.md itself has only the steps that work on any cluster
	// awsp is platform/aws/RESTORE.md (aws-wide, volumes, secrets, ids), eksp is platform/aws/eks/RESTORE.md (EKS settings and identity)
	// theyre nil when the source isnt on aws, or isnt on EKS
	awsp, eksp *playbook
}

// classification gives back the snapshot's platform
// a snapshot from before the classifier, or a test's, may only say "eks", so that is turned into a classification here
func classification(s *snapshotv1alpha1.Snapshot) *platform.Classification {
	c := s.GetCluster()
	// checking if the snapshot carries a classification, if so thats the answer
	if c.GetClassification() != nil {
		// invoking platform.FromProto to read it back from the snapshot, its defined in pkg/platform/probe.go
		return platform.FromProto(c.GetClassification())
	}
	// checking if the snapshot only says eks, or has aws inventory, if so its classified as EKS on aws
	if c.GetPlatform() == string(platform.EKS) || s.GetInventory().GetAws() != nil {
		// invoking platform.Decide with one strong signal, its defined in pkg/platform/platform.go
		return platform.Decide([]platform.Signal{{Source: "snapshot", Detail: "platform eks",
			Distribution: platform.EKS, Infrastructure: platform.AWS, Weight: platform.Strong}})
	}
	return &platform.Classification{}
}

// writeRestore takes the snapshot and what the export wrote, and writes RESTORE.md, the platform playbooks and restore/volumes/
func (e Emitter) writeRestore(out *outDir, s *snapshotv1alpha1.Snapshot, written []writtenFile, remotes []remoteSecret, platformObjects int, held []heldResource) error {
	d := &restoreData{s: s, written: written, remotes: remotes, helmRepos: e.HelmRepos, platformObjects: platformObjects, held: held}
	// checking if secrets go to an external store, if so the playbook needs to know which aws service holds them
	if e.ExternalSecrets != nil {
		// invoking resolved to fill in the store settings for this platform, its defined in externalsecrets.go
		d.secretService = e.ExternalSecrets.resolved(classification(s)).Service
	}
	// invoking regionOf and checking if the snapshot knows its region, if not the --region flag or a placeholder is used, its defined in externalsecrets.go
	if r, ok := regionOf(s); ok {
		d.region = r
	} else {
		d.region = cmp.Or(e.Region, regionPlaceholder)
	}
	d.source = s.GetInventory().GetAws().GetClusterName()
	// checking if aws gave no cluster name, if so the snapshot's cluster name stands in
	if d.source == "" {
		d.source = s.GetCluster().GetName()
	}
	// invoking exportPlatform to get the source's platform and its directory in the export, its defined in platformset.go
	d.cls, d.platDir = exportPlatform(s)
	d.eks = d.cls.Is(platform.EKS) || s.GetInventory().GetAws() != nil

	srcContext := e.SourceContext
	// checking if no source context was given, if so the playbook shows a placeholder to fill in
	if srcContext == "" {
		srcContext = "<source-context>"
	}
	p := &playbook{}
	c := s.GetCluster()
	// writing the title and the line saying where the playbook came from, comment and cell are defined in manifest.go
	p.printf("# Restoring %s to another cluster\n\n", comment(c.GetName()))
	p.printf("Generated by SetDrift %s from a snapshot of `%s`", s.GetProducer().GetVersion(), cell(c.GetName()))
	var about []string
	// checking if the kubernetes version is known, if so its named after the cluster
	if v := c.GetKubernetesVersion(); v != "" {
		about = append(about, "Kubernetes "+v)
	}
	// checking if the platform is known, if so its summary is named too
	if d.platDir != "" {
		about = append(about, d.cls.Summary())
	}
	// checking if theres anything to say about the cluster
	if len(about) > 0 {
		p.printf(" (%s)", strings.Join(about, ", "))
	}
	// checking if the snapshot recorded when it was taken
	if t := s.GetCapturedAt(); t != nil {
		p.printf(", taken %s", t.AsTime().UTC().Format("2006-01-02 15:04 UTC"))
	}
	p.printf(".\n\n")
	p.printf(`Follow the steps in order. Commands run from this directory. SetDrift ran
none of them; the ones marked *source cluster* are the only ones that change
it. Set these first:

`)
	// the shell variables every command reads, text.ShellQuote is defined in internal/text/text.go
	vars := []string{
		fmt.Sprintf("SRC=%s   # kubeconfig context of %s", text.ShellQuote(srcContext), comment(c.GetName())),
		"DST=<target-context>   # kubeconfig context of the new cluster",
	}
	p.code(vars...)

	// checking if the source is on aws, if so the platform gets its own playbooks with their own settings
	if strings.HasPrefix(d.platDir, awsDir) {
		// invoking platformPlaybook to start the aws one, its defined below
		d.awsp = platformPlaybook(c.GetName(), "AWS", "the AWS side of the restore that applies to any cluster on AWS: "+
			"volumes copied from EBS snapshots, secrets in AWS's secret managers, and the AWS resources the manifests name",
			append(vars, "REGION="+text.ShellQuote(d.region)))
		// checking if its EKS too, if so theres an EKS playbook as well
		if d.eks {
			d.eksp = platformPlaybook(c.GetName(), "EKS", "the EKS settings that are not Kubernetes objects: add-ons, access entries, "+
				"and the IAM roles service accounts use (IRSA and Pod Identity)",
				append(vars, "DST_CLUSTER=<target-eks-cluster-name>", "REGION="+text.ShellQuote(d.region)))
		}
	}

	// invoking each step builder in playbook order, prepare, platform and crds are defined below, controllers in restore_controllers.go
	d.prepare(p)
	d.platform(p)
	d.crds(p)
	d.controllers(p)
	// remembering where the platform's step goes, its filled in at the end once its known
	platformAt := len(p.steps)
	// invoking identity for the service account roles, its defined in restore_identity.go
	d.identity(p)
	// invoking storageClasses for the storage classes the claims need, its defined in restore_volumes.go
	if err := d.storageClasses(p, out); err != nil {
		return err
	}
	// invoking volumes for the data in persistent volumes, its defined in restore_volumes.go
	volumes, err := d.volumes(p, out)
	if err != nil {
		return err
	}
	// invoking apply for the export itself, its defined below
	d.apply(p, volumes)
	// invoking cloudResources to move the held-back objects one controller at a time, its defined in restore_cloudresources.go
	d.cloudResources(p)
	// invoking secrets after the apply, which creates the namespaces, pods that need these values wait for them, its defined in restore_secrets.go
	d.secrets(p)
	// invoking after and verify for what changes once applied and how to check it, theyre defined below
	d.after(p)
	d.verify(p)
	// checking if a manifest didnt parse along the way, if so thats a terminal error, the snapshot wasnt one an export wrote
	if d.manifests.err != nil {
		return d.manifests.err
	}
	// checking if there are platform playbooks, if so RESTORE.md points to them and theyre written
	if d.awsp != nil {
		// invoking platformSteps to write the pointer as a numbered step of its own, so the steps after it move up by one, its defined below
		var pointer playbook
		d.platformSteps(&pointer)
		p.steps = slices.Insert(p.steps, platformAt, pointer.steps...)
		// invoking out.write to write the aws playbook in the cloud's directory, platform/aws, its defined in kustomize.go, done is defined below
		if err := out.write(filepath.Join(filepath.Dir(d.platDir), restoreFile), d.awsp.done()); err != nil {
			return err
		}
		// checking if theres an EKS playbook too, if so it goes in the EKS directory
		if d.eksp != nil {
			// invoking out.write to write the EKS playbook, its defined in kustomize.go
			if err := out.write(filepath.Join(d.platDir, restoreFile), d.eksp.done()); err != nil {
				return err
			}
		}
	}
	// invoking render to number the steps and writing RESTORE.md, its defined above
	return out.write(restoreFile, p.render())
}

// builtinGroups are the api groups kubernetes itself serves, their kinds never need a CRD
var builtinGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "autoscaling": true, "policy": true, "networking.k8s.io": true,
	"rbac.authorization.k8s.io": true, "storage.k8s.io": true, "admissionregistration.k8s.io": true,
	"apiextensions.k8s.io": true, "scheduling.k8s.io": true, "coordination.k8s.io": true, "discovery.k8s.io": true,
	"node.k8s.io": true, "flowcontrol.apiserver.k8s.io": true, "certificates.k8s.io": true, "resource.k8s.io": true,
	"events.k8s.io": true, "apiregistration.k8s.io": true, "internal.apiserver.k8s.io": true,
	"storagemigration.k8s.io": true,
}

// missingAPIs gives back, by api group, the kinds of written custom resources whose CustomResourceDefinition is nowhere in the snapshot
// thats neither exported nor the platform's, so the user has to install it before applying
func missingAPIs(s *snapshotv1alpha1.Snapshot, written []writtenFile, man *manifests) map[string][]string {
	defined := map[string]bool{} // keyed by group/kind
	// collecting the group and kind each CRD in the snapshot defines
	for _, r := range s.GetResources() {
		// invoking isCRD and checking if the resource is a CRD, its defined in kustomize.go
		if isCRD(r) {
			// invoking of for the decoded manifest and str to read the group and kind, theyre defined in manifest.go
			m := man.of(r)
			defined[str(m, "spec", "group")+"/"+str(m, "spec", "names", "kind")] = true
		}
	}
	out := map[string][]string{}
	// going through what was written for kinds no CRD defines
	for _, w := range written {
		// invoking resourceGroupKind for the resource's api group and kind, its defined in platformset.go
		gk := resourceGroupKind(w.resource)
		// checking if the group is built in, a CRD defines the kind, or the kind is listed already, if so its skipped
		if builtinGroups[gk.Group] || defined[gk.Group+"/"+gk.Kind] || slices.Contains(out[gk.Group], gk.Kind) {
			continue
		}
		out[gk.Group] = append(out[gk.Group], gk.Kind)
	}
	// sorting each group's kinds so the table comes out the same every time
	for g := range out {
		sort.Strings(out[g])
	}
	return out
}

// platformPlaybook starts one of the platform's playbooks, with its title, when to follow it and its shell variables
func platformPlaybook(cluster, name, what string, vars []string) *playbook {
	p := &playbook{}
	// writing the title and intro, comment is defined in manifest.go and SafeName in kustomize.go
	p.printf("# Restoring %s: %s steps\n\n", comment(cluster), name)
	p.printf("For a new cluster on %s too: %s. RESTORE.md in the cluster's directory\n"+
		"says when to follow these; on another platform, skip them. Commands run from\n"+
		"the cluster's directory (clusters/%s), like RESTORE.md's. Set these first:\n\n", name, what, SafeName(cluster))
	p.code(vars...)
	return p
}

// done gives back the rendered playbook, with a line saying theres nothing to do when it has no steps
func (p *playbook) done() []byte {
	// checking if no step was written, if so the playbook says so instead of ending after its intro
	if len(p.steps) == 0 {
		p.printf("Nothing to do here for this cluster.\n")
	}
	return p.render()
}

// platformSteps writes the step of RESTORE.md that points to the platform's playbooks and says when to follow them
func (d *restoreData) platformSteps(p *playbook) {
	name := "AWS"
	// checking if the source is on EKS, if so the step is named for EKS
	if d.eks {
		name = "EKS"
	}
	p.section(name + " settings")
	p.printf("The source ran on %s. **If the new cluster is %s too**, follow these now; on\n"+
		"another platform skip them: the steps here cover what Kubernetes needs, and\n"+
		"its own identity, storage and secret services take their place. For EKS, AKS or\n"+
		"GKE, `setdrift translate --to <platform>` writes this export for it, with its own\n"+
		"RESTORE.md to follow instead of this one.\n\n", d.cls.Summary(), name)
	// checking if the EKS playbook has steps, if so its linked with their titles, lowerList is defined below
	if d.eksp != nil && len(d.eksp.steps) > 0 {
		p.printf("- [%s/RESTORE.md](%s/RESTORE.md): %s\n", d.platDir, d.platDir, lowerList(d.eksp.titles()))
	}
	cloud := filepath.ToSlash(filepath.Dir(d.platDir))
	// checking if the aws playbook has steps, if so its linked the same way
	if len(d.awsp.steps) > 0 {
		p.printf("- [%s/RESTORE.md](%s/RESTORE.md): %s\n", cloud, cloud, lowerList(d.awsp.titles()))
	}
	p.printf("\n")
	p.printf("Then apply the %s set, `%s/`, instead of the generic one where the steps\n"+
		"below say so: it is the generic set plus the objects that need %s.\n", name, d.platDir, name)
}

// lowerList joins step titles into a list for a sentence, "a, b and c", lowercased except for names like EBS and IAM
func lowerList(titles []string) string {
	out := make([]string, len(titles))
	// lowercasing each title's first word
	for i, t := range titles {
		first, rest, _ := strings.Cut(t, " ")
		// checking if the first word isnt all caps, if so its not a name and gets lowercased
		if strings.ToUpper(first) != first {
			first = strings.ToLower(first)
		}
		out[i] = strings.TrimSpace(first + " " + rest)
	}
	// checking if theres only one title, if so theres nothing to join
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

// prepare writes the step that checks the target cluster answers and runs a fitting kubernetes version
func (d *restoreData) prepare(p *playbook) {
	p.section("Check the target cluster")
	p.code(`kubectl --context "$DST" version`, `kubectl --context "$DST" get nodes`)
	// checking if the source's kubernetes version is known, if so the step asks for the same minor version or newer
	if v := d.s.GetCluster().GetKubernetesVersion(); v != "" {
		p.printf("The source runs Kubernetes %s. Use the same minor version or a newer one;\n"+
			"`kubectl apply` reports resources whose API a newer version removed.\n", v)
	}
}

// platform writes what the platform recreates on its own, and the EKS cluster settings that arent kubernetes objects
func (d *restoreData) platform(p *playbook) {
	// invoking platformCreated for the objects the platform creates in every cluster, its defined below
	d.platformCreated(p)
	q := d.eksp
	// checking if theres no EKS playbook, if so theres no EKS setting to write
	if q == nil {
		return
	}
	q.section("Cluster settings outside Kubernetes")
	aws := d.s.GetInventory().GetAws()
	// checking if the export was taken without --aws, if so the step says how to list the settings on the source instead
	if aws == nil {
		q.printf("Add-ons and access entries are EKS settings, not Kubernetes objects. Export\n" +
			"with `--aws` to have them listed here, or list them on the source:\n\n")
		q.code(
			fmt.Sprintf(`aws eks list-addons --region "$REGION" --cluster-name %s`, text.ShellQuote(d.source)),
			fmt.Sprintf(`aws eks list-access-entries --region "$REGION" --cluster-name %s`, text.ShellQuote(d.source)),
		)
		return
	}
	// checking if the source had add-ons, if so addonSteps writes their install commands, its defined below
	if len(aws.Addons) > 0 {
		d.addonSteps(q, aws.Addons)
	}
	// invoking accessEntrySteps to give the same principals access, its defined below
	accessEntrySteps(q, aws.AccessEntries)
	// checking if some aws reads failed, if so the step lists them since the playbook may be missing those
	if len(aws.Errors) > 0 {
		q.printf("Not read from AWS (the playbook may be missing these):\n\n")
		// listing each failed read
		for _, e := range aws.Errors {
			q.printf("- %s\n", e)
		}
		q.printf("\n")
	}
}

// platformCreated says what the platform creates in every cluster, which the export leaves out, so the user knows theres nothing to do for it
func (d *restoreData) platformCreated(p *playbook) {
	kinds := map[string]int{}
	var by []string // who of the platform created them, EKS, kubeadm, kind
	n := 0
	// counting the platform's objects by kind and collecting who created them
	for _, r := range d.s.GetResources() {
		// invoking Platform and checking if the platform created this object, its defined in kustomize.go
		if Platform(r) {
			kinds[r.Kind]++
			n++
			// invoking PlatformName for the creator's name and checking if its new to the list, its defined in kustomize.go
			if m := PlatformName(r.GetProvenance().GetManager()); !slices.Contains(by, m) {
				by = append(by, m)
			}
		}
	}
	sort.Strings(by)
	// checking if the platform created anything, if so it gets a step saying so
	if n > 0 {
		p.section("What the platform creates")
		var parts []string
		// listing the count of each kind, sortedMapKeys is defined below
		for _, k := range sortedMapKeys(kinds) {
			parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
		}
		verbs := "creates and manages"
		// checking if more than one creator is named, if so the verbs are plural
		if len(by) > 1 {
			verbs = "create and manage"
		}
		// text.Plural and text.Choose are defined in internal/text/text.go
		p.printf("%s that %s %s in every cluster %s left out of this\n"+
			"export (%s). The new cluster gets its own; the snapshot keeps the\n"+
			"originals. Nothing to do.\n\n", text.Plural(n, "object", "objects"), strings.Join(by, " and "), verbs,
			text.Choose(n == 1, "was", "were"), strings.Join(parts, ", "))
	}
}

// addonSteps writes the commands that install the same add-on versions on the new cluster
// configuration values are read from the source add-on when the command runs, never written into the playbook
// they can hold secrets, like a vendor add-on's api key, that a pattern would miss, and the playbook goes into git
func (d *restoreData) addonSteps(q *playbook, addons []*snapshotv1alpha1.Addon) {
	q.printf("**Add-ons.** Install the same versions:\n\n")
	var lines []string
	configured := false
	// building one create-addon command per add-on
	for _, a := range addons {
		l := fmt.Sprintf(`aws eks create-addon --region "$REGION" --cluster-name "$DST_CLUSTER" --addon-name %s --addon-version %s`,
			text.ShellQuote(a.Name), text.ShellQuote(a.Version))
		// checking if the add-on used an IAM role, if so the command passes the same one
		if a.ServiceAccountRoleArn != "" {
			l += " --service-account-role-arn " + text.ShellQuote(a.ServiceAccountRoleArn)
		}
		// checking if the add-on had configuration values, if so a line before it reads them from the source into CONFIG
		if a.HasConfiguration {
			configured = true
			lines = append(lines, fmt.Sprintf(`CONFIG=$(aws eks describe-addon --region "$REGION" --cluster-name %s --addon-name %s --query addon.configurationValues --output text)   # source cluster, read-only`,
				text.ShellQuote(d.source), text.ShellQuote(a.Name)))
			l += ` --configuration-values "$CONFIG"`
		}
		lines = append(lines, l)
	}
	q.code(lines...)
	// checking if any add-on reads its configuration from the source, if so the step explains why and says to run it while the source exists
	if configured {
		q.printf("An add-on's configuration values are read from the source add-on into\n" +
			"CONFIG just before its create-addon (`describe-addon`), so they never go\n" +
			"into this file: they can hold secrets. Run these while the source cluster\n" +
			"still exists.\n\n")
	}
}

// accessEntrySteps writes the commands that give the source's principals access to the new cluster
func accessEntrySteps(q *playbook, all []*snapshotv1alpha1.AccessEntry) {
	var entries []*snapshotv1alpha1.AccessEntry
	// picking out the entries the new cluster wont get on its own
	for _, a := range all {
		// checking if its a standard entry and not a service-linked role
		// EKS creates the node role's entry (EC2 types) and its own service-linked role's for the new cluster
		if (a.Type == "STANDARD" || a.Type == "") && !strings.Contains(a.PrincipalArn, ":role/aws-service-role/") {
			entries = append(entries, a)
		}
	}
	// checking if any entries are left, if so they get their commands
	if len(entries) > 0 {
		q.printf("**Access entries.** Who could reach the source cluster; give the same\n" +
			"principals access to the new one (the creator of the new cluster may\n" +
			"already have an entry):\n\n")
		var lines []string
		// building the create command for each entry, and one associate command per policy
		for _, a := range entries {
			l := fmt.Sprintf(`aws eks create-access-entry --region "$REGION" --cluster-name "$DST_CLUSTER" --principal-arn %s`, text.ShellQuote(a.PrincipalArn))
			// checking if the entry put the principal in kubernetes groups, if so the command does too, text.ShellQuoteAll is defined in internal/text/text.go
			if len(a.KubernetesGroups) > 0 {
				l += " --kubernetes-groups " + strings.Join(text.ShellQuoteAll(a.KubernetesGroups), " ")
			}
			lines = append(lines, l)
			// going through the entry's access policies
			for _, pol := range a.Policies {
				scope := "type=cluster"
				// checking if the policy was scoped to namespaces, if so the scope names them
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

// crds writes the step for the definitions to create before anything that uses them, and the apis whose definitions the export doesnt have
func (d *restoreData) crds(p *playbook) {
	var crds []writtenFile
	// picking out the written CRDs, isCRD is defined in kustomize.go
	for _, w := range d.written {
		// checking if the written resource is a CRD
		if isCRD(w.resource) {
			crds = append(crds, w)
		}
	}
	// invoking missingAPIs for the custom resources nothing in the snapshot defines, its defined above
	missing := missingAPIs(d.s, d.written, &d.manifests)
	// checking if there are neither CRDs nor missing apis, if so theres no step
	if len(crds) == 0 && len(missing) == 0 {
		return
	}
	p.section("CustomResourceDefinitions")
	// checking if some apis are missing, if so the step lists them to install first
	if len(missing) > 0 {
		p.printf("**Install these first.** The export has custom resources of APIs whose\n" +
			"definitions it does not have (they were not read, or something outside the\n" +
			"cluster's objects installs them, e.g. an operator's own install): install\n" +
			"what provides each before applying, or applying them fails.\n\n")
		p.printf("| API group | Kinds |\n|---|---|\n")
		// writing a table row per api group
		for _, g := range sortedMapKeys(missing) {
			p.printf("| `%s` | %s |\n", cell(g), cell(strings.Join(missing[g], ", ")))
		}
		p.printf("\n")
	}
	// checking if the export has no CRDs of its own, if so the missing ones were all there is to say
	if len(crds) == 0 {
		return
	}
	p.printf("kubectl rejects a custom resource whose definition does not exist yet, so\n" +
		"create the definitions first. Each line below creates one only if the target\n" +
		"cluster does not have it yet. Those a Helm release installs are left to it\n" +
		"(next step): Helm refuses to install a release whose CRDs exist already.\n\n")
	p.printf("| CRD | Installed by |\n|---|---|\n")
	// writing a table row per CRD with who installed it, manager is defined in restore_controllers.go
	for _, w := range crds {
		p.printf("| `%s` | %s |\n", cell(w.resource.Name), cell(manager(w.resource)))
	}
	p.printf("\n")
	var lines []string
	// building the create and wait commands for each CRD
	for _, w := range crds {
		// invoking helmRelease and checking if a helm release installs this CRD, if so its left to helm, its defined in restore_controllers.go
		if helmRelease(w.resource) != "" {
			continue
		}
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" get crd %s >/dev/null 2>&1 || kubectl --context "$DST" apply --server-side -f %s`,
			text.ShellQuote(w.resource.Name), text.ShellQuote(w.path)))
		// right after the create the CRD may have no status yet and kubectl wait fails instead of waiting, so it tries once more
		wait := fmt.Sprintf(`kubectl --context "$DST" wait --for condition=established --timeout=120s crd/%s`, text.ShellQuote(w.resource.Name))
		lines = append(lines, wait+" || { sleep 5; "+wait+"; }")
	}
	// checking if any CRD is left to create here, if so the commands get a shell block
	if len(lines) > 0 {
		p.code(lines...)
	}
}

// hasFinding tells if r has a finding with the given code
func hasFinding(r *snapshotv1alpha1.Resource, code string) bool {
	// looking through r's findings for the code
	for _, f := range r.GetFindings() {
		// checking if this finding has the code
		if f.GetCode() == code {
			return true
		}
	}
	return false
}

// apply writes the step that applies the export itself, and the platform set instead when the new cluster is on the same platform
func (d *restoreData) apply(p *playbook, volumes bool) {
	p.section("Apply the export")
	what := "Everything that can be applied"
	// checking if volumes were restored first, if so the step says it comes after them
	if volumes {
		what += ", after the volumes above"
	}
	p.printf("%s: CRDs, cluster-scoped objects, then each namespace.\n\n", what)
	p.code(`kubectl --context "$DST" apply --server-side -k .`)
	// checking if the platform set has objects of its own, if so the step says to apply it on the same platform
	if d.platformObjects > 0 {
		// invoking PlatformName for the distribution's name, its defined in kustomize.go
		name := PlatformName(string(d.cls.Distribution))
		// checking if the distribution is unknown, if so the infrastructure's name is used instead
		if d.cls.Distribution == platform.Unknown {
			name = d.cls.Infrastructure.Name()
		}
		p.printf("**If the new cluster is %s too**, apply the %s set instead: the same, plus\n"+
			"%s that only work there (custom resources of APIs %s provides):\n\n",
			name, name, text.Plural(d.platformObjects, "object", "objects"), name)
		p.code(fmt.Sprintf(`kubectl --context "$DST" apply --server-side -k %s`, d.platDir))
	}
}

// after writes what differs from the source once applied, new load balancer addresses, things that run again and jobs that ran once
func (d *restoreData) after(p *playbook) {
	runOnce := map[string][]string{}
	// collecting the written resources that run again once applied
	for _, w := range d.written {
		// going through each resource's findings
		for _, f := range w.resource.Findings {
			// checking if the finding says it ran once or is a bare pod
			switch f.Code {
			case "RUN_ONCE_EXPORTED", "BARE_POD":
				ref := w.resource.Kind + " " + w.resource.Namespace + "/" + w.resource.Name
				runOnce[ref] = append(runOnce[ref], f.Code)
			}
		}
	}
	var ranOnce []*snapshotv1alpha1.Resource
	// collecting the finished jobs the export left out
	for _, r := range d.s.GetResources() {
		// invoking RanOnce and checking if its a finished job the export left out, its defined in kustomize.go
		if RanOnce(r) {
			ranOnce = append(ranOnce, r)
		}
	}
	lbs := d.s.GetInventory().GetLoadBalancers()
	// checking if theres nothing at all to say, if so theres no step
	if len(runOnce) == 0 && len(lbs) == 0 && len(ranOnce) == 0 {
		return
	}
	p.section("After applying")
	// checking if the source had load balancers, if so the step tells how to find their new addresses
	if len(lbs) > 0 {
		p.printf("**Load balancers.** The new cluster's load balancers get new addresses.\n" +
			"Point DNS records (unless external-dns manages them) and clients at the new\n" +
			"ones:\n\n")
		p.printf("| Object | Old address | New address |\n|---|---|---|\n")
		// writing a row per load balancer with its old address and the command for the new one
		for _, lb := range lbs {
			addr := strings.Join(append(slices.Clone(lb.Hostnames), lb.Ips...), ", ")
			res := "svc"
			// checking if its an ingress, if so kubectl gets it as one, otherwise as a service
			if lb.Kind == "Ingress" {
				res = "ingress"
			}
			p.printf("| %s %s/%s | `%s` | `kubectl --context \"$DST\" -n %s get %s %s -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{.status.loadBalancer.ingress[0].ip}'` |\n",
				cell(lb.Kind), cell(lb.Namespace), cell(lb.Name), cell(addr), cell(text.ShellQuote(lb.Namespace)), res, cell(text.ShellQuote(lb.Name)))
		}
		p.printf("\n")
	}
	// checking if anything runs again, if so the step lists it
	if len(runOnce) > 0 {
		p.printf("**Run again.** These ran, or were running, in the source and start again\n" +
			"in the new cluster. Delete them from the export first if they must not\n" +
			"(e.g. a migration):\n\n")
		// listing each one by kind and name
		for _, ref := range sortedMapKeys(runOnce) {
			p.printf("- %s\n", ref)
		}
		p.printf("\n")
	}
	// checking if any jobs ran once and were left out, if so the step lists them with what they ran
	if len(ranOnce) > 0 {
		p.printf("**Ran once in the source.** These Jobs had finished, so the export leaves\n" +
			"them out: applying one runs it again. If the new cluster needs one (a schema\n" +
			"or a seed it has not got), run it by hand from its manifest in the snapshot\n" +
			"(`setdrift snapshot json`); the others need nothing:\n\n")
		p.printf("| Job | Result | Image | Command |\n|---|---|---|---|\n")
		// writing a row per job, jobSummary is defined below and of in manifest.go
		for _, r := range ranOnce {
			result, image, command := jobSummary(r, d.manifests.of(r))
			p.printf("| %s/%s | %s | `%s` | `%s` |\n", cell(r.Namespace), cell(r.Name), cell(result), image, command)
		}
		p.printf("\n")
	}
}

// jobSummary takes a finished job and its manifest and gives back its result (Complete or Failed), image and command, for RESTORE.md's table
func jobSummary(r *snapshotv1alpha1.Resource, m map[string]any) (result, image, command string) {
	// the manifest has no status by now, lifecycle recorded the outcome in its finding, like "finished Job (Complete)"
	result = "finished"
	// looking for the ran-once finding
	for _, f := range r.GetFindings() {
		// checking if its the ran-once finding, lifecycle.RanOnce is defined in pkg/lifecycle/lifecycle.go
		if f.GetCode() == lifecycle.RanOnce {
			// checking if the message has the result in parentheses, if so thats the result
			if _, rest, ok := strings.Cut(f.GetMessage(), "("); ok {
				result, _, _ = strings.Cut(rest, ")")
			}
		}
	}
	var images, commands []string
	containers, _, _ := unstructured.NestedSlice(m, "spec", "template", "spec", "containers")
	// collecting each container's image and command, str and strSlice are defined in manifest.go
	for _, item := range containers {
		c, _ := item.(map[string]any)
		images = append(images, str(c, "image"))
		// checking if the container sets a command or args, if so they make its command
		if cmd := strings.Join(append(strSlice(c, "command"), strSlice(c, "args")...), " "); cmd != "" {
			commands = append(commands, cmd)
		}
	}
	command = strings.Join(commands, "; ")
	// checking if no container sets a command, if so the table says the image's own runs
	if command == "" {
		command = "(the image's own)"
	}
	// escaping the pipes, theyd break the markdown table
	return result, strings.ReplaceAll(strings.Join(images, ", "), "|", `\|`), strings.ReplaceAll(command, "|", `\|`)
}

// verify writes the step that compares the new cluster with the export, with setdrift verify and a look for pods that arent running
func (d *restoreData) verify(p *playbook) {
	p.section("Check the result")
	p.printf("`setdrift verify` dry-runs the export against the new cluster and reports\n" +
		"what differs; nothing should be left except explained differences.\n\n")
	p.code(`setdrift verify --context "$DST" .`,
		`kubectl --context "$DST" get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded`)
}

// sortedMapKeys gives back m's keys, sorted, so the playbook comes out the same every time
func sortedMapKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// sortedCopy gives back a sorted copy of ss, leaving ss as it was
func sortedCopy(ss []string) []string { return slices.Sorted(slices.Values(ss)) }

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
