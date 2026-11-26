// checks an export against a live cluster with a server-side dry-run, nothing in the cluster changes
// the dry-run and the comparison live in pkg/verify, this file handles the flags, the connection and the report

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/internal/discover"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/netpolicy"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// verifyOptions holds the flags verify was given, so run and the helpers below read them from one place
type verifyOptions struct {
	kubeconfig string
	context    string
	// which part of the export to check, auto, generic or platform
	set string
	// the directory the report goes to, <path-to-export>/verify when its empty
	output string
	// skips writing the report, the result is only printed
	noReport bool
	// the percent of resources that must not change for verify to pass
	threshold int
	// limits the check to these namespaces, the same way export -n does
	namespaces []string
	// lets the run pass when the source enforced the export's NetworkPolicies and this cluster doesnt, for an emergency
	acceptUnenforced bool
}

// newVerifyCmd builds the verify command with its help text and flags, main.go adds it to the command tree
func newVerifyCmd() *cobra.Command {
	o := &verifyOptions{}
	cmd := &cobra.Command{
		Use:   "verify <path-to-export>",
		Short: "Check an exported repository against the cluster with a server-side dry-run",
		Long: `Verify server-side applies every resource in an exported repository with
dryRun=All, and compares the result with the live object. Nothing in the
cluster changes.

<path-to-export> is an export holding one cluster, or a cluster's directory
in it (clusters/<cluster>). What its kustomization.yaml builds is checked, as
kubectl kustomize builds it. Secrets under redacted/ are skipped: they hold placeholders and are
never applied. external-secrets/ is not applied, so not checked.

It passes when at least 95% of resources (--threshold) would not change
and every remaining difference has a known explanation. The differences are
grouped by cause, with the cluster's and the export's values, who changed
the resource after the export was taken, and the likely cause overall.

Each run writes a report, in Markdown and JSON, to <path-to-export>/verify/ (-o for
another directory, --no-report for none), named for when it ran and the
cluster: verify/2026-11-20T191402Z-prod-report.md.

A server-side apply only changes the fields it sends, so verify can't see a
field the export left out: the cluster keeps it. It flags a resource that
lost all of its spec or data; other missing fields only show when the export
is applied to a new cluster.`,
		Example: `  setdrift verify setdrift-export
  setdrift verify setdrift-export --context restored-prod   # after restoring it elsewhere
  setdrift verify setdrift-export -o ./verify-reports --threshold 100`,
		Args: cobra.ExactArgs(1),
		// invoking run with the export directory, its defined below
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	// registering the flags, each one fills a field of verifyOptions above
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&o.context, "context", "", "kubeconfig context (default: current context)")
	cmd.Flags().StringVar(&o.set, "set", "auto", "what to check: generic (the cluster's directory), platform (its platform set, e.g. platform/aws/eks), "+
		"or auto: the platform set when this cluster is on the export's platform, else generic")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "directory to write the report to (default: <path-to-export>/verify)")
	cmd.Flags().BoolVar(&o.noReport, "no-report", false, "write no report, only print the result")
	// invoking cannotUseWith to declare that -o and --no-report contradict each other, checkFlags enforces it, its defined in flagrules.go
	cannotUseWith(cmd.Flags(), "names where to write the report", "output", "no-report")
	cmd.Flags().IntVar(&o.threshold, "threshold", verify.AcceptancePercent, "percent of resources that must not change to pass, 0 to 100")
	cmd.Flags().StringSliceVarP(&o.namespaces, "namespace", "n", nil, "check only the export's resources in these namespaces, and their Namespace objects; skips cluster-scoped resources (repeatable)")
	cmd.Flags().BoolVar(&o.acceptUnenforced, "accept-unenforced-network-policies", false,
		"pass even though the source enforced the export's NetworkPolicies and this cluster doesn't (or verify can't tell), for an emergency; said loudly in the result")
	// invoking acceptSingular so the singular is the same flag, like --to gcp for gke, its defined in verifynetpol.go
	cmd.Flags().SetNormalizeFunc(acceptSingular)
	return cmd
}

// connect loads the kubeconfig and checks the cluster answers, then gives back the discovery and dynamic clients verify needs
func (o *verifyOptions) connect(cmd *cobra.Command) (*kubeContext, *discover.Client, dynamic.Interface, error) {
	// invoking loadConfig to read the kubeconfig for the chosen context, its defined in kubeconfig.go
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err != nil {
		return nil, nil, nil, err
	}
	// invoking warnInsecure to warn when the context skips the certificate check, its defined in kubeconfig.go
	warnInsecure(cmd.ErrOrStderr(), kc)
	// invoking checkReachable to make one quick request, so an unreachable cluster or expired credentials fail right away, its defined in kubeconfig.go
	if err := checkReachable(cmd.Context(), kc); err != nil {
		return nil, nil, nil, err
	}
	// invoking discover.New for the client that lists the cluster's API types, its defined in internal/discover/discover.go
	dc, err := discover.New(kc.config)
	if err != nil {
		return nil, nil, nil, err
	}
	// building the dynamic client that reads and dry-run applies objects of any kind
	dyn, err := dynamic.NewForConfig(kc.config)
	if err != nil {
		return nil, nil, nil, err
	}
	return kc, dc, dyn, nil
}

// run takes the export directory, checks it against the cluster, prints the result and writes the report
// the directory is checked before the kubeconfig, so a wrong directory is reported as that and not as a cluster problem
//
//revive:disable-next-line:cognitive-complexity,function-length the command in order, from flags to report; split by step after v0.3.0 (BACKLOG)
func (o *verifyOptions) run(cmd *cobra.Command, dir string) error {
	// checking if the threshold is outside 0 to 100, if so the flag was given wrong
	if o.threshold < 0 || o.threshold > 100 {
		return fmt.Errorf("--threshold %d: give a percent from 0 to 100", o.threshold)
	}
	// invoking folderKind to tell a translation or a platform set from an export, its defined in folders.go
	// either is checked as asked, from its export, a translation with its decisions
	given := folderKind(dir)
	switch given.kind {
	case folderTranslation:
		dir = given.root
	case folderPlatformSet:
		if o.set == "generic" {
			return fmt.Errorf("%s is the export's platform set (%s), and --set generic asks for the generic set: give the export, %s, with --set generic", dir, given.rel, given.root)
		}
		dir, o.set = given.root, "platform"
	}
	// invoking verify.ClusterRoot to find the cluster's directory inside dir, its defined in pkg/verify/dryrun.go
	root, err := verify.ClusterRoot(dir)
	if err != nil {
		return err
	}
	// invoking kustomize.ReadExportInfo to read which cluster the export came from, its defined in pkg/emit/kustomize/exportinfo.go
	info, err := kustomize.ReadExportInfo(root)
	if err != nil {
		return err
	}
	// invoking connect to reach the cluster, its defined above
	kc, dc, dyn, err := o.connect(cmd)
	if err != nil {
		return err
	}
	// checking if namespaces were asked for, if so each must be readable like in export and diff
	// they dont have to exist, verify reports their resources as missing from the cluster
	if len(o.namespaces) > 0 {
		// invoking collect.CheckNamespaces to confirm they can be read, its defined in pkg/collect/access.go
		if err := collect.CheckNamespaces(cmd.Context(), dyn, o.namespaces, false, false); err != nil {
			return err
		}
	}
	// invoking discover.RESTMapper to map each kind in the export to its API resource, its defined in internal/discover/preferred.go
	mapper, err := discover.RESTMapper(cmd.Context(), dc)
	if err != nil {
		return err
	}
	// setting up the dry-run verifier with the cluster, the mapper and the flags
	v := verify.DryRunVerifier{
		Cluster:    verify.DynamicCluster{Client: dyn},
		Mapper:     mapper,
		Threshold:  o.threshold,
		Namespaces: o.namespaces,
	}
	// checking if the export recorded when it was taken, if so the verifier uses it to tell changes made since apart
	if info != nil {
		// checking if that time parses, an export without a readable time leaves it out
		if t, err := time.Parse(time.RFC3339, info.ExportedAt); err == nil {
			v.ExportedAt = t
		}
	}
	started := time.Now().UTC()
	// classifying this cluster once, for the set to check and for the capability check
	// invoking platform.FromKubeconfig to read the hints the kubeconfig gives, its defined in pkg/platform/signals.go
	signals := platform.FromKubeconfig(kc.platform)
	// invoking platform.Probe to ask the cluster itself, its defined in pkg/platform/probe.go
	// the error is dropped: Probe's second result is notes for the user, not an error, verify words its own
	probed, _ := platform.Probe(cmd.Context(), dc, dyn)
	// invoking platform.Decide to weigh both into one classification, its defined in pkg/platform/platform.go
	cls := platform.Decide(append(signals, probed...))
	// checking if the classification is confident, if so the capability check holds the export to this cluster's platform
	// a guess could flag a user's objects as not working here, so with weak evidence nothing is checked
	if cls.Confidence == platform.HighConfidence || cls.Confidence == platform.MediumConfidence {
		// invoking capability.Load to read the capability map built into the binary, its defined in pkg/capability/capability.go
		capMap, err := capability.Load()
		if err != nil {
			return err
		}
		v.Target, v.Capability = cls.Distribution, capMap
	}

	// the decisions of the translation checked, if its one, for the network policy check
	var decisions []translate.Decision
	// invoking chooseSet to pick the part of the export to check and say why, its defined below
	set, why, err := o.chooseSet(info, cls)
	// checking if a translation was given, if so its the set to check, with its decisions, whatever this cluster is
	if given.kind == folderTranslation && err == nil {
		set, why = given.rel, given.describe()+", as given"
		ds, derr := translate.ReadDecisions(filepath.Join(root, filepath.FromSlash(given.rel)))
		if derr != nil {
			return derr
		}
		v.Explain, v.Kept, decisions = translate.Explainer(ds), translate.IdentityKept(ds), ds
	}
	// checking if auto picked no platform set, if so a translation for this cluster's platform is the one to check
	if err == nil && o.set == "auto" && set == "" && given.kind != folderTranslation {
		// invoking translationFor to find the export's translations and the one for this cluster, its defined below
		tdir, tstamp, others := translationFor(root, cls)
		switch {
		case tdir != "":
			set, why = tdir, fmt.Sprintf("the %s translation (from %s): this cluster is %s", tstamp.To, tstamp.From, cls.Summary())
			// invoking translate.ReadDecisions to read the choices made for it, its defined in pkg/translate/write.go
			ds, derr := translate.ReadDecisions(filepath.Join(root, filepath.FromSlash(tdir)))
			if derr != nil {
				return derr
			}
			v.Explain, v.Kept = translate.Explainer(ds), translate.IdentityKept(ds)
			decisions = ds
		case len(others) > 0:
			why += fmt.Sprintf(". This export also has a translation for %s, and this cluster is %s: run setdrift translate --to %s, or verify against a %s cluster", strings.Join(others, " and "), cls.Summary(), cls.Distribution, strings.Join(others, " or "))
			// checking if no context was given, if so the current one may be the wrong cluster, so its said plainly
			if o.context == "" {
				why += fmt.Sprintf(". This is your current context: did you mean --context <your %s context>?", strings.Join(others, " or "))
			}
		}
	}
	if err != nil {
		return err
	}
	v.Set = set
	w := cmd.OutOrStdout()
	target := root
	// checking if a platform set was chosen, if so its directory inside the export is what gets checked
	if set != "" {
		target = filepath.Join(root, set)
	}
	which := "context"
	// checking if no context was given, if so its said that the current one was used
	if o.context == "" {
		which = "your current context"
	}
	fmt.Fprintf(w, "Verifying %s against %s %s (%s)\n", target, which, kc.name, why)
	// checking if its a translation older than the export, if so its decisions may be about objects the export has changed since
	if strings.HasPrefix(set, capability.TranslationsDir+"/") {
		// invoking staleTranslation to compare the two times, its defined in folders.go
		if note := staleTranslation(root, set, info); note != "" {
			fmt.Fprintln(w, note)
		}
	}
	// invoking sameCluster to tell if this is the cluster the export came from, its defined below
	same := sameCluster(cmd.Context(), dyn, info)
	// invoking otherClusterNote and checking if it has something to say, if so it gets printed before the result
	if note := otherClusterNote(info, same); note != "" {
		fmt.Fprintln(w, note)
	}
	// checking if this isnt the export's own cluster, if so the export was restored or translated here, and verify checks this cluster runs what its objects name
	// on the export's own cluster an unused class was already that way, so it isnt checked there
	if same == nil || !*same {
		// invoking served to read this cluster's CSI drivers and IngressClasses, its defined in verifynetpol.go
		v.Served = served(cmd.Context(), dyn)
		// the check needs the map to know which drivers are a cloud's, even on a cluster classified with little confidence
		if v.Capability == nil {
			if capMap, err := capability.Load(); err == nil {
				v.Capability = capMap
			}
		}
	}
	// invoking Verify to dry-run every resource and compare it with the live object, its defined in pkg/verify/dryrun.go
	report, err := v.Verify(cmd.Context(), root)
	if err != nil {
		return err
	}

	identity := ""
	// checking if some resources couldnt be checked, if so we look up who the credentials belong to so the error can name them
	if len(report.NotChecked) > 0 {
		// invoking collect.Identity to ask the cluster who we are, its defined in pkg/collect/access.go
		identity = collect.Identity(cmd.Context(), dyn)
	}
	// checking if nothing at all could be checked, if so thats a terminal error explained by notCheckedError, its defined below
	if report.Total == 0 && len(report.NotChecked) > 0 {
		return notCheckedError(report, identity)
	}
	// checking if the export built no resources to check, if so thats a terminal error saying why
	if report.Total == 0 {
		msg := "nothing to verify: the kustomization builds no resources"
		// checking if namespaces were asked for, if so the message names them since they hold nothing
		if len(o.namespaces) > 0 {
			msg = "nothing to verify: the export has no resources in " + strings.Join(o.namespaces, ", ")
		}
		// invoking skippedTotal and checking if any redacted files were skipped, if so the message says how many, its defined below
		if n := skippedTotal(report); n > 0 {
			msg += fmt.Sprintf(" (%s skipped)", text.Plural(n, "redacted file", "redacted files"))
		}
		return errors.New(msg)
	}
	cluster := &snapshotv1alpha1.Cluster{}
	// invoking applyContext to fill in the cluster's name from the kubeconfig context, its defined in kubeconfig.go
	applyContext(cluster, kc)
	// putting together everything the printed result and the report show, verifyDocument is defined in verifyreport.go
	doc := verifyDocument{
		VerifiedAt: started.Format(time.RFC3339), Context: kc.name, Cluster: cluster.Name,
		Directory: target, Set: why, SameCluster: same, Report: *report,
		Passed: report.Passed(), Analysis: report.Analyze(), Identity: identity,
	}
	// checking if the export recorded its source, if so the report names it
	if info != nil {
		doc.ExportCluster, doc.ExportedAt = info.Cluster, info.ExportedAt
	}
	// checking if the run failed, if so failReason says which part of the test it missed, its defined below
	if !doc.Passed {
		doc.FailReason = failReason(report)
	}
	// invoking networkPolicies to check this cluster enforces what the source did, its defined below
	if doc.NetworkPolicy, err = o.networkPolicies(cmd, dyn, info, root, target, cls, decisions); err != nil {
		return err
	}
	// checking if the network policy check failed, if so the run fails with it
	if doc.NetworkPolicy != nil && doc.NetworkPolicy.Failed {
		doc.Passed = false
		doc.FailReason = strings.TrimPrefix(doc.FailReason+"; NetworkPolicies the source enforced aren't enforced here", "; ")
	}
	// invoking likelyCause to sum up why things differ, its defined in verifyreport.go
	doc.LikelyCause = likelyCause(doc)

	// invoking printVerify to print the result, its defined in verifyreport.go
	printVerify(w, doc, root, kc.name)
	// invoking olderApplyNote to say when the cluster was applied from another run of the translation, its defined in folders.go
	if note := olderApplyNote(report); note != "" {
		fmt.Fprintln(w, note)
	}
	// invoking printNetworkPolicy to say what the network policy check found, its defined in verifynetpol.go
	printNetworkPolicy(w, doc.NetworkPolicy)
	// checking if a report was wanted, if so its written next to the export or where -o says
	if !o.noReport {
		out, ignore := o.output, false
		// checking if -o was left out, if so the report goes to <path-to-export>/verify, which git is told to ignore
		if out == "" {
			out, ignore = filepath.Join(dir, "verify"), true
		}
		// invoking writeVerifyReport to write the markdown and json report, its defined in verifyreport.go
		md, err := writeVerifyReport(out, doc, started, ignore)
		if err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
		fmt.Fprintf(w, "\nReport: %s (and .json)\n", md)
	}
	// checking if the run failed, if so the reason goes on the last line, the one ci logs show first
	if !doc.Passed {
		return fmt.Errorf("verification failed: %s", doc.FailReason)
	}
	fmt.Fprintln(w, "Passed.")
	return nil
}

// networkPolicies runs the network policy check, nil when theres nothing to say: no NetworkPolicies, or a source that didnt enforce them
func (o *verifyOptions) networkPolicies(cmd *cobra.Command, dyn dynamic.Interface, info *kustomize.ExportInfo, root, target string, cls *platform.Classification, decisions []translate.Decision) (*networkPolicyCheck, error) {
	source := netpolicy.Unknown
	if info != nil {
		source = info.NetworkPolicy
	}
	// invoking verify.Build to build what was checked, to count its NetworkPolicies, its defined in pkg/verify/dryrun.go
	objs, err := verify.Build(root, target)
	if err != nil {
		return nil, err
	}
	// invoking countNetworkPolicies, its defined in verifynetpol.go
	n := countNetworkPolicies(objs, o.namespaces)
	accepted := ""
	switch {
	case o.acceptUnenforced:
		accepted = "--accept-unenforced-network-policies"
	case translate.NetworkPolicyChoice(decisions) == translate.AcceptUnenforced:
		accepted = "accept-unenforced in " + translate.DecisionsFile
	}
	// checking if theres nothing the check could fail on, if so the flag, when given, has nothing to accept
	if n == 0 || source == netpolicy.None {
		if o.acceptUnenforced {
			fmt.Fprintln(cmd.ErrOrStderr(), "--accept-unenforced-network-policies: nothing to accept, the export has no NetworkPolicies the source enforced")
		}
		return nil, nil
	}
	// invoking targetNetworkPolicy to read this cluster's engine, its defined in verifynetpol.go
	tgt, why, idle := targetNetworkPolicy(cmd.Context(), dyn)
	return checkNetworkPolicies(n, source, tgt, why, idle, accepted, string(cls.Distribution)), nil
}

// namespacesGVR is how the dynamic client finds namespaces, sameCluster reads kube-system through it
var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// chooseSet takes the --set flag and the cluster and decides which part of the export to check, and says why
// with auto its the platform set when this cluster is classified as the export's platform, like platform/aws/eks, otherwise the generic set that fits any cluster
func (o *verifyOptions) chooseSet(info *kustomize.ExportInfo, cls *platform.Classification) (set, why string, err error) {
	platDir := ""
	// checking if the export recorded its platform set, if so thats the one auto and platform can pick
	if info != nil {
		platDir = info.PlatformDir
	}
	// checking which --set was given, generic and platform are taken as asked, auto carries on below
	switch o.set {
	case "generic":
		return "", "the generic set, as --set generic asks", nil
	case "platform":
		// checking if the export has no platform set, if so --set platform cant be honored
		if platDir == "" {
			return "", "", errors.New("--set platform: this export has no platform set (its platform was not identified)")
		}
		return platDir, "the platform set, as --set platform asks", nil
	case "auto":
	default:
		return "", "", fmt.Errorf("--set must be auto, generic or platform, not %q", o.set)
	}
	// checking if the export has no platform set, if so auto falls back to the generic set
	if platDir == "" {
		return "", "the generic set; the export has no platform set", nil
	}
	// checking if this cluster is on the export's platform, if so its platform set is the one to check
	if cls.Dir() == platDir {
		return platDir, "the " + platDir + " set: this cluster is " + cls.Summary(), nil
	}
	return "", "the generic set: the export's platform set is " + platDir + ", and this cluster is " + cls.Summary(), nil
}

// translationFor finds the translations written into the export, under translations/<code>, the one for this cluster's platform when theres one, with its stamp, and the platforms of the others
func translationFor(root string, cls *platform.Classification) (dir string, stamp *translate.Stamp, others []string) {
	m, err := capability.Load()
	if err != nil {
		return "", nil, nil
	}
	// the error is dropped: no translations directory, or one that can't be read, means no translation to name here, the other commands report it
	entries, _ := os.ReadDir(filepath.Join(root, capability.TranslationsDir))
	for _, e := range entries {
		d := capability.TranslationsDir + "/" + e.Name()
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(d), translate.StampFile)) //#nosec G304 -- inside the export directory the user named
		if err != nil {
			continue
		}
		var st translate.Stamp
		if yaml.Unmarshal(b, &st) != nil {
			continue
		}
		to := platform.Distribution(st.To)
		// checking if its for this cluster's platform, classified with confidence, if so its the set to check
		if cls != nil && cls.Is(to) && cls.Distribution == to {
			dir, stamp = d, &st
			continue
		}
		if p := m.For(to); p != nil {
			others = append(others, p.Name)
		}
	}
	return dir, stamp, others
}

// sameCluster tells if the cluster being verified is the one the export came from, by comparing their kube-system namespace uids
// it gives back nil when either uid is unknown, since then theres no way to tell
func sameCluster(ctx context.Context, dyn dynamic.Interface, info *kustomize.ExportInfo) *bool {
	// checking if the export recorded its kube-system uid, if not theres nothing to compare
	if info == nil || info.KubeSystemUID == "" {
		return nil
	}
	// reading this cluster's kube-system namespace for its uid
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	// checking if it couldnt be read, if so the answer is unknown rather than an error
	if err != nil {
		return nil
	}
	same := string(ns.GetUID()) == info.KubeSystemUID
	return &same
}

// otherClusterNote takes what sameCluster found and gives back a note when this isnt the export's cluster, empty otherwise
// thats expected after restoring the export somewhere else, and a mistake otherwise, like checking staging instead of prod
// its only ever informational, never a failure
func otherClusterNote(info *kustomize.ExportInfo, same *bool) string {
	// checking if its the same cluster or unknown, if so theres nothing to say
	if same == nil || *same {
		return ""
	}
	from := info.Cluster
	// checking if the export recorded the cluster's provider id, if so the note includes it
	if info.ProviderID != "" {
		from += " (" + info.ProviderID + ")"
	}
	// checking if the export recorded when it was taken, if so the note includes that too
	if info.ExportedAt != "" {
		from += ", exported " + info.ExportedAt
	}
	return "Note: this export came from " + from + ", and this is a different cluster.\n" +
		"That is expected after restoring it here; otherwise check --context."
}

// failReason takes a failed report and says which part of the test it missed
// that can be too few resources unchanged, resources that couldnt be checked, unexplained differences, or several of those
func failReason(r *verify.Report) string {
	var why []string
	// checking if too many resources would change, if so the percent goes in the reason
	if !r.MeetsThreshold() {
		why = append(why, fmt.Sprintf("%.1f%% would not change, %d%% required", r.CleanPercent(), r.RequiredPercent()))
	}
	// checking if some resources couldnt be checked for lack of permission
	if n := len(r.NotChecked); n > 0 {
		why = append(why, text.Plural(n, "resource", "resources")+" not checked (no permission)")
	}
	// checking if any difference has no known explanation
	if unexplained := r.Unexplained(); unexplained > 0 {
		why = append(why, text.Plural(unexplained, "unexplained difference", "unexplained differences"))
	}
	return strings.Join(why, "; ")
}

// notCheckedError builds the error for a run where the credentials could check nothing, they cant read the resources or cant patch them for the dry-run
func notCheckedError(r *verify.Report, identity string) error {
	who := "These credentials"
	// checking if we know who the credentials belong to, if so the error names them
	if identity != "" {
		who += " (" + identity + ")"
	}
	// invoking refusals to say what was refused and how to fix it, its defined below
	return fmt.Errorf("%s could check none of the %s: %s", who, text.Plural(len(r.NotChecked), "resource", "resources"), refusals(r.NotChecked, identity))
}

// refusals takes the resources that couldnt be checked and says why, grouped by whether patch or get was refused, with the fix for each
func refusals(notChecked []verify.Diff, identity string) string {
	patch, get := 0, 0
	// counting how many were refused patch and how many were refused get
	for _, d := range notChecked {
		// checking if it was patch that was refused, otherwise it was get
		if d.Refused == verify.RefusedPatch {
			patch++
		} else {
			get++
		}
	}
	var parts []string
	// checking if any were refused patch, if so the fix is a role that allows it, like the built-in edit role
	if patch > 0 {
		parts = append(parts, fmt.Sprintf("%s may be read but not patched, and verify's dry-run apply needs patch "+
			"(dryRun=All: the cluster changes nothing); the built-in edit role is enough, e.g. "+
			"kubectl create rolebinding setdrift-verify -n <namespace> --clusterrole=edit %s", text.Plural(patch, "resource", "resources"), collect.Subject(identity)))
	}
	// checking if any were refused get, if so the fix is fewer namespaces or credentials with more access
	if get > 0 {
		parts = append(parts, fmt.Sprintf("%s may not be read at all (verify needs get; the view role leaves out RBAC objects, "+
			"and namespace-scoped access leaves out cluster-scoped resources): check fewer with -n <namespace>, or use credentials with access",
			text.Plural(get, "resource", "resources")))
	}
	return strings.Join(parts, "; ")
}

// skippedTotal adds up how many files were skipped, for the nothing to verify message
func skippedTotal(r *verify.Report) int {
	n := 0
	// adding up each kind of skip, like redacted files
	for _, c := range r.Skipped {
		n += c
	}
	return n
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
