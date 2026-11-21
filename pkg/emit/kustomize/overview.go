// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"fmt"
	"sort"
	"strings"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// CleanupCodes are the findings shown as cleanup suggestions, in order.
var CleanupCodes = []string{"DELETION_STUCK", "TERMINATED_PODS_TO_CLEANUP", "JOBS_WITHOUT_TTL", "CRONJOB_HISTORY_LIMIT"}

// builtinNamespaces are created by Kubernetes in every cluster; nobody
// applied them, so they are not listed as applied by hand.
var builtinNamespaces = map[string]bool{"default": true, "kube-system": true, "kube-public": true, "kube-node-lease": true}

// Overview is what an export found: who manages what, what nothing
// manages, and what was held back. The CLI prints it and README.md has it.
type Overview struct {
	// BySource counts every resource read, by who manages it.
	BySource map[snapshotv1alpha1.Source]int
	// ByHand are the written resources no GitOps tool, Helm or operator
	// manages, by namespace ("" for cluster-scoped): "<Kind> <name>".
	ByHand map[string][]string
	// HeldBack are the written resources other than Secrets that hold secret
	// values in their manifest, so are in redacted/ instead of the
	// kustomization: "<Kind> <namespace>/<name>" and where the values are.
	HeldBack []Held
	// Secrets is how many Secrets need their values supplied.
	Secrets int
	// Cleanup are the cleanup suggestions, e.g. Jobs without a TTL.
	Cleanup []string
}

// Held is a resource written to redacted/ for a secret value in its manifest.
type Held struct {
	Resource string
	Fields   []string
}

// NewOverview summarizes s; with unmanagedOnly only the unmanaged
// resources were written.
func NewOverview(s *snapshotv1alpha1.Snapshot, unmanagedOnly bool) Overview {
	o := Overview{BySource: map[snapshotv1alpha1.Source]int{}, ByHand: map[string][]string{}}
	cleanup := map[string][]string{}
	for _, r := range s.GetResources() {
		o.BySource[r.GetProvenance().GetSource()]++
		for _, f := range r.GetFindings() {
			cleanup[f.GetCode()] = append(cleanup[f.GetCode()], fmt.Sprintf("%s %s: %s", r.Kind, qualified(r), f.GetMessage()))
		}
		if NotWritten(r) || (unmanagedOnly && !Unmanaged(r)) {
			continue
		}
		isSecret := r.Kind == "Secret" && r.ApiVersion == "v1"
		if NeedsValues(r) {
			if isSecret {
				o.Secrets++
			} else {
				h := Held{Resource: r.Kind + " " + qualified(r)}
				for _, red := range r.GetRedactions() {
					if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
						h.Fields = append(h.Fields, red.GetPath())
					}
				}
				o.HeldBack = append(o.HeldBack, h)
			}
		}
		if Unmanaged(r) && !(r.Kind == "Namespace" && builtinNamespaces[r.Name]) {
			o.ByHand[r.Namespace] = append(o.ByHand[r.Namespace], r.Kind+" "+r.Name)
		}
	}
	for _, code := range CleanupCodes {
		o.Cleanup = append(o.Cleanup, cleanup[code]...)
	}
	for ns := range o.ByHand {
		sort.Strings(o.ByHand[ns])
	}
	sort.Slice(o.HeldBack, func(i, j int) bool { return o.HeldBack[i].Resource < o.HeldBack[j].Resource })
	return o
}

func qualified(r *snapshotv1alpha1.Resource) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// ByHandCount is how many resources ByHand lists.
func (o Overview) ByHandCount() int {
	n := 0
	for _, l := range o.ByHand {
		n += len(l)
	}
	return n
}

// Namespaces are ByHand's namespaces, cluster-scoped ("") first.
func (o Overview) Namespaces() []string {
	return sortedKeys(o.ByHand)
}

// Grouped joins "<Kind> <name>" entries as "Kind a, b; Kind c".
func Grouped(entries []string) string {
	var kinds []string
	names := map[string][]string{}
	for _, e := range entries {
		k, n, _ := strings.Cut(e, " ")
		if names[k] == nil {
			kinds = append(kinds, k)
		}
		names[k] = append(names[k], n)
	}
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = k + " " + strings.Join(names[k], ", ")
	}
	return strings.Join(parts, "; ")
}

// markdown is the overview's section of README.md.
func (o Overview) markdown() string {
	var b strings.Builder
	b.WriteString("\n## What SetDrift found\n\n")
	if n := o.ByHandCount(); n > 0 {
		fmt.Fprintf(&b, "**Applied by hand: %s.** No GitOps tool, Helm or operator manages these,\n"+
			"so this export is the only record of them. Adopt them into Git (export with\n"+
			"`--unmanaged-only` to write just these):\n\n", pluralize(n, "resource", "resources"))
		b.WriteString("| Namespace | Resources |\n|---|---|\n")
		for _, ns := range o.Namespaces() {
			fmt.Fprintf(&b, "| %s | %s |\n", orDefault(ns, "(cluster-scoped)"), Grouped(o.ByHand[ns]))
		}
		b.WriteString("\n")
	}
	if len(o.HeldBack) > 0 {
		fmt.Fprintf(&b, "**Held back: %s with secret values in the manifest.** They are in\n"+
			"redacted/, not in the kustomization, so applying this export leaves them out.\n"+
			"Move the values into Secrets and reference them, and the next export\n"+
			"includes them:\n\n", pluralize(len(o.HeldBack), "resource", "resources"))
		for _, h := range o.HeldBack {
			fmt.Fprintf(&b, "- %s: `%s`\n", h.Resource, strings.Join(h.Fields, "`, `"))
		}
		b.WriteString("\n")
	}
	if o.Secrets > 0 {
		fmt.Fprintf(&b, "**Secrets: %s whose values were left out.** RESTORE.md's secrets step\n"+
			"says how to supply them.\n\n", pluralize(o.Secrets, "Secret", "Secrets"))
	}
	if len(o.Cleanup) > 0 {
		b.WriteString("**Cleanup suggestions** (SetDrift changes nothing in the cluster):\n\n")
		for _, c := range o.Cleanup {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
	if b.Len() == len("\n## What SetDrift found\n\n") {
		return ""
	}
	return b.String()
}
