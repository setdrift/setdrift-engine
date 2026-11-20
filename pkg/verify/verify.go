// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package verify checks an exported repository against the live cluster with a
// server-side dry-run apply. This is the MVP acceptance test: at least 95% of
// resources (or another threshold) must show no diff, and every remaining
// diff must be explained.
package verify

import (
	"context"
	"strings"
	"time"
)

// AcceptancePercent is the default minimum share of resources, in percent,
// that must round-trip with no diff (Report.Threshold sets another).
// Passed compares whole numbers, so no float rounding decides a run at the
// threshold.
const AcceptancePercent = 95

// The kinds of Diff, by cause.
const (
	// KindChanged: applying the export would change fields of the live object.
	KindChanged = "changed"
	// KindNotInCluster: the resource is not in the cluster; applying would create it.
	KindNotInCluster = "not-in-cluster"
	// KindNotServed: the cluster does not serve the resource's type (a CRD is missing).
	KindNotServed = "type-not-served"
	// KindLost: the live object has spec or data the export lost entirely.
	KindLost = "lost-fields"
	// KindRejected: the cluster rejected the dry-run apply.
	KindRejected = "rejected"
	// KindNotChecked: the credentials may not get the resource, or may not
	// patch it for the dry-run, so it could not be checked. Not counted in
	// Total: it says nothing about the export.
	KindNotChecked = "not-checked"
)

// Diff is a resource that would not come out of the export unchanged: the
// dry-run apply result differs from the live object, or the resource is not
// in the cluster, its type is not served, the apply was rejected, or the
// export lost its spec or data.
type Diff struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
	// Paths are the first fields that differ, or what went wrong.
	Paths []string `json:"paths,omitempty"`
	// Fields are the differing fields with their values, for KindChanged:
	// Cluster is the live value, Export the value applying the export gives.
	Fields []Field `json:"fields,omitempty"`
	// ChangedAfterExport lists who wrote the live object after the export
	// was taken, from its managedFields: why it no longer matches.
	ChangedAfterExport []Write `json:"changedAfterExport,omitempty"`
	// Explanation is the known cause, e.g. a bare Pod, which can't be
	// updated in place. Empty means unexplained, which fails acceptance.
	Explanation string `json:"explanation,omitempty"`
}

// Field is one differing field of a changed resource; nil where absent.
type Field struct {
	Path    string `json:"path"`
	Cluster any    `json:"cluster,omitempty"`
	Export  any    `json:"export,omitempty"`
	// SetBy are the field managers that own the live value, from its
	// managedFields: who set what the cluster has.
	SetBy []Owner `json:"setBy,omitempty"`
}

// Owner is a field manager owning a field.
type Owner struct {
	Manager     string `json:"manager"`
	Operation   string `json:"operation,omitempty"`
	Subresource string `json:"subresource,omitempty"`
}

// ByHand reports whether the owner is a person's kubectl command (edit,
// patch, set, scale, label, annotate) rather than an apply of manifests.
func (o Owner) ByHand() bool {
	switch {
	case o.Manager == "kubectl-client-side-apply", o.Manager == "kubectl" && o.Operation == "Apply":
		return false
	case o.Manager == "kubectl", strings.HasPrefix(o.Manager, "kubectl-"):
		return true
	}
	return false
}

// Write is a field manager's write to a live object.
type Write struct {
	Manager   string    `json:"manager"`
	Operation string    `json:"operation,omitempty"`
	Time      time.Time `json:"time"`
}

// Report is the result of a verification run.
type Report struct {
	Total int    `json:"total"`
	Clean int    `json:"clean"`
	Diffs []Diff `json:"diffs,omitempty"`
	// Skipped counts resources left out on purpose, by reason, e.g.
	// Secrets under redacted/. They are not in Total.
	Skipped map[string]int `json:"skipped,omitempty"`
	// MissingCRDs are the CustomResourceDefinitions in the export that the
	// cluster lacks: until they exist, their custom resources can't be
	// checked (or applied), and show as types the cluster does not serve.
	MissingCRDs []string `json:"missingCRDs,omitempty"`
	// Threshold is the share of resources, in percent, that must not
	// change; zero means AcceptancePercent.
	Threshold int `json:"threshold"`
	// NotChecked are the resources the credentials may not get, or patch
	// for the dry-run (Kind KindNotChecked): not in Total, and any fails the run.
	NotChecked []Diff `json:"notChecked,omitempty"`
	// Namespaces limits the check to these namespaces (-n); empty is every resource.
	Namespaces []string `json:"namespaces,omitempty"`
}

// RequiredPercent is the threshold the run is held to.
func (r Report) RequiredPercent() int {
	if r.Threshold == 0 {
		return AcceptancePercent
	}
	return r.Threshold
}

// CleanRatio is the share of resources with no diff; 0 when there are none.
func (r Report) CleanRatio() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Clean) / float64(r.Total)
}

// CleanPercent is the share of resources with no diff in percent, rounded
// down to one decimal, so a failing run never prints a passing figure.
func (r Report) CleanPercent() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Clean*1000/r.Total) / 10
}

// MeetsThreshold reports whether enough resources would not change.
func (r Report) MeetsThreshold() bool {
	return r.Total > 0 && r.Clean*100 >= r.Total*r.RequiredPercent()
}

// Unexplained counts the differences with no known explanation.
func (r Report) Unexplained() int {
	n := 0
	for _, d := range r.Diffs {
		if d.Explanation == "" {
			n++
		}
	}
	return n
}

// Passed reports whether the run meets the acceptance test: enough
// resources would not change, and every difference is explained. A run
// that checked nothing does not pass.
func (r Report) Passed() bool {
	return r.MeetsThreshold() && r.Unexplained() == 0 && len(r.NotChecked) == 0
}

// Analysis summarizes a report's differences, to tell their likely cause.
type Analysis struct {
	// ByKind counts the differences by Diff.Kind.
	ByKind map[string]int `json:"byKind"`
	// ChangedAfterExport counts the differing resources written after the
	// export, or whose differing fields a kubectl command set by hand.
	ChangedAfterExport int `json:"changedAfterExport"`
	// Writers counts, by field manager, the resources it wrote after the export.
	Writers map[string]int `json:"writers,omitempty"`
	// CommonField is a field that differs on most changed resources (at
	// least 3, and at least half of them): one cause behind many differences.
	CommonField string `json:"commonField,omitempty"`
	CommonCount int    `json:"commonCount,omitempty"`
}

// Analyze summarizes the report's differences.
func (r Report) Analyze() Analysis {
	a := Analysis{ByKind: map[string]int{}, Writers: map[string]int{}}
	fields := map[string]int{}
	changed := 0
	for _, d := range r.Diffs {
		a.ByKind[d.Kind]++
		seen := map[string]bool{}
		for _, w := range d.ChangedAfterExport {
			seen[w.Manager] = true
		}
		for _, f := range d.Fields {
			for _, o := range f.SetBy {
				if o.ByHand() {
					seen[o.Manager] = true
				}
			}
		}
		if len(seen) > 0 {
			a.ChangedAfterExport++
			for m := range seen {
				a.Writers[m]++
			}
		}
		if d.Kind == KindChanged {
			changed++
			for _, f := range d.Fields {
				fields[f.Path]++
			}
		}
	}
	for path, n := range fields {
		if n >= 3 && n*2 >= changed && (n > a.CommonCount || (n == a.CommonCount && path < a.CommonField)) {
			a.CommonField, a.CommonCount = path, n
		}
	}
	return a
}

// Verifier checks an exported repository against a cluster.
type Verifier interface {
	Verify(ctx context.Context, repoDir string) (*Report, error)
}
