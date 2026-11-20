// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/diff"
)

// copyCorpus copies a corpus case's input into a new directory named name.
func copyCorpus(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dst, os.DirFS(filepath.Join("..", "..", "testdata", "corpus", "basic", "input"))); err != nil {
		t.Fatal(err)
	}
	return dst
}

func runDiff(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetArgs(append([]string{"diff"}, args...))
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	return out.String(), err
}

func TestDiffDirectories(t *testing.T) {
	a, b := copyCorpus(t, "staging"), copyCorpus(t, "prod")

	out, err := runDiff(t, a, b, "--exit-code")
	if err != nil {
		t.Fatalf("identical inputs: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No differences.") {
		t.Errorf("identical inputs:\n%s", out)
	}

	web := filepath.Join(b, "web.yaml")
	raw, err := os.ReadFile(web)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(strings.Replace(string(raw), "replicas: 3", "replicas: 6", 1), "web:1.4.2", "web:1.5.0", 1)
	if err := os.WriteFile(web, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(b, "secret.yaml")
	raw, _ = os.ReadFile(secret)
	if err := os.WriteFile(secret, []byte(strings.Replace(string(raw), "  username: YXBw", "  username: YXBw\n  password_ro: eA==", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err = runDiff(t, a, b, "--exit-code")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 || ee.err != nil {
		t.Fatalf("--exit-code with differences: err = %v, want exit 1", err)
	}
	for _, want := range []string{
		"Comparing staging with prod",
		"shop/web Deployment.apps  (Argo CD shop-web)",
		"spec.replicas: 3 -> 6",
		`spec.template.spec.containers[web].image: "ghcr.io/example/web:1.4.2" -> "ghcr.io/example/web:1.5.0"`,
		"data.password_ro: (none) -> (redacted)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "aHVudGVyMg") {
		t.Errorf("a secret value reached the output:\n%s", out)
	}

	// without --exit-code, differences are not an error
	if _, err := runDiff(t, a, b); err != nil {
		t.Errorf("without --exit-code: %v", err)
	}

	out, err = runDiff(t, a, b, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var r diff.Result
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if d, _ := r.Counts(); d != 2 || r.A.Label != "staging" {
		t.Errorf("json: %d differ, label %q; want 2, staging", d, r.A.Label)
	}

	// -n limits both sides
	out, err = runDiff(t, a, b, "-n", "nothing-here")
	if err != nil || !strings.Contains(out, "0 in both") {
		t.Errorf("-n: %v\n%s", err, out)
	}
}

func TestDiffSnapshotWithItself(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "before.pb")
	root := newRootCmd()
	root.SetArgs([]string{"export", "--from-dir", filepath.Join("..", "..", "testdata", "corpus", "basic", "input"),
		"--cluster-name", "prod", "-o", filepath.Join(dir, "out"), "--snapshot", pb})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	out, err := runDiff(t, pb, pb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Comparing prod snapshot ") || !strings.Contains(out, "No differences.") {
		t.Errorf("snapshot with itself:\n%s", out)
	}
}

func TestDiffArgs(t *testing.T) {
	a := copyCorpus(t, "a")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{a, a, "-o", "yaml"}, "-o yaml: use text or json"},
		{[]string{a, a, "--map-namespace", "shop"}, "--map-namespace shop: use A_NS=B_NS"},
		{[]string{a, filepath.Join(t.TempDir(), "missing.pb"), "--kubeconfig", filepath.Join(t.TempDir(), "none")}, "is not a file or directory, so it was read as a kubeconfig context"},
	} {
		_, err := runDiff(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	notSnapshot := filepath.Join(t.TempDir(), "x.pb")
	_ = os.WriteFile(notSnapshot, []byte("hello"), 0o644)
	_, err := runDiff(t, a, notSnapshot, "--exit-code")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), "not a snapshot") {
		t.Errorf("bad snapshot with --exit-code: %v, want exit 2", err)
	}
}
