package netpolicy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ds builds a DaemonSet with one container and its args
func ds(ns, name, container string, args ...any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": container, "args": args},
		}}}},
	}}
}

// withReady gives a DaemonSet the status of a live one, with n ready Pods
func withReady(o *unstructured.Unstructured, n int64) *unstructured.Unstructured {
	o.Object["status"] = map[string]any{"numberReady": n, "desiredNumberScheduled": n}
	return o
}

// check each engine is recognized, and the ones that can run without enforcing aren't counted then
func TestDetect(t *testing.T) {
	cm := func(v string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"namespace": "kube-system", "name": VPCCNIConfigMap},
			"data":     map[string]any{"enable-network-policy-controller": v}}}
	}
	cases := []struct {
		name string
		objs []*unstructured.Unstructured
		want string
	}{
		{"nothing", nil, None},
		{"kube-proxy only", []*unstructured.Unstructured{ds("kube-system", "kube-proxy", "kube-proxy")}, None},
		{"gke dataplane v2", []*unstructured.Unstructured{ds("kube-system", "anetd", "cilium-agent")}, "GKE Dataplane V2"},
		{"calico operator", []*unstructured.Unstructured{ds("calico-system", "calico-node", "calico-node")}, "Calico"},
		{"azure npm", []*unstructured.Unstructured{ds("kube-system", "azure-npm", "azure-npm")}, "Azure Network Policy Manager"},
		{"vpc cni agent off", []*unstructured.Unstructured{ds("kube-system", "aws-node", "aws-network-policy-agent", "--enable-network-policy=false")}, None},
		{"vpc cni agent on", []*unstructured.Unstructured{ds("kube-system", "aws-node", "aws-network-policy-agent", "--enable-network-policy=true")}, "the EKS VPC CNI's network policy agent"},
		{"auto mode switch on", []*unstructured.Unstructured{cm("true")}, "the EKS VPC CNI's network policy agent"},
		{"auto mode switch off", []*unstructured.Unstructured{cm("false")}, None},
		{"kube-router without its firewall", []*unstructured.Unstructured{ds("kube-system", "kube-router", "kube-router", "--run-firewall=false")}, None},
		{"kube-router", []*unstructured.Unstructured{ds("kube-system", "kube-router", "kube-router", "--run-router=true")}, "kube-router"},
		// an engine's name in an app's namespace isnt the cluster's
		{"other namespace", []*unstructured.Unstructured{ds("shop", "cilium", "cilium")}, None},
		// a live DaemonSet with no ready Pods enforces nothing, like GKE's calico-node before its nodes are recreated
		{"calico not ready", []*unstructured.Unstructured{withReady(ds("kube-system", "calico-node", "calico-node"), 0)}, None},
		{"calico ready", []*unstructured.Unstructured{withReady(ds("kube-system", "calico-node", "calico-node"), 1)}, "Calico"},
		{"two engines", []*unstructured.Unstructured{ds("kube-system", "cilium", "cilium"), ds("calico-system", "calico-node", "calico-node")}, "Calico, Cilium"},
	}
	for _, c := range cases {
		if got := Detect(c.objs); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if Enforced(None) || Enforced(Unknown) || !Enforced("Calico") {
		t.Error("Enforced")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
