// tests the small parsing helpers of the package from the inside, plainKind and decodeManifests

package collect

import (
	"bytes"
	"testing"
)

// check that plainKind accepts kubernetes style kind names and refuses digits first, paths, spaces and non-ascii
func TestPlainKind(t *testing.T) {
	for kind, want := range map[string]bool{"Pod": true, "V1Beta1Thing": true, "": false, "1Pod": false, "../x": false, "Pod Spec": false, "Pöd": false} {
		if got := plainKind(kind); got != want {
			t.Errorf("plainKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

// check that decodeManifests, which reads whatever a --from-dir directory holds, may fail but never panics
func FuzzDecodeManifests(f *testing.F) {
	for _, s := range []string{"", "---\n", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", "{\"kind\":\"List\",\"items\":[1]}", "- a\n", "kind: [\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeManifests(bytes.NewReader(b))
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
