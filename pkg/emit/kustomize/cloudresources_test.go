package kustomize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// typed builds a test resource of an api group, with a minimal manifest
func typed(apiVersion, kind, ns, name, extra string) *snapshotv1alpha1.Resource {
	return &snapshotv1alpha1.Resource{
		ApiVersion: apiVersion, Kind: kind, Namespace: ns, Name: name,
		ManifestJson: `{"apiVersion":"` + apiVersion + `","kind":"` + kind + `","metadata":{"name":"` + name + `","namespace":"` + ns + `"}` + extra + `}`,
	}
}

// check that objects of operators that create cloud resources are held back in cloud-resources/, in no kustomization, with RESTORE.md moving each one controller by controller
func TestCloudResourcesHeldBack(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{
		Cluster: &snapshotv1alpha1.Cluster{Name: "prod"},
		Resources: []*snapshotv1alpha1.Resource{
			resource("ConfigMap", "shop", "settings", false),
			typed("s3.services.k8s.aws/v1alpha1", "Bucket", "shop", "acme-orders", `,"spec":{"name":"acme-orders"}`),
			typed("storage.cnrm.cloud.google.com/v1beta1", "StorageBucket", "shop", "acme-media", ""),
			typed("s3.aws.upbound.io/v1beta1", "Bucket", "", "acme-logs", ""),
			// AKS's own groups under azure.com aren't cloud resources
			typed("karpenter.azure.com/v1beta1", "AKSNodeClass", "", "default", ""),
		},
	}
	dir := t.TempDir()
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, "prod")
	for _, f := range []string{
		"cloud-resources/README.md",
		"cloud-resources/namespaces/shop/bucket.s3.services.k8s.aws/acme-orders.yaml",
		"cloud-resources/namespaces/shop/storagebucket.storage.cnrm.cloud.google.com/acme-media.yaml",
		"cloud-resources/cluster-scoped/bucket.s3.aws.upbound.io/acme-logs.yaml",
		"cluster-scoped/aksnodeclass.karpenter.azure.com/default.yaml",
	} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	// no kustomization reaches them
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != "kustomization.yaml" {
			return err
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "cloud-resources") || strings.Contains(string(b), "acme-") {
			t.Errorf("%s reaches a held-back object:\n%s", path, b)
		}
		return nil
	})
	b, err := os.ReadFile(filepath.Join(root, "RESTORE.md"))
	if err != nil {
		t.Fatal(err)
	}
	restore := string(b)
	for _, want := range []string{
		"Cloud resources",
		`kubectl --context "$SRC" -n shop annotate bucket.s3.services.k8s.aws/acme-orders services.k8s.aws/deletion-policy=retain --overwrite`,
		`kubectl --context "$SRC" -n shop delete bucket.s3.services.k8s.aws/acme-orders`,
		`kubectl annotate --local -f cloud-resources/namespaces/shop/bucket.s3.services.k8s.aws/acme-orders.yaml services.k8s.aws/adoption-policy=adopt -o yaml | kubectl --context "$DST" apply --server-side -f -`,
		`cnrm.cloud.google.com/deletion-policy=abandon --overwrite`,
		// Crossplane's retain is a spec field, set by a merge patch
		`kubectl --context "$SRC" patch bucket.s3.aws.upbound.io/acme-logs --type merge -p '{"spec":{"deletionPolicy":"Orphan"}}'`,
		"setdrift translate",
	} {
		if !strings.Contains(restore, want) {
			t.Errorf("RESTORE.md is missing %q", want)
		}
	}
	readme, _ := os.ReadFile(filepath.Join(root, "README.md"))
	if !strings.Contains(string(readme), "cloud-resources/ is not included") {
		t.Errorf("README.md doesn't say cloud-resources/ isn't applied:\n%s", readme)
	}
}

// check that the export records the engine enforcing NetworkPolicies on the source, none when kube-system has none, and nothing when it didnt read kube-system
func TestExportInfoNetworkPolicy(t *testing.T) {
	cases := []struct {
		name string
		res  []*snapshotv1alpha1.Resource
		want string
	}{
		{"calico", []*snapshotv1alpha1.Resource{typed("apps/v1", "DaemonSet", "calico-system", "calico-node", ""), resource("ConfigMap", "kube-system", "coredns", false)}, "Calico"},
		{"none", []*snapshotv1alpha1.Resource{typed("apps/v1", "DaemonSet", "kube-system", "kube-proxy", "")}, "none"},
		{"auto mode switch", []*snapshotv1alpha1.Resource{typed("v1", "ConfigMap", "kube-system", "amazon-vpc-cni", `,"data":{"enable-network-policy-controller":"true"}`)}, "the EKS VPC CNI's network policy agent"},
		{"no kube-system", []*snapshotv1alpha1.Resource{resource("ConfigMap", "shop", "settings", false)}, ""},
	}
	for _, c := range cases {
		s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"}, Resources: c.res}
		if got := exportInfo(s).NetworkPolicy; got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
