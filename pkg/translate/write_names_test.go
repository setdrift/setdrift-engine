package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// check that an added object whose name isnt a plain file name, as an export read from disk can hold, is refused instead of written outside the overlay
func TestWriteRefusesPathNames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte("resources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "storage.cnrm.cloud.google.com/v1beta1", "kind": "StorageBucket",
		"metadata": map[string]any{"name": "../../escaped", "namespace": "shop"}}}
	r := &Result{Dir: "translations/te2g", Code: "te2g", Added: []*unstructured.Unstructured{bad}}
	err := Write(root, r, nil, "test", RestoreInfo{Cluster: "prod"})
	if err == nil || !strings.Contains(err.Error(), "isn't a valid Kubernetes name") {
		t.Fatalf("got %v, want the name refused", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.yaml")); err == nil {
		t.Fatal("a file was written outside the overlay")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
