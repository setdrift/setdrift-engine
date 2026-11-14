// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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
)

// An export whose ExternalSecrets read from a generic store needs to be told
// which secret manager to write to; that is checked before any cluster read.
func TestSecretsExportNeedsAManager(t *testing.T) {
	root := t.TempDir()
	es := filepath.Join(root, "external-secrets", "namespaces", "shop", "externalsecret.external-secrets.io")
	if err := os.MkdirAll(es, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join(root, "kustomization.yaml"):     "resources: []\n",
		filepath.Join(root, kustomize.ExportInfoFile): "cluster: prod\nsecretManager: generic\n",
		filepath.Join(es, "db.yaml"): "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata: {name: db, namespace: shop}\n" +
			"spec: {target: {name: db}, data: [{secretKey: password, remoteRef: {key: prod/shop/db, property: password}}]}\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	cmd := newRootCmd()
	cmd.SetArgs([]string{"secrets", "export", root})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--secret-manager aws-secrets-manager") {
		t.Errorf("got %v", err)
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
