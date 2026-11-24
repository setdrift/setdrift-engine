// prints what setdrift export says when its done, what was read and written, what the export found, and what RESTORE.md asks
// export.go calls printSummary once the export is written

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

// truncate takes a string and shortens it to n runes ending in an ellipsis, so long lists fit on a terminal line
// it counts runes and not bytes, so a multi-byte character is never cut in half
func truncate(s string, n int) string {
	// checking if the string is longer than n runes, if so it gets cut to n-1 and the ellipsis
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// maxSuggestions caps how many cleanup suggestions and similar lists get printed, the snapshot has all of them
const maxSuggestions = 10

// platformCreators names who of the platform created the objects the export left out, like "kind and kubeadm"
func platformCreators(s *snapshotv1alpha1.Snapshot) string {
	var by []string
	// going through the resources to collect the names of the platform managers
	for _, r := range s.Resources {
		// invoking kustomize.Platform and checking if the platform manages this resource, its defined in pkg/emit/kustomize/kustomize.go
		if kustomize.Platform(r) {
			// invoking kustomize.PlatformName for the manager's display name and checking if its new, its defined in pkg/emit/kustomize/kustomize.go
			if n := kustomize.PlatformName(r.GetProvenance().GetManager()); !slices.Contains(by, n) {
				by = append(by, n)
			}
		}
	}
	sort.Strings(by)
	return strings.Join(by, " and ")
}

// exportCounts holds the numbers an export's summary prints, countExport fills it
type exportCounts struct {
	// every resource read, by who manages it
	bySource map[snapshotv1alpha1.Source]int
	// the findings and redactions in the snapshot
	findings, redactions int
	// written is the resources written, referenceOnly is those of them written to redacted/
	// redactedSecrets is the Secrets with values to supply, which get an ExternalSecret
	written, referenceOnly, redactedSecrets int
	// needsValues is the resources someone must supply values for
	// regenerated is those whose only values a controller writes again
	needsValues, regenerated int
}

// countExport takes the snapshot and counts what the summary reports
// with unmanagedOnly only the unmanaged resources were written, so only those count as written
func countExport(s *snapshotv1alpha1.Snapshot, unmanagedOnly bool) exportCounts {
	c := exportCounts{bySource: map[snapshotv1alpha1.Source]int{}, findings: len(s.Findings)}
	// going through the resources and counting each one, the kustomize helpers used here are defined in pkg/emit/kustomize/kustomize.go
	for _, r := range s.Resources {
		c.bySource[r.GetProvenance().GetSource()]++
		c.findings += len(r.Findings)
		c.redactions += len(r.Redactions)
		// invoking kustomize.NotWritten and kustomize.Unmanaged and checking if the resource wasnt written, if so it counts only as read
		if kustomize.NotWritten(r) || (unmanagedOnly && !kustomize.Unmanaged(r)) {
			continue
		}
		c.written++
		// invoking kustomize.NeedsValues and checking if someone must supply its values
		// if not, invoking kustomize.ReferenceOnly to check if it had values taken out that a controller writes again
		if kustomize.NeedsValues(r) {
			c.needsValues++
		} else if kustomize.ReferenceOnly(r) {
			c.regenerated++
		}
		// invoking kustomize.ReferenceOnly and checking if it went to redacted/ for reference only
		if kustomize.ReferenceOnly(r) {
			c.referenceOnly++
			// checking if its a Secret with values to supply, only those get an ExternalSecret, a controller recreates the others
			if r.Kind == "Secret" && r.ApiVersion == "v1" && kustomize.NeedsValues(r) {
				c.redactedSecrets++
			}
		}
	}
	return c
}

// printSummary prints what export did, what the credentials limited the read to, what was read by who manages it, what the export found, and where to go next
// export.go calls it once the export is written
func printSummary(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, unmanagedOnly, externalSecrets bool) {
	// invoking countExport to count what gets reported, its defined above
	c := countExport(s, unmanagedOnly)
	// invoking printAccess and printRead to say what was read, theyre defined below
	printAccess(w, s)
	printRead(w, s, out, c, unmanagedOnly)
	// invoking kustomize.NewOverview to sum up what the export found, its defined in pkg/emit/kustomize/overview.go
	ov := kustomize.NewOverview(s, unmanagedOnly)
	// invoking printFound, printRedacted and printNext to print the rest, theyre defined below
	printFound(w, ov)
	fmt.Fprintln(w)
	printRedacted(w, c, externalSecrets)
	printNext(w, s, out, c, ov)
}

// printRead prints what was exported, the platform, and the resources by who manages them
func printRead(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, c exportCounts, unmanagedOnly bool) {
	// invoking text.Plural to word the count, its defined in internal/text/text.go
	fmt.Fprintf(w, "Exported %s from %s to %s\n", text.Plural(len(s.Resources), "resource", "resources"), s.Cluster.GetName(), out)
	// checking if the cluster was classified, if so the platform gets a line
	if c := s.Cluster.GetClassification(); c != nil {
		// invoking platform.FromProto to turn the stored classification back into one, its defined in pkg/platform/probe.go
		cls := platform.FromProto(c)
		// invoking Dir and checking if theres a platform set for it, if so the line names it, Dir and Summary are defined in pkg/platform/platform.go
		// otherwise checking if anything about the platform is known, if so the line says it wasnt certain enough
		if dir := cls.Dir(); dir != "" {
			fmt.Fprintf(w, "Platform: %s. The export is generic (any cluster); %s/ is the set for the same platform.\n", cls.Summary(), dir)
		} else if cls.Distribution != platform.Unknown || cls.Infrastructure != platform.UnknownInfra {
			fmt.Fprintf(w, "Platform: %s, not certain enough for platform-specific output; the export is generic.\n", cls.Summary())
		}
	}
	// going through every source from unmanaged to ci/cd, in the order the summary lists them
	for src := snapshotv1alpha1.Source_SOURCE_UNMANAGED; src <= snapshotv1alpha1.Source_SOURCE_CI_CD; src++ {
		// checking if any resources came from this source, if so it gets a line
		if n := c.bySource[src]; n > 0 {
			note := ""
			// checking if the source needs a note, platform and ci/cd do
			switch src {
			case snapshotv1alpha1.Source_SOURCE_PLATFORM:
				// invoking platformCreators to name who created them, its defined above
				note = "  (created by " + platformCreators(s) + "; in the snapshot, not written)"
			case snapshotv1alpha1.Source_SOURCE_CI_CD:
				note = "  (deployed by a pipeline: setdrift.io/source-* annotations)"
			}
			name := strings.ToLower(strings.TrimPrefix(src.String(), "SOURCE_"))
			// checking if its ci/cd, if so it gets its slash rather than the enum's ci_cd
			if src == snapshotv1alpha1.Source_SOURCE_CI_CD {
				name = "ci/cd"
			}
			fmt.Fprintf(w, "  %-10s %d%s\n", name, n, note)
		}
	}
	// checking if --unmanaged-only was given, if so the summary says only those were written
	if unmanagedOnly {
		fmt.Fprintf(w, "Wrote the %s nothing manages (--unmanaged-only); the snapshot has all of them.\n", text.Plural(c.written, "resource", "resources"))
	}
}

// printFound prints what the export found, what was applied by hand, and what was held back for secret values in the manifest
// the overview methods used here are defined in pkg/emit/kustomize/overview.go
func printFound(w io.Writer, ov kustomize.Overview) {
	// invoking ByHandCount and checking if anything was applied by hand, if so those get listed by namespace
	if n := ov.ByHandCount(); n > 0 {
		fmt.Fprintf(w, "\nApplied by hand, so this export is their only record (no GitOps tool, Helm or operator manages them): %d\n", n)
		// invoking Namespaces to go through the namespaces in order, cluster-scoped first
		for i, ns := range ov.Namespaces() {
			// checking if maxSuggestions is reached, if so the rest are only counted
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more namespaces (see README.md)\n", len(ov.ByHand)-maxSuggestions)
				break
			}
			// invoking kustomize.Grouped to join the resources by kind and truncate to fit the line, truncate is defined above
			fmt.Fprintf(w, "  %-20s %3d  %s\n", cmp.Or(ns, "(cluster-scoped)"), len(ov.ByHand[ns]), truncate(kustomize.Grouped(ov.ByHand[ns]), 90))
		}
	}
	// checking if any resources were held back from the kustomization, if so each is listed with where its secret values are
	if len(ov.HeldBack) > 0 {
		fmt.Fprintf(w, "\nHeld back from the kustomization, secret values in the manifest (move them into Secrets):\n")
		// printing each one with its fields
		for _, h := range ov.HeldBack {
			fmt.Fprintf(w, "  %s: %s\n", h.Resource, strings.Join(h.Fields, ", "))
		}
	}
}

// printRedacted says where the resources with redacted values went, and the ExternalSecrets made for them
func printRedacted(w io.Writer, c exportCounts, externalSecrets bool) {
	// checking if any resources went to redacted/, if so the summary says why theyre not applied
	if c.referenceOnly > 0 {
		// invoking text.Plural and text.Word to word the counts, theyre defined in internal/text/text.go
		fmt.Fprintf(w, "%s with redacted values %s written to redacted/ for reference only.\n"+
			"They are in no kustomization.yaml: applying them would overwrite the real values. See redacted/README.md.\n",
			text.Plural(c.referenceOnly, "resource", "resources"), text.Word(c.referenceOnly, "was", "were"))
		// checking if ExternalSecrets are written and some Secrets got one, if so the summary says where they are
		if externalSecrets && c.redactedSecrets > 0 {
			fmt.Fprintf(w, "%s for the redacted Secrets %s in external-secrets/, not applied until the values are in the secret manager (see its README).\n",
				text.Plural(c.redactedSecrets, "ExternalSecret", "ExternalSecrets"), text.Word(c.redactedSecrets, "is", "are"))
		}
	}

}

// printNext points to RESTORE.md and README.md with what they ask, and lists the cleanup suggestions
func printNext(w io.Writer, s *snapshotv1alpha1.Snapshot, out string, c exportCounts, ov kustomize.Overview) {
	// invoking kustomize.ClusterDir to find the cluster's directory in the export, its defined in pkg/emit/kustomize/kustomize.go
	fmt.Fprintf(w, "\nTo recreate this cluster elsewhere, follow %s", filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "RESTORE.md"))
	// invoking restoreTodo and checking if RESTORE.md asks anything, if so its listed on the same line, its defined below
	if todo := restoreTodo(s, c.needsValues, c.regenerated); len(todo) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(todo, ", "))
	}
	fmt.Fprintln(w, ".")
	// invoking kustomize.ClusterDir again for the README.md and text.Plural to word the counts, its defined in internal/text/text.go
	fmt.Fprintf(w, "Overview: %s (%s, %s; every finding is in the snapshot).\n",
		filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "README.md"), text.Plural(c.redactions, "redaction", "redactions"), text.Plural(c.findings, "finding", "findings"))

	// checking if there are cleanup suggestions, if so up to maxSuggestions of them are printed
	if lines := ov.Cleanup; len(lines) > 0 {
		fmt.Fprintf(w, "\nCleanup suggestions (SetDrift changes nothing in the cluster):\n")
		// printing each suggestion
		for i, l := range lines {
			// checking if maxSuggestions is reached, if so the rest are only counted
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more in the snapshot\n", len(lines)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
}

// printAccess says what the credentials limited the read to, before the rest of the summary
func printAccess(w io.Writer, s *snapshotv1alpha1.Snapshot) {
	sc := s.GetScope()
	who := "these credentials"
	// checking if we know who the credentials belong to, if so the message names them
	if id := sc.GetIdentity(); id != "" {
		who += " (" + id + ")"
	}
	// checking how the read was scoped, the modes are defined in pkg/snapshot/snapshot.go, a full read prints nothing here
	switch sc.GetMode() {
	case snapshot.ScopeContextNamespace:
		hint := ""
		// going through the findings to find the namespace-scoped access one, its hint goes in the message
		for _, f := range s.Findings {
			// checking if its the namespace-scoped access finding, its code is defined in pkg/collect/cluster.go
			if f.Code == collect.NamespaceScopedAccess {
				// checking if the message has a hint after "was exported; ", if so that part is used
				// invoking capitalize to start it as a sentence, its defined in kubeconfig.go
				if _, after, ok := strings.Cut(f.Message, "was exported; "); ok {
					hint = " " + capitalize(after) + "."
				}
			}
		}
		fmt.Fprintf(w, "Read namespace %s only: %s may not list namespaces, so SetDrift read the kubeconfig context's namespace.%s\n"+
			"Pass -n to export other namespaces you can read.\n\n", strings.Join(sc.GetNamespaces(), ", "), who, hint)
	case snapshot.ScopeReadableNamespaces:
		// invoking truncate to keep the unreadable namespaces to 200 characters, its defined above
		fmt.Fprintf(w, "Read namespaces %s only: %s can't read across the cluster, only in these, so cluster-scoped resources were not read either.\n"+
			"Not readable with them (%d): %s\n\n", strings.Join(sc.GetNamespaces(), ", "), who, len(sc.GetUnreadable()), truncate(strings.Join(sc.GetUnreadable(), ", "), 200))
	}
	var denied []string
	// going through the findings to collect what couldnt be listed for lack of permission
	for _, f := range s.Findings {
		// checking if its a list denied finding, its code is defined in pkg/collect/cluster.go
		if f.Code == collect.ListDenied {
			denied = append(denied, f.Message)
		}
	}
	// checking if anything was denied, if so up to maxSuggestions of them are printed
	if len(denied) > 0 {
		fmt.Fprintln(w, "Not read for lack of permission:")
		// printing each denied message
		for i, d := range denied {
			// checking if maxSuggestions is reached, if so the rest are only counted
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more namespaces in the snapshot (LIST_DENIED)\n", len(denied)-maxSuggestions)
				break
			}
			// invoking capitalize to start the message as a sentence, its defined in kubeconfig.go
			fmt.Fprintf(w, "  %s\n", capitalize(d))
		}
		fmt.Fprintln(w)
	}
}

// restoreTodo sums up what RESTORE.md asks of the user, one short item each, for the line that points to it
// each item words its count with text.Plural, its defined in internal/text/text.go
func restoreTodo(s *snapshotv1alpha1.Snapshot, needsValues, regenerated int) []string {
	var todo []string
	// checking if some resources need their secret values supplied
	if needsValues > 0 {
		todo = append(todo, text.Plural(needsValues, "resource", "resources")+" to supply secret values for")
	}
	// checking if some Secrets get recreated by their controllers, theyre listed so nobody goes looking for their values
	if regenerated > 0 {
		todo = append(todo, text.Plural(regenerated, "Secret", "Secrets")+" their controllers create again (nothing to do)")
	}
	// checking if the snapshot has volumes, their data has to be brought over separately
	if n := len(s.GetInventory().GetVolumes()); n > 0 {
		todo = append(todo, text.Plural(n, "volume", "volumes")+" to bring over")
	}
	crds, irsa := 0, 0
	releases := map[string]bool{}
	// going through the resources to count the crds, the irsa service accounts and the helm releases
	for _, r := range s.Resources {
		// invoking kustomize.Platform and checking if the platform manages it, if so a new cluster brings its own, its defined in pkg/emit/kustomize/kustomize.go
		if kustomize.Platform(r) {
			continue
		}
		// checking if its a crd, or a service account with an irsa role annotation
		switch {
		case r.Kind == "CustomResourceDefinition":
			crds++
		case r.Kind == "ServiceAccount" && strings.Contains(r.ManifestJson, `"eks.amazonaws.com/role-arn"`):
			irsa++
		}
		// checking if helm manages it with a known release name, and not only with low confidence, if so the release counts
		if p := r.GetProvenance(); p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM && p.GetManager() != "" && p.GetEvidence()["confidence"] != "low" {
			releases[p.GetManager()] = true
		}
	}
	// checking if there are crds, they have to be created before the resources that use them
	if crds > 0 {
		todo = append(todo, text.Plural(crds, "CRD", "CRDs")+" to create first")
	}
	// checking if there are helm releases to install
	if len(releases) > 0 {
		todo = append(todo, text.Plural(len(releases), "Helm release", "Helm releases")+" to install")
	}
	// checking if there are irsa service accounts, their iam roles have to trust the new cluster
	if irsa > 0 {
		todo = append(todo, text.Plural(irsa, "IRSA service account", "IRSA service accounts")+" whose roles must trust the new cluster")
	}
	// checking if the snapshot has eks pod identity associations to create
	if n := len(s.GetInventory().GetAws().GetPodIdentityAssociations()); n > 0 {
		todo = append(todo, text.Plural(n, "Pod Identity association", "Pod Identity associations")+" to create")
	}
	// checking if the snapshot has load balancers, their addresses change on a new cluster
	if n := len(s.GetInventory().GetLoadBalancers()); n > 0 {
		todo = append(todo, text.Plural(n, "load balancer address", "load balancer addresses")+" that will change")
	}
	return todo
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
