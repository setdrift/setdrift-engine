// tests the diff command end to end, on directories of manifests, snapshots over time, namespace mapping and argument checks

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/diff"
)

// copyCorpus copies the corpus input into a new temp directory with the given name, so a test can change it and name its side
func copyCorpus(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dst, os.DirFS(corpusInput)); err != nil {
		t.Fatal(err)
	}
	return dst
}

// runDiff runs setdrift diff with the given arguments and gives back what it printed and its error
func runDiff(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, _, err := runCmd(t, append([]string{"diff"}, args...)...)
	return out, err
}

// check that two directories of manifests diff clean when identical, and show changed fields, redacted secrets and the --exit-code status when not
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
	if raw, err = os.ReadFile(secret); err != nil {
		t.Fatal(err)
	}
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

	// without --exit-code differences arent an error
	if _, err := runDiff(t, a, b); err != nil {
		t.Errorf("without --exit-code: %v", err)
	}

	out, err = runDiff(t, a, b, "--format", "json")
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

// check that two snapshots of the same cluster are labeled by when they were taken and show no differences
func TestDiffSnapshotOverTime(t *testing.T) {
	dir := t.TempDir()
	export := func(pb string) {
		root := newRootCmd()
		root.SetArgs([]string{"export", "--from-dir", corpusInput,
			"--cluster-name", "prod", "-o", filepath.Join(dir, filepath.Base(pb)+".out"), "--snapshot", pb})
		root.SetOut(&bytes.Buffer{})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	before, after := filepath.Join(dir, "before.pb"), filepath.Join(dir, "after.pb")
	export(before)
	export(after)
	out, err := runDiff(t, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Comparing prod snapshot ") || !strings.Contains(out, "No differences.") {
		t.Errorf("one cluster at two times:\n%s", out)
	}
}

// check that comparing a source with itself needs --map-namespace, and then each side reads only its mapped namespace
func TestDiffNamespacesOfOneCluster(t *testing.T) {
	a := copyCorpus(t, "c")
	// a second namespace, prod, holding only a ConfigMap shop doesnt have
	extra := "---\napiVersion: v1\nkind: Namespace\nmetadata: {name: prod}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: only-prod, namespace: prod}\ndata: {x: \"1\"}\n"
	if err := os.WriteFile(filepath.Join(a, "prod.yaml"), []byte(extra), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runDiff(t, a, a); err == nil || !strings.Contains(err.Error(), "with itself: to compare two of its namespaces") {
		t.Errorf("itself without a map: %v", err)
	}
	out, err := runDiff(t, a, a, "--map-namespace", "shop=prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Comparing c (shop) with c (prod)", "Only in c (prod) (1):", "prod, applied by hand: ConfigMap only-prod"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// check that bad flags, missing files, unknown contexts and an unreadable snapshot fail with a clear message, and exit 2 under --exit-code
func TestDiffArgs(t *testing.T) {
	a := copyCorpus(t, "a")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{a, a, "--format", "yaml"}, "--format yaml: use text or json"},
		{[]string{a, a, "-o", "json"}, "unknown shorthand flag: 'o'"},
		{[]string{a, a, "--map-namespace", "shop"}, "--map-namespace shop: use A_NS=B_NS"},
		{[]string{a, filepath.Join(t.TempDir(), "missing.pb")}, "missing.pb: no such file or directory"},
		{[]string{a, "prod", "--kubeconfig", filepath.Join(t.TempDir(), "none")}, "prod is neither a file or directory here nor a kubeconfig context: --kubeconfig"},
	} {
		_, err := runDiff(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	notSnapshot := filepath.Join(t.TempDir(), "x.pb")
	if err := os.WriteFile(notSnapshot, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runDiff(t, a, notSnapshot, "--exit-code")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), "not a snapshot") {
		t.Errorf("bad snapshot with --exit-code: %v, want exit 2", err)
	}
}

// check that both arguments are checked before either is read, a json snapshot reads like its .pb, a mapped namespace must exist on its side, and an unreadable file says permission denied
func TestDiffArgumentChecks(t *testing.T) {
	dir := t.TempDir()
	_, err := runDiff(t, filepath.Join(dir, "foo.pb"), filepath.Join(dir, "prod.json"))
	if err == nil || !strings.Contains(err.Error(), "foo.pb: no such file") || !strings.Contains(err.Error(), "prod.json: no such file") {
		t.Errorf("both missing: %v", err)
	}

	root := newRootCmd()
	out := filepath.Join(dir, "out")
	root.SetArgs([]string{"export", "--from-dir", corpusInput, "--cluster-name", "prod", "-o", out})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	pb, err := filepath.Glob(filepath.Join(out, "snapshots", "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := filepath.Glob(filepath.Join(out, "snapshots", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb) != 1 || len(js) != 1 {
		t.Fatalf("snapshots: %v %v", pb, js)
	}
	text, err := runDiff(t, pb[0], js[0])
	if err != nil || !strings.Contains(text, "No differences.") {
		t.Errorf(".pb against its .json: %v\n%s", err, text)
	}

	_, err = runDiff(t, pb[0], js[0], "--map-namespace", "shpo=shop")
	if err == nil || !strings.Contains(err.Error(), "has no namespace shpo; its namespaces are shop") {
		t.Errorf("mapping a namespace the side lacks: %v", err)
	}

	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		locked := filepath.Join(dir, "locked.pb")
		if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := runDiff(t, locked, pb[0]); err == nil || !strings.Contains(err.Error(), "can't read it (permission denied)") {
			t.Errorf("unreadable: %v", err)
		}
	}
}

// check that a bare name thats both a directory and a kubeconfig context is an error, while ./name is the directory and context:name the context
func TestDiffResolveAmbiguity(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: x}}]
contexts: [{name: prod, context: {cluster: c, user: u}}, {name: "arn:aws:eks:us-east-2:111122223333:cluster/prod", context: {cluster: c, user: u}}]
current-context: prod
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	o := &diffOptions{kubeconfig: kubeconfig}

	if _, err := o.resolve("prod"); err == nil || !strings.Contains(err.Error(),
		"prod is both a directory here and a kubeconfig context: write ./prod for the directory, or context:prod for the cluster") {
		t.Errorf("ambiguous: %v", err)
	}
	for _, arg := range []string{"./prod", "prod/", filepath.Join(dir, "prod")} {
		if sd, err := o.resolve(arg); err != nil || !sd.dir || sd.context != "" {
			t.Errorf("%s: %+v %v, want the directory", arg, sd, err)
		}
	}
	if sd, err := o.resolve("staging"); err != nil || !sd.dir {
		t.Errorf("a directory only: %+v %v", sd, err)
	}
	if sd, err := o.resolve("context:prod"); err != nil || sd.context != "prod" || sd.path != "" {
		t.Errorf("context:prod: %+v %v", sd, err)
	}
	if sd, err := o.resolve("context:arn:aws:eks:us-east-2:111122223333:cluster/prod"); err != nil ||
		sd.context != "arn:aws:eks:us-east-2:111122223333:cluster/prod" {
		t.Errorf("an ARN context: %+v %v", sd, err)
	}
	if _, err := o.resolve("context:"); err == nil || !strings.Contains(err.Error(), "name the context") {
		t.Errorf("empty context: %v", err)
	}
	if _, err := o.resolve("context:nope"); err == nil || !strings.Contains(err.Error(), `no context "nope"`) {
		t.Errorf("unknown context: %v", err)
	}
	a, _ := o.resolve("./prod")
	b, _ := o.resolve(filepath.Join(dir, "prod"))
	c, _ := o.resolve("context:prod")
	if !a.same(b) || a.same(c) || !c.same(c) {
		t.Error("same: the same directory by two paths is one side; a directory and a context are not")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
