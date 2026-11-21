// fuzz tests for the names that become paths, so no cluster, kind or group name can make a path that leaves its directory

package kustomize

import (
	"path/filepath"
	"strings"
	"testing"
)

// one fails the test unless p is a single path element that names something inside its directory
func one(t *testing.T, fn, in, p string) {
	t.Helper()
	if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "/\\") || filepath.Base(p) != p {
		t.Fatalf("%s(%q) = %q: not one path element", fn, in, p)
	}
}

// check that SafeName, KindDir and ClusterDir always give one path element inside their directory, whatever the kind and group
func FuzzPathNames(f *testing.F) {
	for _, s := range []string{"", ".", "..", "../../x", "a/b", "C:\\x", "rbac:role", "...."} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, group, kind string) {
		// SafeName may give back "" or dots, but never a separator
		one(t, "SafeName", kind, "x"+SafeName(kind))
		one(t, "KindDir", kind, KindDir(group+"/v1", kind))
		one(t, "KindDir", kind, KindDir("v1", kind))
		root := filepath.Join("out", "clusters")
		dir := ClusterDir("out", kind)
		if filepath.Dir(dir) != root {
			t.Fatalf("ClusterDir(out, %q) = %q: not inside %s", kind, dir, root)
		}
		one(t, "ClusterDir", kind, filepath.Base(dir))
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
