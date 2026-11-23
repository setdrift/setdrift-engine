// tests reading manifests from a directory, what gets skipped, what gets refused and that errors never print secret values

package collect_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/collect"
)

// writeFiles writes files, keyed by relative path, into a new temp directory and gives back the directory
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// configMap is a minimal ConfigMap manifest the tests read back
const configMap = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: shop}\n"

// check that a repository checkout can be read directly, skipping dot-directories, Kustomization files, runtime state and non-yaml files
func TestFileSourceSkips(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"apps/settings.yaml":            configMap,
		"apps/kustomization.yaml":       "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [settings.yaml]\n",
		".github/workflows/ci.yaml":     "name: ci\non: push\n",
		".git/config.yaml":              "not: kubernetes\n",
		"nodes.yaml":                    "apiVersion: v1\nkind: Node\nmetadata: {name: n1}\n---\napiVersion: v1\nkind: Node\nmetadata: {name: n2}\n",
		"leases.yaml":                   "apiVersion: coordination.k8s.io/v1\nkind: Lease\nmetadata: {name: l, namespace: kube-system}\n",
		"apps/.hidden/ignored.yaml":     "garbage: [",
		"apps/components/comp.yaml":     "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n",
		"apps/settings-backup.yaml.bak": "garbage: [",
	})
	res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].GetName() != "settings" {
		t.Fatalf("got %d objects, want only the ConfigMap", len(res.Objects))
	}
	var msgs []string
	for _, f := range res.Findings {
		msgs = append(msgs, f.Code+": "+f.Message)
	}
	want := []string{
		"RUNTIME_STATE_SKIPPED: 1 Lease object was not exported: the cluster writes this kind itself",
		"RUNTIME_STATE_SKIPPED: 2 Node objects were not exported: the cluster writes this kind itself",
	}
	if strings.Join(msgs, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(msgs, "\n"), strings.Join(want, "\n"))
	}
}

// check that decode errors name the document and never print its content, which may be a Secret
func TestFileSourceErrors(t *testing.T) {
	secretNoKind := "apiVersion: v1\nmetadata: {name: db}\ndata: {password: aHVudGVyMg==}\n"
	for _, tc := range []struct {
		name, content, want string
	}{
		{"missing kind", configMap + "---\n" + secretNoKind, "document 2: object has no kind"},
		{"missing apiVersion", "kind: Secret\nstringData: {password: hunter2}\n", "document 1: object has no apiVersion"},
		{"bad yaml", configMap + "---\nkind: [\n", "document 2:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"in.yaml": tc.content})
			_, err := collect.FileSource{Dir: dir}.Collect(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
			for _, leak := range []string{"hunter2", "aHVudGVyMg=="} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error prints the secret value: %q", err)
				}
			}
		})
	}
}

// check that a Kustomization only listing files in the tree is skipped, with or without kind, and one whose output differs is refused naming why and the command that renders it
func TestFileSourceKustomize(t *testing.T) {
	for _, tc := range []struct{ name, file, kustomization, want string }{
		{"resources only, no kind", "kustomization.yaml", "resources: [settings.yaml]\n", ""},
		{"resources written with ./", "kustomization.yml", "resources: [./settings.yaml]\n", ""},
		{"a file left out", "kustomization.yaml", "resources: []\n", "it leaves out settings.yaml"},
		{"a listed path that isnt the file", "kustomization.yaml", "resources: [prod/settings.yaml]\n", "it leaves out settings.yaml"},
		{"remote base over ssh", "kustomization.yaml", "resources:\n- git@gitlab.com:example/app.git\n", "it pulls in git@"},
		{"remote base with git::", "kustomization.yaml", "resources:\n- git::https://gitlab.com/example/app.git\n", "it pulls in git::"},
		{"patches", "Kustomization", "resources: [settings.yaml]\npatches: [{path: p.yaml}]\n", "it sets patches"},
		{"transformers", "kustomization.yaml", "namespace: prod\nimages: [{name: web, newTag: v2}]\n", "it sets images, namespace"},
		{"remote base", "kustomization.yaml", "resources:\n- https://github.com/example/app//base?ref=v1\n", "it pulls in https://"},
		{"outside the tree", "kustomization.yaml", "resources: [../../base]\n", "outside the directory read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"settings.yaml": configMap, tc.file: tc.kustomization})
			res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
			if tc.want == "" {
				if err != nil || len(res.Objects) != 1 {
					t.Fatalf("got %v, want only the ConfigMap", err)
				}
				return
			}
			var kerr *collect.KustomizeError
			if !errors.As(err, &kerr) || kerr.Dir != dir || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "kubectl kustomize") {
				t.Errorf("got %v, want a KustomizeError with %q", err, tc.want)
			}
		})
	}
}

// check that an export read as manifests leaves out its own snapshots and verify reports, while a snapshots/ directory elsewhere is read
func TestFileSourceSkipsExportRecords(t *testing.T) {
	dir := t.TempDir()
	cm := func(name string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + name + ", namespace: shop}\n"
	}
	for path, content := range map[string]string{
		"clusters/prod/cm.yaml":              cm("in-export"),
		"snapshots/2026-11-20-prod.json":     `{"schemaVersion": "setdrift.snapshot.v1alpha1"}`,
		"verify/2026-11-20-prod-report.json": `{"passed": true}`,
		"apps/snapshots/cm.yaml":             cm("app-snapshots"),
	} {
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range res.Objects {
		names = append(names, o.GetName())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "app-snapshots,in-export" {
		t.Errorf("read %v, want app-snapshots and in-export", names)
	}
}

// check that an export's cluster_profile/ is left out, with its own kustomization too, while a cluster_profile/ outside an export is read
func TestFileSourceSkipsClusterProfile(t *testing.T) {
	dir := t.TempDir()
	cm := func(name string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + name + ", namespace: shop}\n"
	}
	for path, content := range map[string]string{
		"clusters/prod/.setdrift-export":             "{}",
		"clusters/prod/kustomization.yaml":           "resources: [cm.yaml]\n",
		"clusters/prod/cm.yaml":                      cm("in-export"),
		"clusters/prod/cluster_profile/profile.json": `{"network": {"pod_cidrs": ["10.244.0.0/16"]}}`,
		"clusters/prod/cluster_profile/vpc.yaml":     "AWSTemplateFormatVersion: '2010-09-09'\n",
		"apps/cluster_profile/cm.yaml":               cm("not-an-export"),
	} {
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range res.Objects {
		names = append(names, o.GetName())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "in-export,not-an-export" {
		t.Errorf("read %v, want in-export and not-an-export", names)
	}
}

// check that a symlink in the directory isnt followed, since it could lead to a file outside it
func TestFileSourceSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: outside, namespace: shop}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeFiles(t, map[string]string{"in.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: in, namespace: shop}\n"})
	if err := os.Symlink(outside, filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].GetName() != "in" {
		t.Errorf("read %d objects, want only the ConfigMap in the directory", len(res.Objects))
	}
}

// check that a manifest in a subfolder the Kustomization doesnt list is caught, while a subfolder with its own Kustomization is its own unit
func TestFileSourceKustomizeNested(t *testing.T) {
	k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [settings.yaml]\n"
	dir := writeFiles(t, map[string]string{"settings.yaml": configMap, "kustomization.yaml": k, "extra/more.yaml": strings.Replace(configMap, "settings", "more", 1)})
	if _, err := (collect.FileSource{Dir: dir}).Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "it leaves out extra/more.yaml") {
		t.Errorf("unlisted nested manifest: %v", err)
	}
	dir = writeFiles(t, map[string]string{"settings.yaml": configMap, "kustomization.yaml": k,
		"app/more.yaml": strings.Replace(configMap, "settings", "more", 1), "app/kustomization.yaml": strings.Replace(k, "settings.yaml", "more.yaml", 1)})
	if res, err := (collect.FileSource{Dir: dir}).Collect(context.Background()); err != nil || len(res.Objects) != 2 {
		t.Errorf("a subfolder with its own Kustomization: %v", err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
