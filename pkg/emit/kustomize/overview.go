// sums up what an export found, who manages what, what was applied by hand, what was held back and what could be cleaned up
// the cli prints it after an export and README.md has it as a section

package kustomize

import (
	"cmp"
	"fmt"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// CleanupCodes are the findings shown as cleanup suggestions, in order
var CleanupCodes = []string{"DELETION_STUCK", "TERMINATED_PODS_TO_CLEANUP", "JOBS_WITHOUT_TTL", "CRONJOB_HISTORY_LIMIT"}

// builtinNamespaces are created by kubernetes in every cluster, nobody applied them so theyre not listed as applied by hand
var builtinNamespaces = map[string]bool{"default": true, "kube-system": true, "kube-public": true, "kube-node-lease": true}

// Overview is what an export found, who manages what, what nothing manages, and what was held back
// the cli prints it and README.md has it
type Overview struct {
	// BySource counts every resource read, by who manages it
	BySource map[snapshotv1alpha1.Source]int
	// ByHand are the written resources no gitops tool, helm or operator manages, by namespace ("" for cluster-scoped), each as "<Kind> <name>"
	ByHand map[string][]string
	// HeldBack are the written resources other than Secrets that hold secret values in their manifest, so theyre in redacted/ instead of the kustomization
	// each is "<Kind> <namespace>/<name>" and where the values are
	HeldBack []Held
	// Secrets is how many Secrets need their values supplied
	Secrets int
	// Cleanup are the cleanup suggestions, e.g. Jobs without a ttl
	Cleanup []string
}

// Held is a resource written to redacted/ for a secret value in its manifest
type Held struct {
	// the resource, as "<Kind> <namespace>/<name>"
	Resource string
	// the paths of the secret values in its manifest
	Fields []string
}

// NewOverview sums up s, with unmanagedOnly only the unmanaged resources were written so only they count as written
func NewOverview(s *snapshotv1alpha1.Snapshot, unmanagedOnly bool) Overview {
	o := Overview{BySource: map[snapshotv1alpha1.Source]int{}, ByHand: map[string][]string{}}
	// the findings by code, each as a line naming its resource
	cleanup := map[string][]string{}
	// going through every resource the snapshot read
	for _, r := range s.GetResources() {
		o.BySource[r.GetProvenance().GetSource()]++
		// going through r's findings to keep each as a possible cleanup suggestion
		for _, f := range r.GetFindings() {
			// invoking qualified to name r, its defined below
			cleanup[f.GetCode()] = append(cleanup[f.GetCode()], fmt.Sprintf("%s %s: %s", r.Kind, qualified(r), f.GetMessage()))
		}
		// invoking NotWritten and Unmanaged, both defined in kustomize.go, to skip what the export didnt write
		if NotWritten(r) || (unmanagedOnly && !Unmanaged(r)) {
			continue
		}
		isSecret := r.Kind == "Secret" && r.ApiVersion == "v1"
		// invoking NeedsValues to tell if someone must supply values for r, its defined in kustomize.go
		if NeedsValues(r) {
			// checking if r is a Secret, if so its counted, any other resource is listed as held back with where its values are
			if isSecret {
				o.Secrets++
			} else {
				h := Held{Resource: r.Kind + " " + qualified(r)}
				// going through r's redactions to collect the paths of its secret values
				for _, red := range r.GetRedactions() {
					// checking if this one was taken out as a secret value
					if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
						h.Fields = append(h.Fields, red.GetPath())
					}
				}
				o.HeldBack = append(o.HeldBack, h)
			}
		}
		// checking if nothing manages r and its not a built-in Namespace, if so it was applied by hand
		if Unmanaged(r) && !(r.Kind == "Namespace" && builtinNamespaces[r.Name]) {
			o.ByHand[r.Namespace] = append(o.ByHand[r.Namespace], r.Kind+" "+r.Name)
		}
	}
	// keeping the findings that are cleanup suggestions, in the order of CleanupCodes
	for _, code := range CleanupCodes {
		o.Cleanup = append(o.Cleanup, cleanup[code]...)
	}
	// sorting the applied-by-hand list of each namespace
	for ns := range o.ByHand {
		sort.Strings(o.ByHand[ns])
	}
	// sorting the held back resources by name
	sort.Slice(o.HeldBack, func(i, j int) bool { return o.HeldBack[i].Resource < o.HeldBack[j].Resource })
	return o
}

// qualified gives r's name, with its namespace in front when it has one
func qualified(r *snapshotv1alpha1.Resource) string {
	// checking if r is cluster-scoped, if so its name alone
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// ByHandCount gives how many resources ByHand lists
func (o Overview) ByHandCount() int {
	n := 0
	// adding up the list of each namespace
	for _, l := range o.ByHand {
		n += len(l)
	}
	return n
}

// Namespaces gives ByHand's namespaces sorted, cluster-scoped ("") first
func (o Overview) Namespaces() []string {
	// invoking sortedMapKeys to sort them, its defined in restore.go
	return sortedMapKeys(o.ByHand)
}

// Grouped joins "<Kind> <name>" entries by kind, each kind as "Kind a, b", the kinds separated by semicolons in the order they first show up
func Grouped(entries []string) string {
	var kinds []string
	names := map[string][]string{}
	// going through the entries to collect the names of each kind
	for _, e := range entries {
		k, n, _ := strings.Cut(e, " ")
		// checking if this kind is new, if so it goes on the list of kinds
		if names[k] == nil {
			kinds = append(kinds, k)
		}
		names[k] = append(names[k], n)
	}
	parts := make([]string, len(kinds))
	// joining each kind with its names
	for i, k := range kinds {
		parts[i] = k + " " + strings.Join(names[k], ", ")
	}
	return strings.Join(parts, "; ")
}

// markdown builds the overview's section of README.md, empty when theres nothing to say
func (o Overview) markdown() string {
	// invoking ByHandCount, its defined above, and checking if the overview has nothing at all, if so theres no section
	if o.ByHandCount() == 0 && len(o.HeldBack) == 0 && o.Secrets == 0 && len(o.Cleanup) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## What SetDrift found\n\n")
	// checking if anything was applied by hand, if so a table lists it by namespace
	// invoking text.Plural for the count, its defined in internal/text/text.go
	if n := o.ByHandCount(); n > 0 {
		fmt.Fprintf(&b, "**Applied by hand: %s.** No GitOps tool, Helm or operator manages these,\n"+
			"so this export is the only record of them. Adopt them into Git (export with\n"+
			"`--unmanaged-only` to write just these):\n\n", text.Plural(n, "resource", "resources"))
		b.WriteString("| Namespace | Resources |\n|---|---|\n")
		// invoking Namespaces and Grouped, both defined above, for a row per namespace
		for _, ns := range o.Namespaces() {
			fmt.Fprintf(&b, "| %s | %s |\n", cmp.Or(ns, "(cluster-scoped)"), Grouped(o.ByHand[ns]))
		}
		b.WriteString("\n")
	}
	// checking if any resource was held back, if so each is listed with where its secret values are
	if len(o.HeldBack) > 0 {
		fmt.Fprintf(&b, "**Held back: %s with secret values in the manifest.** They are in\n"+
			"redacted/, not in the kustomization, so applying this export leaves them out.\n"+
			"Move the values into Secrets and reference them, and the next export\n"+
			"includes them:\n\n", text.Plural(len(o.HeldBack), "resource", "resources"))
		// adding a line for each held back resource
		for _, h := range o.HeldBack {
			fmt.Fprintf(&b, "- %s: `%s`\n", h.Resource, strings.Join(h.Fields, "`, `"))
		}
		b.WriteString("\n")
	}
	// checking if any Secret needs its values, if so the section points at RESTORE.md
	if o.Secrets > 0 {
		fmt.Fprintf(&b, "**Secrets: %s whose values were left out.** RESTORE.md's secrets step\n"+
			"says how to supply them.\n\n", text.Plural(o.Secrets, "Secret", "Secrets"))
	}
	// checking if there are cleanup suggestions, if so each gets a line
	if len(o.Cleanup) > 0 {
		b.WriteString("**Cleanup suggestions** (SetDrift changes nothing in the cluster):\n\n")
		// adding a line for each suggestion
		for _, c := range o.Cleanup {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
