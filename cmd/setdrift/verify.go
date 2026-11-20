// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// checks export against a live cluster, prove 95% of resources would not change & <5% of changes have explanation
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// only flags related to verify functions
type verifyOptions struct {
	kubeconfig string
	context    string
	// set: auto, generic or platform
	set string
	// reports: the directory the report is written to (default DIR/verify), or none
	output   string
	noReport bool
	// percent of resources that must not change
	threshold int
	// limit the check to these namespaces, as export -n
	namespaces []string
}

// server side apply with dryRun=All comparing DIR to live object
func newVerifyCmd() *cobra.Command {
	o := &verifyOptions{}
	cmd := &cobra.Command{
		Use:   "verify DIR",
		Short: "Check an exported repository against the cluster with a server-side dry-run",
		Long: `Verify server-side applies every resource in an exported repository with
dryRun=All, and compares the result with the live object. Nothing in the
cluster changes.

DIR is a cluster's directory (clusters/<cluster>) or an export holding one
cluster. What its kustomization.yaml builds is checked, as kubectl kustomize
builds it. Secrets under redacted/ are skipped: they hold placeholders and are
never applied. external-secrets/ is not applied, so not checked.

It passes when at least 95% of resources (--threshold) would not change
and every remaining difference has a known explanation. The differences are
grouped by cause, with the cluster's and the export's values, who changed
the resource after the export was taken, and the likely cause overall.

Each run writes a report, in Markdown and JSON, to DIR/verify/ (-o for
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
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&o.context, "context", "", "kubeconfig context (default: current context)")
	cmd.Flags().StringVar(&o.set, "set", "auto", "what to check: generic (the cluster's directory), platform (its platform set, e.g. platform/aws/eks), "+
		"or auto: the platform set when this cluster is on the export's platform, else generic")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "directory to write the report to (default: DIR/verify)")
	cmd.Flags().BoolVar(&o.noReport, "no-report", false, "write no report, only print the result")
	cmd.Flags().IntVar(&o.threshold, "threshold", verify.AcceptancePercent, "percent of resources that must not change to pass, 0 to 100")
	cmd.Flags().StringSliceVarP(&o.namespaces, "namespace", "n", nil, "check only the export's resources in these namespaces, and their Namespace objects; skips cluster-scoped resources (repeatable)")
	return cmd
}

// loadconfig, build clients, executes check, generates result
func (o *verifyOptions) run(cmd *cobra.Command, dir string) error {
	// resolve DIR to one cluster folder, fails if no kustomization or more then one cluster
	// checked before the kubeconfig, so a wrong DIR is reported as such
	if o.threshold < 0 || o.threshold > 100 {
		return fmt.Errorf("--threshold %d: give a percent from 0 to 100", o.threshold)
	}
	if o.noReport && o.output != "" {
		return errors.New("-o names where to write the report, so it cannot be used with --no-report")
	}
	root, err := verify.ClusterRoot(dir)
	if err != nil {
		return err
	}
	info, err := kustomize.ReadExportInfo(root)
	if err != nil {
		return err
	}
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err != nil {
		return err
	}
	if err := checkReachable(cmd.Context(), kc); err != nil {
		return err
	}
	cfg := kc.config
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	// namespaces asked for must be readable, as for export and diff; they
	// need not exist: verify reports their resources as not in the cluster
	if len(o.namespaces) > 0 {
		if err := collect.CheckNamespaces(cmd.Context(), dyn, o.namespaces, false, false); err != nil {
			return err
		}
	}
	v := verify.DryRunVerifier{
		Cluster:    verify.DynamicCluster{Client: dyn},
		Mapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
		Threshold:  o.threshold,
		Namespaces: o.namespaces,
	}
	if info != nil {
		if t, err := time.Parse(time.RFC3339, info.ExportedAt); err == nil {
			v.ExportedAt = t
		}
	}
	started := time.Now().UTC()
	set, why, err := o.chooseSet(cmd.Context(), root, kc, dc, dyn)
	if err != nil {
		return err
	}
	v.Set = set
	w := cmd.OutOrStdout()
	target := root
	if set != "" {
		target = filepath.Join(root, set)
	}
	fmt.Fprintf(w, "Verifying %s against context %s (%s)\n", target, kc.name, why)
	same := sameCluster(cmd.Context(), dyn, info)
	if note := otherClusterNote(info, same); note != "" {
		fmt.Fprintln(w, note)
	}
	report, err := v.Verify(cmd.Context(), root)
	if err != nil {
		return err
	}

	identity := ""
	if len(report.NotChecked) > 0 {
		identity = collect.Identity(cmd.Context(), dyn)
	}
	if report.Total == 0 && len(report.NotChecked) > 0 {
		return notCheckedError(report, identity)
	}
	if report.Total == 0 {
		msg := "nothing to verify: the kustomization builds no resources"
		if len(o.namespaces) > 0 {
			msg = "nothing to verify: the export has no resources in " + strings.Join(o.namespaces, ", ")
		}
		if n := skippedTotal(report); n > 0 {
			msg += fmt.Sprintf(" (%s skipped)", plural(n, "redacted file", "redacted files"))
		}
		return errors.New(msg)
	}
	cluster := &snapshotv1alpha1.Cluster{}
	applyContext(cluster, kc)
	doc := verifyDocument{
		VerifiedAt: started.Format(time.RFC3339), Context: kc.name, Cluster: cluster.Name,
		Directory: target, Set: why, SameCluster: same, Report: *report,
		Passed: report.Passed(), Analysis: report.Analyze(), Identity: identity,
	}
	if info != nil {
		doc.ExportCluster, doc.ExportedAt = info.Cluster, info.ExportedAt
	}
	if !doc.Passed {
		doc.FailReason = failReason(report)
	}
	doc.LikelyCause = likelyCause(doc)

	printVerify(w, doc, root, kc.name)
	if !o.noReport {
		out := o.output
		if out == "" {
			out = filepath.Join(dir, "verify")
		}
		md, err := writeVerifyReport(out, doc, started)
		if err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
		fmt.Fprintf(w, "\nReport: %s (and .json)\n", md)
	}
	if !doc.Passed {
		// say why on the last line, the one CI logs show first
		return fmt.Errorf("verification failed: %s", doc.FailReason)
	}
	fmt.Fprintln(w, "Passed.")
	return nil
}

var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// chooseSet decides which set to check, and says why: with auto, the
// platform set when the target is classified as the export's platform (the
// same directory, platform/aws/eks), else the generic set, which applies to
// any cluster.
func (o *verifyOptions) chooseSet(ctx context.Context, root string, kc *kubeContext, dc discovery.DiscoveryInterface, dyn dynamic.Interface) (string, string, error) {
	info, err := kustomize.ReadExportInfo(root)
	if err != nil {
		return "", "", err
	}
	platDir := ""
	if info != nil {
		platDir = info.PlatformDir
	}
	switch o.set {
	case "generic":
		return "", "the generic set, as --set generic asks", nil
	case "platform":
		if platDir == "" {
			return "", "", errors.New("--set platform: this export has no platform set (its platform was not identified)")
		}
		return platDir, "the platform set, as --set platform asks", nil
	case "auto":
	default:
		return "", "", fmt.Errorf("--set must be auto, generic or platform, not %q", o.set)
	}
	if platDir == "" {
		return "", "the generic set; the export has no platform set", nil
	}
	signals := platform.FromKubeconfig(kc.platform)
	probed, _ := platform.Probe(ctx, dc, dyn)
	cls := platform.Decide(append(signals, probed...))
	if cls.Dir() == platDir {
		return platDir, "the " + platDir + " set: this cluster is " + cls.Summary(), nil
	}
	return "", "the generic set: the export's platform set is " + platDir + ", and this cluster is " + cls.Summary(), nil
}

// sameCluster reports whether the cluster verified is the one the export
// came from, by their kube-system Namespace UIDs; nil when either is unknown.
func sameCluster(ctx context.Context, dyn dynamic.Interface, info *kustomize.ExportInfo) *bool {
	if info == nil || info.KubeSystemUID == "" {
		return nil
	}
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	same := string(ns.GetUID()) == info.KubeSystemUID
	return &same
}

// otherClusterNote says when the cluster verified is not the one the export came from: expected after restoring
// the export elsewhere, a mistake otherwise (e.g. staging instead of prod), where a nearly clean result means
// nothing. Informational, never a failure; empty when they match or either is unknown.
func otherClusterNote(info *kustomize.ExportInfo, same *bool) string {
	if same == nil || *same {
		return ""
	}
	from := info.Cluster
	if info.ProviderID != "" {
		from += " (" + info.ProviderID + ")"
	}
	if info.ExportedAt != "" {
		from += ", exported " + info.ExportedAt
	}
	return "Note: this export came from " + from + ", and this is a different cluster.\n" +
		"That is expected after restoring it here; otherwise check --context."
}

// failReason says which part of the acceptance test a run missed, the share of resources that would not change, unexplained differences, or both.
func failReason(r *verify.Report) string {
	var why []string
	if !r.MeetsThreshold() {
		why = append(why, fmt.Sprintf("%.1f%% would not change, %d%% required", r.CleanPercent(), r.RequiredPercent()))
	}
	if n := len(r.NotChecked); n > 0 {
		why = append(why, plural(n, "resource", "resources")+" not checked (no permission)")
	}
	if unexplained := r.Unexplained(); unexplained > 0 {
		why = append(why, plural(unexplained, "unexplained difference", "unexplained differences"))
	}
	return strings.Join(why, "; ")
}

// notCheckedError is a run where the credentials could check nothing: they
// may not get the resources, or may not patch them for the dry-run.
func notCheckedError(r *verify.Report, identity string) error {
	who := "These credentials"
	if identity != "" {
		who += " (" + identity + ")"
	}
	return fmt.Errorf("%s could check none of the %s: %s", who, plural(len(r.NotChecked), "resource", "resources"), refusals(r.NotChecked, identity))
}

// refusals says why resources could not be checked, by what was refused.
func refusals(notChecked []verify.Diff, identity string) string {
	patch, get := 0, 0
	for _, d := range notChecked {
		if len(d.Paths) > 0 && strings.Contains(d.Paths[0], "may not patch") {
			patch++
		} else {
			get++
		}
	}
	var parts []string
	if patch > 0 {
		parts = append(parts, fmt.Sprintf("%s may be read but not patched, and verify's dry-run apply needs patch "+
			"(dryRun=All: the cluster changes nothing); the built-in edit role is enough, e.g. "+
			"kubectl create rolebinding setdrift-verify -n <namespace> --clusterrole=edit %s", plural(patch, "resource", "resources"), collect.Subject(identity)))
	}
	if get > 0 {
		parts = append(parts, fmt.Sprintf("%s may not be read at all (verify needs get; the view role leaves out RBAC objects, "+
			"and namespace-scoped access leaves out cluster-scoped resources): check fewer with -n <namespace>, or use credentials with access",
			plural(get, "resource", "resources")))
	}
	return strings.Join(parts, "; ")
}

// adds up the skipped counts for the "nothing to verify" message
func skippedTotal(r *verify.Report) int {
	n := 0
	for _, c := range r.Skipped {
		n += c
	}
	return n
}
