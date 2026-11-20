// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"encoding/json"
	"testing"
)

func TestOwns(t *testing.T) {
	var f map[string]any
	if err := json.Unmarshal([]byte(`{"f:spec":{"f:replicas":{},"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{"f:image":{}}},"f:args":{}}}}}`), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		keys []string
		want bool
	}{
		{[]string{"f:spec", "f:replicas"}, true},
		{[]string{"f:spec", "f:template", "f:spec", "f:containers", "n:web", "f:image"}, true},
		{[]string{"f:spec", "f:template", "f:spec", "f:containers", "n:api", "f:image"}, false},
		// an atomic list: owned as a whole
		{[]string{"f:spec", "f:template", "f:spec", "f:args", "i:0"}, true},
		{[]string{"f:metadata", "f:labels"}, false},
	} {
		if got := owns(f, c.keys); got != c.want {
			t.Errorf("owns(%v) = %v, want %v", c.keys, got, c.want)
		}
	}
}

func TestThreshold(t *testing.T) {
	r := Report{Total: 20, Clean: 18}
	if r.MeetsThreshold() || r.RequiredPercent() != 95 {
		t.Errorf("default: 90%% meets 95%%? %v (required %d)", r.MeetsThreshold(), r.RequiredPercent())
	}
	r.Threshold = 90
	if !r.MeetsThreshold() || !r.Passed() {
		t.Error("90% should meet --threshold 90")
	}
	r.Diffs = []Diff{{Ref: "x", Kind: KindChanged}}
	if r.Passed() {
		t.Error("an unexplained difference passes")
	}
	r = Report{Total: 20, Clean: 19, Threshold: 100}
	if r.MeetsThreshold() {
		t.Error("19 of 20 meets --threshold 100")
	}
	if (Report{Threshold: 50}).Passed() {
		t.Error("a run that checked nothing passes")
	}
}

func TestAnalyze(t *testing.T) {
	hand := []Owner{{Manager: "kubectl-edit", Operation: "Update"}}
	applied := []Owner{{Manager: "kubectl-client-side-apply", Operation: "Update"}}
	r := Report{Diffs: []Diff{
		{Kind: KindChanged, Fields: []Field{{Path: "spec.replicas", SetBy: hand}, {Path: "metadata.annotations.mesh"}}},
		{Kind: KindChanged, Fields: []Field{{Path: "metadata.annotations.mesh", SetBy: applied}}},
		{Kind: KindChanged, Fields: []Field{{Path: "metadata.annotations.mesh"}}},
		{Kind: KindNotInCluster},
	}}
	a := r.Analyze()
	if a.ByKind[KindChanged] != 3 || a.ByKind[KindNotInCluster] != 1 {
		t.Errorf("by kind = %v", a.ByKind)
	}
	if a.ChangedAfterExport != 1 || a.Writers["kubectl-edit"] != 1 {
		t.Errorf("changed by hand = %d %v; an apply is not by hand", a.ChangedAfterExport, a.Writers)
	}
	if a.CommonField != "metadata.annotations.mesh" || a.CommonCount != 3 {
		t.Errorf("common field = %q x%d", a.CommonField, a.CommonCount)
	}
	if (Owner{Manager: "kubectl", Operation: "Apply"}).ByHand() || !(Owner{Manager: "kubectl", Subresource: "scale", Operation: "Update"}).ByHand() {
		t.Error("server-side apply is not by hand; kubectl scale is")
	}
}
