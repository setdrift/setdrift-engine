package translate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// cluster reads a representative cluster of a platform as its export holds it, from testdata/roundtrip/<distribution>.yaml
// every platform in the capability map needs one, so a new platform is in every direction of the round trip from the start
func cluster(t *testing.T, d platform.Distribution) []*unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "roundtrip", string(d)+".yaml"))
	if err != nil {
		t.Fatalf("%s has no round-trip cluster: add testdata/roundtrip/%s.yaml, a representative cluster of it (%v)", d, d, err)
	}
	var out []*unstructured.Unstructured
	for _, doc := range strings.Split(string(b), "\n---\n") {
		out = append(out, u(t, doc))
	}
	return out
}

// roundTrip translates a platform's cluster to another and back with every default, and gives back each object's differences from where it started, as kind/name: paths
func roundTrip(t *testing.T, home, away platform.Distribution) map[string][]string {
	t.Helper()
	start := cluster(t, home)
	back := translated(t, translated(t, start, home, away), away, home)
	out := map[string][]string{}
	for _, o := range start {
		got := find(back, o.GetKind(), o.GetName())
		key := o.GetKind() + "/" + o.GetName()
		if got == nil {
			out[key] = []string{"(missing)"}
			continue
		}
		for _, f := range diff.Fields(diff.WithoutTranslationMark(jsonOf(o.Object)), diff.WithoutTranslationMark(jsonOf(got.Object))) {
			out[key] = append(out[key], fmt.Sprintf("%s: %v -> %v", f.Path, f.A, f.B))
		}
	}
	return out
}

// jsonOf gives back an object as JSON decodes it, so number types match on both sides
func jsonOf(m map[string]any) any {
	o := &unstructured.Unstructured{Object: m}
	b, _ := o.MarshalJSON()
	back := &unstructured.Unstructured{}
	_ = back.UnmarshalJSON(b)
	return back.Object
}

// every direction, there and back with every default, comes back exactly as it was, ROUNDTRIP=1 prints any difference
func TestRoundTripAllDirections(t *testing.T) {
	var all []platform.Distribution
	for _, p := range load(t).Platforms {
		all = append(all, p.Distribution)
	}
	var report []string
	for _, home := range all {
		for _, away := range all {
			if home == away {
				continue
			}
			got := roundTrip(t, home, away)
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				report = append(report, fmt.Sprintf("%s -> %s -> %s  %s\n      %s", home, away, home, k, strings.Join(got[k], "\n      ")))
				for _, line := range got[k] {
					t.Errorf("%s -> %s -> %s: %s doesn't come back as it was: %s", home, away, home, k, line)
				}
			}
		}
	}
	if os.Getenv("ROUNDTRIP") != "" {
		t.Log("\n" + strings.Join(report, "\n"))
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
