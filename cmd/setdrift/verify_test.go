// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// tests for the verify command's own checks and messages; the dry-run, comparison is tested in pkg/verify

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// A DIR that isn't an export is reported as such, even with no usable kubeconfig: the directory is checked first
func TestVerifyChecksDirFirst(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	root := newRootCmd()
	root.SetArgs([]string{"verify", t.TempDir()})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "not an export") {
		t.Errorf("got error %v, want one saying the directory is not an export", err)
	}
}

// The failure message names the part of the acceptance test that was missed
func TestFailReason(t *testing.T) {
	unexplained := verify.Diff{Ref: "a"}
	explained := verify.Diff{Ref: "b", Explanation: "bare Pod"}
	for _, c := range []struct {
		name   string
		report verify.Report
		want   string
	}{
		{"too few clean", verify.Report{Total: 10, Clean: 9, Diffs: []verify.Diff{explained}}, "90.0% would not change, 95% required"},
		{"unexplained", verify.Report{Total: 100, Clean: 99, Diffs: []verify.Diff{unexplained}}, "1 unexplained difference"},
		{"both", verify.Report{Total: 10, Clean: 8, Diffs: []verify.Diff{unexplained, unexplained}}, "80.0% would not change, 95% required; 2 unexplained differences"},
	} {
		if got := failReason(&c.report); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The note appears only when the export's kube-system UID is known and differs
// from the cluster's.
func TestOtherClusterNote(t *testing.T) {
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "kube-system", "uid": "uid-target"}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{namespacesGVR: "NamespaceList"}, ns)
	for _, c := range []struct {
		name, info, want string
	}{
		{"other cluster", "cluster: prod\nproviderId: arn:aws:eks:us-east-2:111122223333:cluster/prod\nkubeSystemUID: uid-source\nexportedAt: \"2026-11-12T00:00:00Z\"\n",
			"this export came from prod (arn:aws:eks:us-east-2:111122223333:cluster/prod), exported 2026-11-12T00:00:00Z, and this is a different cluster"},
		{"same cluster", "cluster: prod\nkubeSystemUID: uid-target\n", ""},
		{"UID unknown (-n)", "cluster: prod\n", ""},
		{"no file (older export)", "", ""},
	} {
		root := t.TempDir()
		if c.info != "" {
			if err := os.WriteFile(filepath.Join(root, kustomize.ExportInfoFile), []byte(c.info), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := otherClusterNote(context.Background(), dyn, root)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
