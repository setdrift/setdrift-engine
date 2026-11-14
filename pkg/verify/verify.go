// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package verify checks an exported repository against the live cluster with a
// server-side dry-run apply. This is the MVP acceptance test: at least 95% of
// resources must show no diff, and every remaining diff must be explained.
package verify

import (
	"context"
)

// AcceptancePercent is the minimum share of resources, in percent, that must
// round-trip with no diff. Passed compares whole numbers, so no float
// rounding decides a run at the threshold.
const AcceptancePercent = 95

// Diff is a resource that would not come out of the export unchanged: the
// dry-run apply result differs from the live object, or the resource is not
// in the cluster, its type is not served, the apply was rejected, or the
// export lost its spec or data.
type Diff struct {
	Ref string
	// Paths are the first fields that differ, or what went wrong.
	Paths []string
	// Explanation is the known cause, e.g. a bare Pod, which can't be
	// updated in place. Empty means unexplained, which fails acceptance.
	Explanation string
}

// Report is the result of a verification run.
type Report struct {
	Total int
	Clean int
	Diffs []Diff
	// Skipped counts resources left out on purpose, by reason, e.g.
	// Secrets under redacted/. They are not in Total.
	Skipped map[string]int
	// MissingCRDs are the CustomResourceDefinitions in the export that the
	// cluster lacks: until they exist, their custom resources can't be
	// checked (or applied), and show as types the cluster does not serve.
	MissingCRDs []string
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

// Passed reports whether the run meets the acceptance test. A run that
// checked nothing does not pass.
func (r Report) Passed() bool {
	if r.Total == 0 || r.Clean*100 < r.Total*AcceptancePercent {
		return false
	}
	for _, d := range r.Diffs {
		if d.Explanation == "" {
			return false
		}
	}
	return true
}

// Verifier checks an exported repository against a cluster.
type Verifier interface {
	Verify(ctx context.Context, repoDir string) (*Report, error)
}
