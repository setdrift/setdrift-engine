package main

import (
	"bytes"
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

// miniExport writes a small EKS export: one NetworkPolicy the source enforced with Calico, and an ACK Bucket held back in cloud-resources/
func miniExport(t *testing.T, policies bool) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".setdrift-export":                        "cluster: prod\nplatformDir: platform/aws/eks\nnetworkPolicy: Calico\n",
		"kustomization.yaml":                      "resources:\n- namespaces/shop\n",
		"namespaces/shop/kustomization.yaml":      "resources:\n- configmap/settings.yaml\n",
		"namespaces/shop/configmap/settings.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: shop}\n",
		"cloud-resources/namespaces/shop/bucket.s3.services.k8s.aws/acme-orders.yaml": "apiVersion: s3.services.k8s.aws/v1alpha1\nkind: Bucket\nmetadata: {name: acme-orders, namespace: shop}\nspec: {name: acme-orders}\n",
	}
	if policies {
		files["namespaces/shop/kustomization.yaml"] = "resources:\n- configmap/settings.yaml\n- networkpolicy.networking.k8s.io/deny-all.yaml\n"
		files["namespaces/shop/networkpolicy.networking.k8s.io/deny-all.yaml"] = "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: deny-all, namespace: shop}\nspec: {podSelector: {}}\n"
	}
	for f, body := range files {
		path := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runTranslate runs the translate command and gives back its output, its error output and its error
func runTranslate(args ...string) (string, string, error) {
	cmd := newTranslateCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// check a held-back cloud resource stops translate, and the NetworkPolicies flag records accept-unenforced and says so loudly
func TestTranslateHeldAndNetworkPolicies(t *testing.T) {
	root := miniExport(t, true)
	out, _, err := runTranslate(root, "--to", "gke")
	if err == nil || !strings.Contains(out, "cloud-resource/Bucket/shop/acme-orders") {
		t.Fatalf("should stop for the bucket: %v\n%s", err, out)
	}
	// the singular is the same flag
	out, _, err = runTranslate(root, "--to", "gke", "--unmatched", "keep-source", "--accept-unenforced-network-policy")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "WARNING: 1 NetworkPolicy is accepted unenforced") {
		t.Errorf("no warning:\n%s", out)
	}
	ds, _ := os.ReadFile(filepath.Join(root, "translations", "te2g", "translate-decisions.yaml"))
	if !strings.Contains(string(ds), "choose: accept-unenforced") || !strings.Contains(string(ds), "choose: keep-source") {
		t.Errorf("decisions:\n%s", ds)
	}
	restore, _ := os.ReadFile(filepath.Join(root, "translations", "te2g", "RESTORE.md"))
	if !strings.Contains(string(restore), "Turn on network policy enforcement") || !strings.Contains(string(restore), "services.k8s.aws/deletion-policy: retain") {
		t.Errorf("RESTORE.md:\n%s", restore)
	}
}

// check the flag with nothing to accept says so, and an unreadable held-back file is named
func TestTranslateFlagAndHeldErrors(t *testing.T) {
	root := miniExport(t, false)
	_, errOut, err := runTranslate(root, "--to", "gke", "--unmatched", "leave-out", "--accept-unenforced-network-policies")
	if err != nil || !strings.Contains(errOut, "the export has no NetworkPolicies, so there's nothing to accept") {
		t.Errorf("%v %q", err, errOut)
	}
	bad := filepath.Join(root, "cloud-resources", "namespaces", "shop", "broken.yaml")
	if err := os.WriteFile(bad, []byte("kind: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runTranslate(root, "--to", "gke"); err == nil || !strings.Contains(err.Error(), "broken.yaml isn't a readable Kubernetes object") {
		t.Errorf("broken file: %v", err)
	}
}

// check a decision the source changed under gets its own heading, apart from new ones
func TestTranslateReaskHeading(t *testing.T) {
	root := miniExport(t, true)
	if _, _, err := runTranslate(root, "--to", "gke", "--unmatched", "keep-source"); err != nil {
		t.Fatal(err)
	}
	dfile := filepath.Join(root, "translations", "te2g", "translate-decisions.yaml")
	b, _ := os.ReadFile(dfile)
	// the user chose leave-out for the bucket, then the bucket changed at the source
	_ = os.WriteFile(dfile, []byte(strings.Replace(string(b), "choose: keep-source", "choose: leave-out", 1)), 0o600)
	bucket := filepath.Join(root, "cloud-resources", "namespaces", "shop", "bucket.s3.services.k8s.aws", "acme-orders.yaml")
	_ = os.WriteFile(bucket, []byte("apiVersion: s3.services.k8s.aws/v1alpha1\nkind: Bucket\nmetadata: {name: acme-orders, namespace: shop}\nspec: {name: acme-orders-v2}\n"), 0o600)
	out, _, err := runTranslate(root, "--to", "gke")
	if err == nil || !strings.Contains(out, "1 decision changed on the source since you chose, and needs choosing again:") || strings.Contains(out, "no equivalent on GKE") {
		t.Errorf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Earlier you chose to leave out Bucket shop/acme-orders (leave-out)") {
		t.Errorf("%s", out)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
