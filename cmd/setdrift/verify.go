// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// checks export against a live cluster, prove 95% of resources would not change & <5% of changes have explanation
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"

	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// only flags related to verify functions
type verifyOptions struct {
	kubeconfig string
	context    string
	// set: auto, generic or platform
	set string
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

It passes when at least 95% of resources would not change and every
remaining difference has a known explanation.

A server-side apply only changes the fields it sends, so verify can't see a
field the export left out: the cluster keeps it. It flags a resource that
lost all of its spec or data; other missing fields only show when the export
is applied to a new cluster.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&o.context, "context", "", "kubeconfig context (default: current context)")
	cmd.Flags().StringVar(&o.set, "set", "auto", "what to check: generic (the cluster's directory), platform (its platform set, e.g. platform/aws/eks), "+
		"or auto: the platform set when this cluster is on the export's platform, else generic")
	return cmd
}

// loadconfig, build clients, executes check, generates result
func (o *verifyOptions) run(cmd *cobra.Command, dir string) error {
	// resolve DIR to one cluster folder, fails if no kustomization or more then one cluster
	// checked before the kubeconfig, so a wrong DIR is reported as such
	root, err := verify.ClusterRoot(dir)
	if err != nil {
		return err
	}
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err != nil {
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
	v := verify.DryRunVerifier{
		Cluster: verify.DynamicCluster{Client: dyn},
		Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}
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
	if note := otherClusterNote(cmd.Context(), dyn, root); note != "" {
		fmt.Fprintln(w, note)
	}
	report, err := v.Verify(cmd.Context(), root)
	if err != nil {
		return err
	}

	if report.Total == 0 {
		msg := "nothing to verify: the kustomization builds no resources"
		if n := skippedTotal(report); n > 0 {
			msg += fmt.Sprintf(" (%s skipped)", plural(n, "redacted file", "redacted files"))
		}
		return errors.New(msg)
	}
	fmt.Fprintf(w, "%d of %s would not change (%.1f%%; %d%% required)\n",
		report.Clean, plural(report.Total, "resource", "resources"), report.CleanPercent(), verify.AcceptancePercent)
	reasons := make([]string, 0, len(report.Skipped))
	for r := range report.Skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(w, "Skipped %d: %s\n", report.Skipped[r], r)
	}
	if n := len(report.MissingCRDs); n > 0 {
		crds := filepath.Join(root, "crds")
		fmt.Fprintf(w, "%s in the export %s not in this cluster: %s.\n"+
			"Their custom resources show below as types the cluster does not serve. Create them first:\n"+
			"    kubectl --context %s apply --server-side -k %s\n"+
			"    kubectl --context %s wait --for condition=established --timeout=60s -k %s\n",
			plural(n, "CustomResourceDefinition", "CustomResourceDefinitions"), map[bool]string{true: "is", false: "are"}[n == 1],
			strings.Join(report.MissingCRDs, ", "), kc.name, crds, kc.name, crds)
	}
	for _, d := range report.Diffs {
		explanation := "UNEXPLAINED"
		if d.Explanation != "" {
			explanation = d.Explanation
		}
		fmt.Fprintf(w, "  %s: %s\n", d.Ref, explanation)
		for _, p := range d.Paths {
			fmt.Fprintf(w, "      %s\n", p)
		}
	}
	if !report.Passed() {
		// say why on the last line, the one CI logs show first
		return fmt.Errorf("verification failed: %s", failReason(report))
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

// otherClusterNote says when the cluster verified is not the one the export came from, compared by their
// kube-system Namespace UIDs: expected after restoring the export elsewhere, a mistake otherwise (e.g. staging
// instead of prod), where a nearly clean result means nothing. Informational, never a failure; empty when they
// match or either UID is unknown.
func otherClusterNote(ctx context.Context, dyn dynamic.Interface, root string) string {
	info, err := kustomize.ReadExportInfo(root)
	if err != nil || info == nil || info.KubeSystemUID == "" {
		return ""
	}
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || string(ns.GetUID()) == info.KubeSystemUID {
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
	if r.Clean*100 < r.Total*verify.AcceptancePercent {
		why = append(why, fmt.Sprintf("%.1f%% would not change, %d%% required", r.CleanPercent(), verify.AcceptancePercent))
	}
	unexplained := 0
	for _, d := range r.Diffs {
		if d.Explanation == "" {
			unexplained++
		}
	}
	if unexplained > 0 {
		why = append(why, plural(unexplained, "unexplained difference", "unexplained differences"))
	}
	return strings.Join(why, "; ")
}

// adds up the skipped counts for the "nothing to verify" message
func skippedTotal(r *verify.Report) int {
	n := 0
	for _, c := range r.Skipped {
		n += c
	}
	return n
}
