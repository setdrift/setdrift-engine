// What setdrift export prints when it is done: what was read and written,
// what the export found, and what RESTORE.md asks.

package main

import (
	"cmp"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// maxSuggestions caps the cleanup suggestions printed; the snapshot has all.
const maxSuggestions = 10

// platformCreators names who of the platform created the objects left out,
// e.g. "kind and kubeadm".
func platformCreators(s *snapshotv1alpha1.Snapshot) string {
	var by []string
	for _, r := range s.Resources {
		if kustomize.Platform(r) {
			if n := kustomize.PlatformName(r.GetProvenance().GetManager()); !slices.Contains(by, n) {
				by = append(by, n)
			}
		}
	}
	sort.Strings(by)
	return strings.Join(by, " and ")
}

// exportCounts are what an export's summary counts.
type exportCounts struct {
	bySource map[snapshotv1alpha1.Source]int
	// findings and redactions in the snapshot
	findings, redactions int
	// written: resources written; referenceOnly: of them, written to
	// redacted/; redactedSecrets: Secrets with values to supply, which get an
	// ExternalSecret
	written, referenceOnly, redactedSecrets int
	// needsValues: resources someone must supply values for; regenerated:
	// those whose only values a controller writes again
	needsValues, regenerated int
}

// countExport counts what the summary reports; with unmanagedOnly, only the
// unmanaged resources were written.
func countExport(s *snapshotv1alpha1.Snapshot, unmanagedOnly bool) exportCounts {
	c := exportCounts{bySource: map[snapshotv1alpha1.Source]int{}, findings: len(s.Findings)}
	for _, r := range s.Resources {
		c.bySource[r.GetProvenance().GetSource()]++
		c.findings += len(r.Findings)
		c.redactions += len(r.Redactions)
		if kustomize.NotWritten(r) || (unmanagedOnly && !kustomize.Unmanaged(r)) {
			continue
		}
		c.written++
		if kustomize.NeedsValues(r) {
			c.needsValues++
		} else if kustomize.ReferenceOnly(r) {
			c.regenerated++
		}
		if kustomize.ReferenceOnly(r) {
			c.referenceOnly++
			// only Secrets with values to supply get an ExternalSecret: a controller recreates the others
			if r.Kind == "Secret" && r.ApiVersion == "v1" && kustomize.NeedsValues(r) {
				c.redactedSecrets++
			}
		}
	}
	return c
}

// printSummary prints what export did: what the credentials limited the read
// to, what was read by who manages it, what the export found, and where to
// go next.
func printSummary(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, unmanagedOnly, externalSecrets bool) {
	c := countExport(s, unmanagedOnly)
	printAccess(w, s)
	printRead(w, s, out, c, unmanagedOnly)
	ov := kustomize.NewOverview(s, unmanagedOnly)
	printFound(w, ov)
	fmt.Fprintln(w)
	printRedacted(w, c, externalSecrets)
	printNext(w, s, out, c, ov)
}

// printRead prints what was exported, the platform, and the resources by
// who manages them.
func printRead(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, c exportCounts, unmanagedOnly bool) {
	fmt.Fprintf(w, "Exported %s from %s to %s\n", text.Plural(len(s.Resources), "resource", "resources"), s.Cluster.GetName(), out)
	if c := s.Cluster.GetClassification(); c != nil {
		cls := platform.FromProto(c)
		if dir := cls.Dir(); dir != "" {
			fmt.Fprintf(w, "Platform: %s. The export is generic (any cluster); %s/ is the set for the same platform.\n", cls.Summary(), dir)
		} else if cls.Distribution != platform.Unknown || cls.Infrastructure != platform.UnknownInfra {
			fmt.Fprintf(w, "Platform: %s, not certain enough for platform-specific output; the export is generic.\n", cls.Summary())
		}
	}
	for src := snapshotv1alpha1.Source_SOURCE_UNMANAGED; src <= snapshotv1alpha1.Source_SOURCE_CI_CD; src++ {
		if n := c.bySource[src]; n > 0 {
			note := ""
			switch src {
			case snapshotv1alpha1.Source_SOURCE_PLATFORM:
				note = "  (created by " + platformCreators(s) + "; in the snapshot, not written)"
			case snapshotv1alpha1.Source_SOURCE_CI_CD:
				note = "  (deployed by a pipeline: setdrift.io/source-* annotations or --ci-field-manager)"
			}
			name := strings.ToLower(strings.TrimPrefix(src.String(), "SOURCE_"))
			if src == snapshotv1alpha1.Source_SOURCE_CI_CD {
				name = "ci/cd"
			}
			fmt.Fprintf(w, "  %-10s %d%s\n", name, n, note)
		}
	}
	if unmanagedOnly {
		fmt.Fprintf(w, "Wrote the %s nothing manages (--unmanaged-only); the snapshot has all of them.\n", text.Plural(c.written, "resource", "resources"))
	}
}

// printFound prints what the export found: what was applied by hand, and
// what was held back for secret values in the manifest.
func printFound(w io.Writer, ov kustomize.Overview) {
	if n := ov.ByHandCount(); n > 0 {
		fmt.Fprintf(w, "\nApplied by hand, so this export is their only record (no GitOps tool, Helm or operator manages them): %d\n", n)
		for i, ns := range ov.Namespaces() {
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more namespaces (see README.md)\n", len(ov.ByHand)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  %-20s %3d  %s\n", cmp.Or(ns, "(cluster-scoped)"), len(ov.ByHand[ns]), truncate(kustomize.Grouped(ov.ByHand[ns]), 90))
		}
	}
	if len(ov.HeldBack) > 0 {
		fmt.Fprintf(w, "\nHeld back from the kustomization, secret values in the manifest (move them into Secrets):\n")
		for _, h := range ov.HeldBack {
			fmt.Fprintf(w, "  %s: %s\n", h.Resource, strings.Join(h.Fields, ", "))
		}
	}
}

// printRedacted says where the resources with redacted values went.
func printRedacted(w io.Writer, c exportCounts, externalSecrets bool) {
	if c.referenceOnly > 0 {
		fmt.Fprintf(w, "%s with redacted values %s written to redacted/ for reference only.\n"+
			"They are in no kustomization.yaml: applying them would overwrite the real values. See redacted/README.md.\n",
			text.Plural(c.referenceOnly, "resource", "resources"), text.Word(c.referenceOnly, "was", "were"))
		if externalSecrets && c.redactedSecrets > 0 {
			fmt.Fprintf(w, "%s for the redacted Secrets %s in external-secrets/, not applied until the values are in the secret manager (see its README).\n",
				text.Plural(c.redactedSecrets, "ExternalSecret", "ExternalSecrets"), text.Word(c.redactedSecrets, "is", "are"))
		}
	}

}

// printNext points to RESTORE.md and README.md, and lists cleanup
// suggestions.
func printNext(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, c exportCounts, ov kustomize.Overview) {
	fmt.Fprintf(w, "\nTo recreate this cluster elsewhere, follow %s", filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "RESTORE.md"))
	if todo := restoreTodo(s, c.needsValues, c.regenerated); len(todo) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(todo, ", "))
	}
	fmt.Fprintln(w, ".")
	fmt.Fprintf(w, "Overview: %s (%s, %s; every finding is in the snapshot).\n",
		filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "README.md"), text.Plural(c.redactions, "redaction", "redactions"), text.Plural(c.findings, "finding", "findings"))

	if lines := ov.Cleanup; len(lines) > 0 {
		fmt.Fprintf(w, "\nCleanup suggestions (SetDrift changes nothing in the cluster):\n")
		for i, l := range lines {
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more in the snapshot\n", len(lines)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
}

// printAccess says what the credentials limited the read to, before the summary.
func printAccess(w io.Writer, s *snapshotv1alpha1.Snapshot) {
	sc := s.GetScope()
	who := "these credentials"
	if id := sc.GetIdentity(); id != "" {
		who += " (" + id + ")"
	}
	switch sc.GetMode() {
	case snapshot.ScopeContextNamespace:
		hint := ""
		for _, f := range s.Findings {
			if f.Code == collect.NamespaceScopedAccess {
				if _, after, ok := strings.Cut(f.Message, "was exported; "); ok {
					hint = " " + capitalize(after) + "."
				}
			}
		}
		fmt.Fprintf(w, "Read namespace %s only: %s may not list namespaces, so SetDrift read the kubeconfig context's namespace.%s\n"+
			"Pass -n to export other namespaces you can read.\n\n", strings.Join(sc.GetNamespaces(), ", "), who, hint)
	case snapshot.ScopeReadableNamespaces:
		fmt.Fprintf(w, "Read namespaces %s only: %s can't read across the cluster, only in these, so cluster-scoped resources were not read either.\n"+
			"Not readable with them (%d): %s\n\n", strings.Join(sc.GetNamespaces(), ", "), who, len(sc.GetUnreadable()), truncate(strings.Join(sc.GetUnreadable(), ", "), 200))
	}
	var denied []string
	for _, f := range s.Findings {
		if f.Code == collect.ListDenied {
			denied = append(denied, f.Message)
		}
	}
	if len(denied) > 0 {
		fmt.Fprintln(w, "Not read for lack of permission:")
		for i, d := range denied {
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more namespaces in the snapshot (LIST_DENIED)\n", len(denied)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  %s\n", capitalize(d))
		}
		fmt.Fprintln(w)
	}
}

// restoreTodo summarizes what RESTORE.md asks of the user.
func restoreTodo(s *snapshotv1alpha1.Snapshot, needsValues, regenerated int) []string {
	var todo []string
	if needsValues > 0 {
		todo = append(todo, text.Plural(needsValues, "resource", "resources")+" to supply secret values for")
	}
	if regenerated > 0 {
		todo = append(todo, text.Plural(regenerated, "Secret", "Secrets")+" their controllers create again (nothing to do)")
	}
	if n := len(s.GetInventory().GetVolumes()); n > 0 {
		todo = append(todo, text.Plural(n, "volume", "volumes")+" to bring over")
	}
	crds, irsa := 0, 0
	releases := map[string]bool{}
	for _, r := range s.Resources {
		if kustomize.Platform(r) {
			continue
		}
		switch {
		case r.Kind == "CustomResourceDefinition":
			crds++
		case r.Kind == "ServiceAccount" && strings.Contains(r.ManifestJson, `"eks.amazonaws.com/role-arn"`):
			irsa++
		}
		if p := r.GetProvenance(); p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM && p.GetManager() != "" && p.GetEvidence()["confidence"] != "low" {
			releases[p.GetManager()] = true
		}
	}
	if crds > 0 {
		todo = append(todo, text.Plural(crds, "CRD", "CRDs")+" to create first")
	}
	if len(releases) > 0 {
		todo = append(todo, text.Plural(len(releases), "Helm release", "Helm releases")+" to install")
	}
	if irsa > 0 {
		todo = append(todo, text.Plural(irsa, "IRSA service account", "IRSA service accounts")+" whose roles must trust the new cluster")
	}
	if n := len(s.GetInventory().GetAws().GetPodIdentityAssociations()); n > 0 {
		todo = append(todo, text.Plural(n, "Pod Identity association", "Pod Identity associations")+" to create")
	}
	if n := len(s.GetInventory().GetLoadBalancers()); n > 0 {
		todo = append(todo, text.Plural(n, "load balancer address", "load balancer addresses")+" that will change")
	}
	return todo
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
