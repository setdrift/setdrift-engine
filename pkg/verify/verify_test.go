package verify

import "testing"

func TestPassed(t *testing.T) {
	explained := Diff{Ref: "Pod a/p", Explanation: "bare Pod"}
	for _, c := range []struct {
		name  string
		r     Report
		want  bool
		print float64
	}{
		{"nothing checked", Report{}, false, 0},
		{"all clean", Report{Total: 3, Clean: 3}, true, 100},
		{"exactly 95%", Report{Total: 20, Clean: 19, Diffs: []Diff{explained}}, true, 95},
		{"just under 95% prints under 95", Report{Total: 2000, Clean: 1899, Diffs: make([]Diff, 101)}, false, 94.9},
		{"unexplained diff", Report{Total: 100, Clean: 99, Diffs: []Diff{{Ref: "ConfigMap a/x"}}}, false, 99},
		{"explained diffs still count against the ratio", Report{Total: 10, Clean: 9, Diffs: []Diff{explained}}, false, 90},
	} {
		for i := range c.r.Diffs {
			if c.r.Diffs[i].Ref == "" {
				c.r.Diffs[i] = explained
			}
		}
		if got := c.r.Passed(); got != c.want {
			t.Errorf("%s: Passed() = %v", c.name, got)
		}
		if got := c.r.CleanPercent(); got != c.print {
			t.Errorf("%s: CleanPercent() = %v, want %v", c.name, got, c.print)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
