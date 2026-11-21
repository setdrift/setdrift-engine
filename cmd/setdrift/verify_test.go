// tests for the verify command's own checks and messages; the dry-run, comparison is tested in pkg/verify

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

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
		info, err := kustomize.ReadExportInfo(root)
		if err != nil {
			t.Fatal(err)
		}
		got := otherClusterNote(info, sameCluster(context.Background(), dyn, info))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLikelyCause(t *testing.T) {
	no, yes := false, true
	changed := func(n int, d verify.Diff) []verify.Diff {
		out := make([]verify.Diff, n)
		for i := range out {
			out[i] = d
		}
		return out
	}
	byHand := verify.Diff{Kind: verify.KindChanged, ChangedAfterExport: []verify.Write{{Manager: "kubectl-edit"}}}
	for _, c := range []struct {
		name string
		doc  verifyDocument
		want string
	}{
		{"clean", verifyDocument{}, ""},
		{"other cluster", verifyDocument{SameCluster: &no, ExportCluster: "prod", Report: verify.Report{Diffs: changed(2, byHand)}}, "a different cluster than the export came from (prod)"},
		{"changed since", verifyDocument{SameCluster: &yes, ExportedAt: "T", Report: verify.Report{Diffs: changed(3, byHand)}}, "The cluster changed after the export (T): 3 of the 3 resources were written since, by kubectl-edit (3)"},
		{"one field everywhere", verifyDocument{Report: verify.Report{Diffs: changed(4, verify.Diff{Kind: verify.KindChanged, Fields: []verify.Field{{Path: "metadata.labels.x"}}})}}, "One field differs on 4 of the changed resources: metadata.labels.x"},
		{"gone", verifyDocument{Report: verify.Report{Diffs: changed(2, verify.Diff{Kind: verify.KindNotInCluster})}}, "2 of the 2 are not in the cluster"},
	} {
		c.doc.Analysis = c.doc.Report.Analyze()
		got := likelyCause(c.doc)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVerifyReportFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "verify")
	at := time.Date(2026, 11, 20, 19, 14, 2, 0, time.UTC)
	doc := verifyDocument{Cluster: "arn:aws:eks:us-east-2:111122223333:cluster/prod", Report: verify.Report{Total: 2, Clean: 1, Diffs: []verify.Diff{
		{Ref: "ConfigMap shop/a", Kind: verify.KindChanged, Fields: []verify.Field{{Path: "data.k", Cluster: "x|y", Export: "z"}}}}}}
	md, err := writeVerifyReport(dir, doc, at, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "2026-11-20T191402Z-arn-aws-eks-us-east-2-111122223333-cluster-prod-report.md"); md != want {
		t.Errorf("report = %s, want %s", md, want)
	}
	raw, err := os.ReadFile(strings.TrimSuffix(md, ".md") + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var back verifyDocument
	if err := json.Unmarshal(raw, &back); err != nil || back.Report.Diffs[0].Fields[0].Cluster != "x|y" {
		t.Errorf("json: %v %+v", err, back)
	}
	text, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if goruntime.GOOS != "windows" {
		for _, f := range []string{md, strings.TrimSuffix(md, ".md") + ".json"} {
			if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, %v; want 0600", filepath.Base(f), fi.Mode().Perm(), err)
			}
		}
	}
	if gi, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err != nil || !strings.Contains(string(gi), "*") {
		t.Errorf("verify/.gitignore = %q, %v; want the reports ignored", gi, err)
	}
	if !strings.Contains(string(text), "| `data.k` | `\"x\\|y\"` | `\"z\"` |") {
		t.Errorf("markdown table:\n%s", text)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
