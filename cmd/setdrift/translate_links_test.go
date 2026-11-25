package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// check that translate reads only regular files from cloud-resources/, a link planted there, like in a shared export, could point at a file outside it
func TestHeldObjectsRefusesLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "elsewhere.yaml")
	if err := os.WriteFile(secret, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: elsewhere}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cloud-resources")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "bucket.yaml")); err != nil {
		t.Fatal(err)
	}
	objs, err := heldObjects(root)
	if err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
		t.Fatalf("got %d objects and error %v, want the link refused", len(objs), err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
