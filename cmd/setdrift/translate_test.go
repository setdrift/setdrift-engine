package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
)

func TestTargetPlatformTakesPlatformsAndClouds(t *testing.T) {
	for in, want := range map[string]platform.Distribution{"gke": platform.GKE, "GCP": platform.GKE, " aws ": platform.EKS, "azure": platform.AKS, "AKS": platform.AKS} {
		if got, err := targetPlatform(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"": "--to is required", "null": `not "null"`, "openshift": "eks, aks or gke"} {
		if _, err := targetPlatform(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error containing %q", in, err, want)
		}
	}
}

func TestCheckExportPathSaysWhatItIs(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "prod.pb")
	file := filepath.Join(dir, "notes.txt")
	for _, f := range []string{snap, file} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{filepath.Join(dir, "nope"): "doesn't exist", snap: "is a snapshot", file: "is a file"} {
		if err := checkExportPath(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", path, err, want)
		}
	}
	if err := checkExportPath(dir); err != nil {
		t.Errorf("a directory: %v", err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
