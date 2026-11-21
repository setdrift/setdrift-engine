// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteExternalSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "es")
	x := &ExternalSecrets{StoreKind: "SecretStore", StoreName: "params", Provider: "AWS Systems Manager Parameter Store", Service: "ParameterStore"}
	err := WriteExternalSecrets(dir, x, "us-east-2", []SecretSource{
		{Namespace: "shop", Name: "db", Type: "Opaque", Keys: []string{"password"}, RemoteKey: "/prod/shop/db"},
		{Namespace: "argocd", Name: "argocd-secret", Keys: []string{"oidc"}, RemoteKey: "/prod/argocd/argocd-secret", Merge: true},
		{Namespace: "shop", Name: "keystore", Type: "kubernetes.io/tls", Keys: []string{"jks"}, RemoteKey: "/prod/shop/keystore", Binary: []string{"jks"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	k := read("kustomization.yaml")
	for _, f := range []string{"secret-store-argocd.yaml", "secret-store-shop.yaml", "namespaces/shop/db.yaml", "namespaces/argocd/argocd-secret.yaml"} {
		if !strings.Contains(k, f) {
			t.Errorf("kustomization lacks %s:\n%s", f, k)
		}
	}
	if s := read("secret-store-shop.yaml"); !strings.Contains(s, "kind: SecretStore") || !strings.Contains(s, "namespace: shop") || !strings.Contains(s, "service: ParameterStore") {
		t.Errorf("store:\n%s", s)
	}
	if m := read("namespaces/argocd/argocd-secret.yaml"); !strings.Contains(m, "creationPolicy: Merge") || !strings.Contains(m, "key: /prod/argocd/argocd-secret") {
		t.Errorf("merge:\n%s", m)
	}
	if m := read("namespaces/shop/keystore.yaml"); !strings.Contains(m, "decodingStrategy: Base64") || !strings.Contains(m, "type: kubernetes.io/tls") {
		t.Errorf("binary:\n%s", m)
	}
	if r := read("README.md"); !strings.Contains(r, "argocd/argocd-secret (merge)") || !strings.Contains(r, "kubectl apply -k .") {
		t.Errorf("README:\n%s", r)
	}
}
