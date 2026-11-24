package capability

import (
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
	"sigs.k8s.io/yaml"
)

// obj builds an Object from YAML, for the tests
func obj(t *testing.T, group, kind, doc string) Object {
	t.Helper()
	var c map[string]any
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("test object: %v", err)
	}
	return Object{Ref: kind + " " + group, Group: group, Kind: kind, Content: c}
}

func TestLoadParsesEveryPlatform(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := map[platform.Distribution]bool{}
	for _, p := range m.Platforms {
		got[p.Distribution] = true
		if p.Name == "" {
			t.Errorf("%s has no name", p.Distribution)
		}
	}
	for _, d := range []platform.Distribution{platform.EKS, platform.AKS, platform.GKE} {
		if !got[d] {
			t.Errorf("no capability map for %s", d)
		}
	}
}

// the M1 run's misfits (EKS export on GKE): the load balancer, the identity, the secret store, the storage class
func TestMisfitsFindsWhatM1Found(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	objs := []Object{
		obj(t, "", "Service", `
metadata:
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-scheme: internal
spec:
  type: LoadBalancer
  loadBalancerClass: eks.amazonaws.com/nlb`),
		obj(t, "", "ServiceAccount", `
metadata:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::111111111111:role/reports`),
		obj(t, "external-secrets.io", "ClusterSecretStore", `
spec:
  provider:
    aws:
      service: SecretsManager`),
		obj(t, "storage.k8s.io", "StorageClass", `
provisioner: ebs.csi.eks.amazonaws.com
parameters:
  type: gp3`),
		obj(t, "apps", "Deployment", `
spec:
  template:
    spec:
      nodeSelector:
        karpenter.sh/capacity-type: spot
        kubernetes.io/arch: arm64
      containers: [{name: app, image: app}]`),
		obj(t, "", "ConfigMap", `
data:
  a: b`),
	}
	got := m.Misfits(objs, platform.GKE)
	want := map[string]string{
		"Service ":                               "eks.amazonaws.com/nlb belongs to EKS, and on GKE no controller",
		"ServiceAccount ":                        "eks.amazonaws.com/role-arn belongs to EKS",
		"ClusterSecretStore external-secrets.io": "secret store provider aws belongs to EKS",
		"StorageClass storage.k8s.io":            "claims for this class wait forever",
		"Deployment apps":                        "karpenter.sh/capacity-type belongs to AKS and EKS",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d misfits, want %d: %+v", len(got), len(want), got)
	}
	for _, mf := range got {
		w, ok := want[mf.Ref]
		if !ok {
			t.Errorf("unexpected misfit %s: %v", mf.Ref, mf.Reasons)
			continue
		}
		if !strings.Contains(strings.Join(mf.Reasons, "\n"), w) {
			t.Errorf("%s: reasons %q, want one containing %q", mf.Ref, mf.Reasons, w)
		}
	}
	// the internal-scheme annotation is flagged too, since GKE would make the load balancer public
	for _, mf := range got {
		if mf.Ref == "Service " && !strings.Contains(strings.Join(mf.Reasons, "\n"), "aws-load-balancer-scheme") {
			t.Errorf("the AWS scheme annotation wasn't flagged: %v", mf.Reasons)
		}
	}
}

// the same objects fit on EKS, and Karpenter's names fit on AKS too
func TestMisfitsNoneOnTheirOwnPlatform(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	objs := []Object{
		obj(t, "", "Service", `
metadata:
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-scheme: internal
spec:
  loadBalancerClass: eks.amazonaws.com/nlb`),
		obj(t, "karpenter.sh", "NodePool", `
spec: {}`),
	}
	if got := m.Misfits(objs, platform.EKS); len(got) != 0 {
		t.Errorf("on EKS: %+v", got)
	}
	if got := m.Misfits(objs[1:], platform.AKS); len(got) != 0 {
		t.Errorf("Karpenter on AKS: %+v", got)
	}
	if got := m.Misfits(objs, platform.Unknown); len(got) != 0 {
		t.Errorf("unknown target checked anyway: %+v", got)
	}
}

// on a cluster that isn't a cloud's, like kind, every cloud's markers are misfits
func TestMisfitsOnKind(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := m.Misfits([]Object{obj(t, "storage.k8s.io", "StorageClass", `provisioner: pd.csi.storage.gke.io`)}, platform.Kind)
	if len(got) != 1 || !strings.Contains(got[0].Reasons[0], "belongs to GKE, and on kind") {
		t.Errorf("got %+v", got)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
