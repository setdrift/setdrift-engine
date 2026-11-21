// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
)

func runSecrets(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, _, err := runCmd(t, append([]string{"secrets", "export"}, args...)...)
	return out, err
}

// --aws first, then every setting, none taken from the export.
func TestSecretsExportRequiresEverySetting(t *testing.T) {
	if _, err := runSecrets(t, t.TempDir()); err == nil || !strings.Contains(err.Error(), "supports AWS only so far: pass --aws") {
		t.Errorf("no --aws: %v", err)
	}
	_, err := runSecrets(t, "--aws", t.TempDir())
	for _, flag := range []string{"--context", "--region", "--secret-manager", "--secret-key-template", "--store-kind", "--store-name", "--external-secrets-out"} {
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("missing %s not named: %v", flag, err)
		}
	}
	// --check writes no ExternalSecrets, so needs no directory for them
	if _, err := runSecrets(t, "--aws", "--check", t.TempDir()); err == nil || strings.Contains(err.Error(), "--external-secrets-out") {
		t.Errorf("--check: %v", err)
	}
}

// Every check runs and is reported before anything is read or written: a
// bad setting, a folder outside an export, an unknown context.
func TestSecretsExportChecks(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "db.yaml.redacted"), []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: shop}\ndata: {k: x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSecrets(t, "--aws", folder, "--context", "prod", "--region", "us-east-2", "--secret-manager", "vault",
		"--secret-key-template", "{{.Namespace}}/{{.Name}}", "--store-kind", "ClusterSecretStore", "--store-name", "Bad_Name", "--check")
	if err == nil || !strings.Contains(err.Error(), "nothing was read from the Secrets or written") {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"failed   inputs        --secret-manager vault: use aws-secrets-manager or aws-parameter-store",
		"failed   folder        " + folder + " is not in a SetDrift export",
		"failed   source        context prod:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("checks lack %q:\n%s", want, out)
		}
	}
}

// Values come only from the export's source cluster, unless --other-cluster.
func TestSecretsExportChecksSource(t *testing.T) {
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "kube-system", "uid": "uid-other"}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{namespacesGVR: "NamespaceList"}, ns)
	info := &kustomize.ExportInfo{Cluster: "prod", KubeSystemUID: "uid-source"}
	if err := (&secretsExportOptions{}).checkSource(context.Background(), dyn, info); err == nil || !strings.Contains(err.Error(), "not the cluster the export came from") {
		t.Errorf("other cluster: %v", err)
	}
	if err := (&secretsExportOptions{otherCluster: true}).checkSource(context.Background(), dyn, info); err != nil {
		t.Errorf("--other-cluster: %v", err)
	}
	info.KubeSystemUID = "uid-other"
	if err := (&secretsExportOptions{}).checkSource(context.Background(), dyn, info); err != nil {
		t.Errorf("source cluster: %v", err)
	}
}

// exportOf finds the export a redacted folder is in.
func TestExportOf(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, "clusters", "prod")
	if err := os.MkdirAll(filepath.Join(cluster, "redacted", "namespaces", "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cluster, kustomize.ExportInfoFile), []byte("cluster: prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := exportOf(filepath.Join(cluster, "redacted", "namespaces", "shop")); info == nil || info.Cluster != "prod" {
		t.Errorf("exportOf = %+v, want the export of prod", info)
	}
	if info := exportOf(t.TempDir()); info != nil {
		t.Error("a folder outside an export")
	}
}

// --force replaces an earlier run's output only: never a directory of other
// files, nor the home, root or current directory.
func TestReplaceableOutput(t *testing.T) {
	earlier := t.TempDir()
	for _, f := range []string{"kustomization.yaml", "README.md", "secret-store.yaml", "secret-store-shop.yaml", "namespaces/shop/db.yaml"} {
		p := filepath.Join(earlier, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := replaceableOutput(earlier); err != nil {
		t.Errorf("an earlier run's output: %v", err)
	}
	if err := replaceableOutput(filepath.Join(t.TempDir(), "new")); err != nil {
		t.Errorf("a new directory: %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "thesis.docx"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceableOutput(other); err == nil || !strings.Contains(err.Error(), "thesis.docx") {
		t.Errorf("a directory of other files: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := replaceableOutput(home); err == nil {
		t.Error("the home directory")
	}
	if err := replaceableOutput("/"); err == nil {
		t.Error("the root directory")
	}
	t.Chdir(earlier)
	if err := replaceableOutput("."); err == nil {
		t.Error("the current directory")
	}
}
