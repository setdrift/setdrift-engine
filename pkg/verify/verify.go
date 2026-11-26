// Package verify checks an exported repository against the live cluster with a server-side dry-run apply
// its the MVP acceptance test, at least 95% of resources, or another threshold, must show no diff, and every remaining diff must be explained
// the report types and the pass or fail test live here, the dry-run itself in dryrun.go
package verify

import (
	"strings"
	"time"
)

// AcceptancePercent is the default minimum share of resources, in percent, that must round-trip with no diff, Report.Threshold sets another
// Passed compares integers, so no float rounding decides a run at the threshold
const AcceptancePercent = 95

// Kind is the cause of a Diff
type Kind string

// the kinds of Diff
const (
	// KindChanged means applying the export would change fields of the live object
	KindChanged Kind = "changed"
	// KindNotInCluster means the resource isnt in the cluster, applying would create it
	KindNotInCluster Kind = "not-in-cluster"
	// KindNotServed means the cluster doesnt serve the resource's type, a CRD is missing
	KindNotServed Kind = "type-not-served"
	// KindLost means the live object has spec or data the export lost entirely
	KindLost Kind = "lost-fields"
	// KindRejected means the cluster rejected the dry-run apply
	KindRejected Kind = "rejected"
	// KindWontWork means the resource would apply but names something this cluster's platform doesnt have, like another cloud's load balancer class or identity annotation, so it wouldnt work here
	// Paths say what it names and what happens, from the capability map
	KindWontWork Kind = "wont-work-here"
	// KindOutsideCluster means the resource is right here and needs a step outside the cluster to work, like a kept cloud identity the cloud must trust this cluster for
	// its clean, so it counts as would not change, Explanation says the step
	KindOutsideCluster Kind = "needs-step-outside-cluster"
	// KindNotChecked means the credentials may not get the resource, or may not patch it for the dry-run, so it couldnt be checked, Diff.Refused says which
	// its not counted in Total, it says nothing about the export
	KindNotChecked Kind = "not-checked"
)

// what the credentials were refused, for a KindNotChecked Diff
const (
	// RefusedGet means they may not get the resource
	RefusedGet = "get"
	// RefusedPatch means they may get it but not patch it, which the dry-run needs
	RefusedPatch = "patch"
)

// Diff is a resource that wouldnt come out of the export unchanged
// the dry-run apply result differs from the live object, or the resource isnt in the cluster, its type isnt served, the apply was rejected, or the export lost its spec or data
type Diff struct {
	// Ref names the resource, as Ref in dryrun.go gives it
	Ref  string `json:"ref"`
	Kind Kind   `json:"kind"`
	// Refused is what the credentials were refused, RefusedGet or RefusedPatch, for KindNotChecked
	Refused string `json:"refused,omitempty"`
	// Paths are the first fields that differ, or what went wrong
	Paths []string `json:"paths,omitempty"`
	// Fields are the differing fields with their values, for KindChanged
	// Cluster is the live value, Export the value applying the export gives
	Fields []Field `json:"fields,omitempty"`
	// ChangedAfterExport lists who wrote the live object after the export was taken, from its managedFields, to say why it no longer matches
	ChangedAfterExport []Write `json:"changedAfterExport,omitempty"`
	// Explanation is the known cause, like a bare Pod which cant be updated in place
	// empty means unexplained, which fails acceptance
	Explanation string `json:"explanation,omitempty"`
}

// Field is one differing field of a changed resource, Cluster or Export is nil where the field is absent
type Field struct {
	Path    string `json:"path"`
	Cluster any    `json:"cluster,omitempty"`
	Export  any    `json:"export,omitempty"`
	// SetBy are the field managers that own the live value, from its managedFields, so the report can say who set what the cluster has
	SetBy []Owner `json:"setBy,omitempty"`
}

// Owner is a field manager owning a field, with the operation and subresource it wrote through
type Owner struct {
	Manager     string `json:"manager"`
	Operation   string `json:"operation,omitempty"`
	Subresource string `json:"subresource,omitempty"`
}

// ByHand tells if the owner is a person's kubectl command, like edit, patch, set, scale, label or annotate, rather than an apply of manifests
func (o Owner) ByHand() bool {
	// checking if its a kubectl apply, client-side or server-side, if so its manifests and not by hand
	// otherwise any other kubectl manager counts as by hand
	switch {
	case o.Manager == "kubectl-client-side-apply", o.Manager == "kubectl" && o.Operation == "Apply":
		return false
	case o.Manager == "kubectl", strings.HasPrefix(o.Manager, "kubectl-"):
		return true
	}
	return false
}

// Write is a field manager's write to a live object, with when it happened
type Write struct {
	Manager   string    `json:"manager"`
	Operation string    `json:"operation,omitempty"`
	Time      time.Time `json:"time"`
}

// Report is the result of a verification run, what was checked, what differs and what was left out
type Report struct {
	// Total is how many resources were checked, Clean how many of them wouldnt change
	Total int    `json:"total"`
	Clean int    `json:"clean"`
	Diffs []Diff `json:"diffs,omitempty"`
	// Skipped counts resources left out on purpose, by reason, like Secrets under redacted/
	// they arent in Total
	Skipped map[string]int `json:"skipped,omitempty"`
	// MissingCRDs are the CustomResourceDefinitions in the export that the cluster lacks
	// until they exist their custom resources cant be checked or applied, and show as types the cluster doesnt serve
	MissingCRDs []string `json:"missingCRDs,omitempty"`
	// Threshold is the share of resources, in percent, that must not change, zero means AcceptancePercent
	Threshold int `json:"threshold"`
	// NotChecked are the resources the credentials may not get, or patch for the dry-run, of Kind KindNotChecked
	// theyre not in Total, and any of them fails the run
	NotChecked []Diff `json:"notChecked,omitempty"`
	// OutsideCluster are the clean resources that need a step outside the cluster to work, of Kind KindOutsideCluster
	// theyre counted as clean, and listed so the step isnt forgotten
	OutsideCluster []Diff `json:"outsideCluster,omitempty"`
	// Namespaces limits the check to these namespaces, from -n, empty is every resource
	Namespaces []string `json:"namespaces,omitempty"`
}

// RequiredPercent gives back the threshold the run is held to, AcceptancePercent when none was set
func (r Report) RequiredPercent() int {
	// checking if no threshold was set
	if r.Threshold == 0 {
		return AcceptancePercent
	}
	return r.Threshold
}

// CleanRatio gives back the share of resources with no diff, 0 when there are none
func (r Report) CleanRatio() float64 {
	// checking if nothing was checked, so theres no dividing by zero
	if r.Total == 0 {
		return 0
	}
	return float64(r.Clean) / float64(r.Total)
}

// CleanPercent gives back the share of resources with no diff in percent, rounded down to one decimal, so a failing run never prints a passing figure
func (r Report) CleanPercent() float64 {
	// checking if nothing was checked, so theres no dividing by zero
	if r.Total == 0 {
		return 0
	}
	return float64(r.Clean*1000/r.Total) / 10
}

// MeetsThreshold tells if enough resources would not change, comparing integers, a run that checked nothing doesnt meet it
func (r Report) MeetsThreshold() bool {
	// invoking RequiredPercent for the threshold, its defined above
	return r.Total > 0 && r.Clean*100 >= r.Total*r.RequiredPercent()
}

// Unexplained counts the differences with no known explanation
func (r Report) Unexplained() int {
	n := 0
	// counting each diff with an empty explanation
	for _, d := range r.Diffs {
		// checking if this diff has no explanation, if so its counted
		if d.Explanation == "" {
			n++
		}
	}
	return n
}

// Passed tells if the run meets the acceptance test, enough resources would not change, every difference is explained and every resource could be checked
// a run that checked nothing doesnt pass
func (r Report) Passed() bool {
	// invoking MeetsThreshold and Unexplained, theyre defined above
	return r.MeetsThreshold() && r.Unexplained() == 0 && len(r.NotChecked) == 0
}

// Analysis sums up a report's differences, to tell their likely cause
type Analysis struct {
	// ByKind counts the differences by Diff.Kind
	ByKind map[Kind]int `json:"byKind"`
	// ChangedAfterExport counts the differing resources written after the export, or whose differing fields a kubectl command set by hand
	ChangedAfterExport int `json:"changedAfterExport"`
	// Writers counts, by field manager, the resources it wrote after the export
	Writers map[string]int `json:"writers,omitempty"`
	// CommonField is a field that differs on most changed resources, at least 3 and at least half of them, one cause behind many differences
	// CommonCount is how many changed resources it differs on
	CommonField string `json:"commonField,omitempty"`
	CommonCount int    `json:"commonCount,omitempty"`
}

// Analyze sums up the report's differences, counting them by kind, who wrote them after the export or by hand, and the one field most of them share
func (r Report) Analyze() Analysis {
	a := Analysis{ByKind: map[Kind]int{}, Writers: map[string]int{}}
	fields := map[string]int{}
	changed := 0
	// going through each diff to count it
	for _, d := range r.Diffs {
		a.ByKind[d.Kind]++
		seen := map[string]bool{}
		// collecting who wrote the object after the export
		for _, w := range d.ChangedAfterExport {
			seen[w.Manager] = true
		}
		// going through each differing field's owners
		for _, f := range d.Fields {
			// invoking ByHand on each owner, a kubectl command by hand counts as a change too, its defined above
			for _, o := range f.SetBy {
				// checking if this owner changed it by hand, if so its noted
				if o.ByHand() {
					seen[o.Manager] = true
				}
			}
		}
		// checking if anyone changed it, if so its counted and so is each writer
		if len(seen) > 0 {
			a.ChangedAfterExport++
			// counting the resource once for each writer
			for m := range seen {
				a.Writers[m]++
			}
		}
		// checking if its a changed resource, if so its fields are counted to find the common one
		if d.Kind == KindChanged {
			changed++
			// counting each changed field, to find the most common one
			for _, f := range d.Fields {
				fields[f.Path]++
			}
		}
	}
	// going through each field's count for the common field
	for path, n := range fields {
		// checking if it differs on at least 3 and half the changed resources, and on more than the best so far, ties going to the first path in order so the result doesnt depend on map order
		if n >= 3 && n*2 >= changed && (n > a.CommonCount || (n == a.CommonCount && path < a.CommonField)) {
			a.CommonField, a.CommonCount = path, n
		}
	}
	return a
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
