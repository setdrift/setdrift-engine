package platform

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

func obj(apiVersion, kind, ns, name string) *resource.Object {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns}}}
	return resource.New(u)
}

func TestDecide(t *testing.T) {
	kubeadmConfig := obj("v1", "ConfigMap", "kube-system", "kubeadm-config")
	for _, c := range []struct {
		name    string
		signals []Signal
		dist    Distribution
		infra   Infrastructure
		managed string // "managed", "self-managed" or ""
		conf    Confidence
		region  string
	}{
		{"EKS from the kubeconfig alone",
			FromKubeconfig(Kubeconfig{Context: "prod", Server: "https://ABC.gr7.us-east-2.eks.amazonaws.com", ExecCmd: "aws", ExecArgs: []string{"eks", "get-token", "--cluster-name", "prod"}}),
			EKS, AWS, "managed", HighConfidence, "us-east-2"},
		{"EKS from the server and nodes (context renamed, endpoint private)",
			append(FromServer("v1.37.1-eks-91f89a9", []string{"apps", "vpcresources.k8s.aws"}),
				FromNodes([]Node{{ProviderID: "aws:///us-east-2a/i-0abc", Labels: map[string]string{"eks.amazonaws.com/compute-type": "auto", "topology.kubernetes.io/region": "us-east-2"}}})...),
			EKS, AWS, "managed", HighConfidence, "us-east-2"},
		{"AKS from its API server address",
			FromKubeconfig(Kubeconfig{Server: "https://prod-dns-abc123.hcp.westeurope.azmk8s.io:443"}),
			AKS, Azure, "managed", HighConfidence, "westeurope"},
		{"kind: context, node provider ID and kubeadm",
			append(append(FromKubeconfig(Kubeconfig{Context: "kind-dev", Server: "https://127.0.0.1:54321"}),
				FromNodes([]Node{{ProviderID: "kind://docker/dev/dev-control-plane"}})...),
				FromObjects([]*resource.Object{kubeadmConfig, obj("apps/v1", "DaemonSet", "kube-system", "kindnet")})...),
			Kind, Local, "self-managed", HighConfidence, ""},
		{"kubeadm on Azure VMs: Azure nodes and disks, no AKS labels",
			append(FromNodes([]Node{{ProviderID: "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm0",
				Labels: map[string]string{"topology.kubernetes.io/region": "eastus"}}}),
				FromObjects([]*resource.Object{kubeadmConfig, obj("storage.k8s.io/v1", "CSIDriver", "", "disk.csi.azure.com")})...),
			Kubeadm, Azure, "self-managed", MediumConfidence, "eastus"},
		{"only a local API server address",
			FromKubeconfig(Kubeconfig{Context: "mine", Server: "https://127.0.0.1:6443"}),
			Unknown, Local, "", NoConfidence, ""},
		{"nothing at all", nil, Unknown, UnknownInfra, "", NoConfidence, ""},
	} {
		got := Decide(c.signals)
		managed := ""
		if got.Managed != nil {
			managed = map[bool]string{true: "managed", false: "self-managed"}[*got.Managed]
		}
		if got.Distribution != c.dist || got.Infrastructure != c.infra || managed != c.managed || got.Confidence != c.conf || got.Region != c.region {
			t.Errorf("%s: %s/%s %s %q %q, want %s/%s %s %q %q\n%v", c.name, got.Distribution, got.Infrastructure, managed, got.Confidence, got.Region,
				c.dist, c.infra, c.managed, c.conf, c.region, got.Evidence)
		}
	}
}

// Strong evidence for two platforms: nothing is acted on.
func TestDecideConflict(t *testing.T) {
	got := Decide(append(FromKubeconfig(Kubeconfig{Context: "arn:aws:eks:us-east-2:111122223333:cluster/prod"}),
		FromNodes([]Node{{ProviderID: "azure:///subscriptions/s/vm", Labels: map[string]string{"kubernetes.azure.com/cluster": "x"}}})...))
	if got.Is(EKS) || got.Is(AKS) || len(got.Conflicts) == 0 || got.Confidence != LowConfidence {
		t.Errorf("%+v", got)
	}
}

func TestFromKubeconfigPatterns(t *testing.T) {
	for _, c := range []struct {
		k    Kubeconfig
		want Distribution
	}{
		{Kubeconfig{Context: "gke_my-project_us-central1_prod"}, GKE},
		{Kubeconfig{ExecCmd: "/usr/local/bin/gke-gcloud-auth-plugin"}, GKE},
		{Kubeconfig{ExecCmd: "kubelogin", ExecArgs: []string{"get-token", "--server-id", "6dae42f8"}}, AKS},
		// int128/kubelogin (generic OIDC) is not AKS
		{Kubeconfig{ExecCmd: "kubelogin", ExecArgs: []string{"get-token", "--oidc-issuer-url", "https://idp"}}, Unknown},
		{Kubeconfig{ExecCmd: `C:\tools\aws.exe`, ExecArgs: []string{"--region", "eu-west-1", "eks", "get-token"}}, EKS},
		{Kubeconfig{Server: "https://api.prod.abcd.p1.openshiftapps.com:6443"}, OpenShift},
		{Kubeconfig{Context: "k3d-dev"}, K3s},
		{Kubeconfig{Context: "docker-desktop"}, DockerDesktop},
	} {
		got := Decide(FromKubeconfig(c.k)).Distribution
		if got != c.want {
			t.Errorf("%+v: %q, want %q", c.k, got, c.want)
		}
	}
}

func TestSummary(t *testing.T) {
	c := Decide(FromNodes([]Node{{ProviderID: "azure:///x", Labels: map[string]string{"topology.kubernetes.io/region": "eastus"}}}))
	c2 := Decide(append(FromNodes([]Node{{ProviderID: "azure:///x"}}), FromObjects([]*resource.Object{obj("v1", "ConfigMap", "kube-system", "kubeadm-config")})...))
	for _, s := range []string{c.Summary(), c2.Summary()} {
		if !strings.Contains(s, "Azure") {
			t.Errorf("summary %q", s)
		}
	}
	if got := c2.Summary(); got != "kubeadm on Azure, medium confidence" {
		t.Errorf("summary %q", got)
	}
}

func TestDir(t *testing.T) {
	kubeadmConfig := obj("v1", "ConfigMap", "kube-system", "kubeadm-config")
	for _, c := range []struct {
		name    string
		signals []Signal
		want    string
	}{
		{"EKS", FromKubeconfig(Kubeconfig{Context: "arn:aws:eks:us-east-2:1:cluster/x"}), "platform/aws/eks"},
		{"AKS", FromKubeconfig(Kubeconfig{Server: "https://x.hcp.eastus.azmk8s.io"}), "platform/azure/aks"},
		{"kubeadm on Azure VMs", append(FromNodes([]Node{{ProviderID: "azure:///vm"}}), FromObjects([]*resource.Object{kubeadmConfig})...), "platform/azure/vm"},
		{"unknown distribution on AWS", FromNodes([]Node{{ProviderID: "aws:///us-east-2a/i-1"}}), "platform/aws/vm"},
		{"kind", FromNodes([]Node{{ProviderID: "kind://docker/x/x"}}), "platform/local/kind"},
		{"k3s, infrastructure unknown", FromServer("v1.35.1+k3s1", nil), "platform/other/k3s"},
		{"k3s on vSphere", append(FromServer("v1.35.1+k3s1", nil), FromNodes([]Node{{ProviderID: "vsphere://x"}})...), "platform/other/k3s"},
		{"only a local API address (weak)", FromKubeconfig(Kubeconfig{Server: "https://127.0.0.1:6443"}), ""},
		{"nothing", nil, ""},
	} {
		if got := Decide(c.signals).Dir(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
