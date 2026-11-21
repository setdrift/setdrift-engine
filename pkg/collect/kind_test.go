package collect

import (
	"bytes"
	"testing"
)

func TestPlainKind(t *testing.T) {
	for kind, want := range map[string]bool{"Pod": true, "V1Beta1Thing": true, "": false, "1Pod": false, "../x": false, "Pod Spec": false, "Pöd": false} {
		if got := plainKind(kind); got != want {
			t.Errorf("plainKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

// decodeManifests reads whatever a --from-dir directory holds: it may fail, never panic.
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
