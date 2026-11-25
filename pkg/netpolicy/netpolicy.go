// Package netpolicy tells whether a cluster enforces NetworkPolicies, and with which engine
// a NetworkPolicy applies on any cluster, but only does anything where a network policy engine runs, and whether one runs is a cluster setting
// so a migration from a cluster that enforces to one that doesnt leaves every policy applied and doing nothing, which export and verify look for
package netpolicy

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// the values Enforcement takes besides an engine's name
const (
	// None means the cluster's system workloads were read and no engine enforces policies
	None = "none"
	// Unknown means they couldnt be read, so theres no telling
	Unknown = ""
)

// Namespaces are where the engines run, the ones to read for Detect
var Namespaces = []string{"kube-system", "calico-system"}

// VPCCNIConfigMap is the EKS VPC CNI's settings in kube-system, it switches the policy agent on, and on EKS Auto Mode its the only switch
const VPCCNIConfigMap = "amazon-vpc-cni"

// engines are the DaemonSets of network policy engines, by name, and the engine each one is
// aws-node is the EKS VPC CNI, checked on its own below, since its policy agent can be off
var engines = map[string]string{
	"calico-node":  "Calico",
	"canal":        "Canal",
	"cilium":       "Cilium",
	"anetd":        "GKE Dataplane V2",
	"azure-npm":    "Azure Network Policy Manager",
	"antrea-agent": "Antrea",
	"kube-router":  "kube-router",
	"weave-net":    "Weave Net",
}

// Detect gives back the engine enforcing NetworkPolicies among a cluster's DaemonSets and the VPC CNI's ConfigMap, or None when none does
// objs must hold what Namespaces have, a caller that couldnt read them gives back Unknown itself
func Detect(objs []*unstructured.Unstructured) string {
	var found []string
	for _, o := range objs {
		if !slices.Contains(Namespaces, o.GetNamespace()) {
			continue
		}
		switch o.GetKind() {
		case "DaemonSet":
			name := o.GetName()
			// checking if a live DaemonSet runs no ready Pods, if so its engine enforces nothing yet, like GKE's calico-node before the nodes are recreated
			// a snapshot's DaemonSet has no status, export drops it, so there it counts by being there
			if ready, ok, _ := unstructured.NestedInt64(o.Object, "status", "numberReady"); ok && ready == 0 {
				continue
			}
			// checking if its the VPC CNI, if so its policy agent decides
			if name == "aws-node" {
				if vpcCNIAgentOn(o) {
					found = append(found, "the EKS VPC CNI's network policy agent")
				}
				continue
			}
			// checking if its kube-router with its firewall switched off, if so it routes but doesnt enforce
			if name == "kube-router" && slices.ContainsFunc(args(o), func(a string) bool { return a == "--run-firewall=false" }) {
				continue
			}
			if e := engines[name]; e != "" {
				found = append(found, e)
			}
		case "ConfigMap":
			// EKS Auto Mode and the VPC CNI add-on switch the policy controller on here
			if o.GetName() == VPCCNIConfigMap && o.GetNamespace() == "kube-system" {
				v, _, _ := unstructured.NestedString(o.Object, "data", "enable-network-policy-controller")
				if strings.EqualFold(v, "true") {
					found = append(found, "the EKS VPC CNI's network policy agent")
				}
			}
		}
	}
	if len(found) == 0 {
		return None
	}
	slices.Sort(found)
	return strings.Join(slices.Compact(found), ", ")
}

// vpcCNIAgentOn tells if the aws-node DaemonSet runs its network policy agent switched on
func vpcCNIAgentOn(o *unstructured.Unstructured) bool {
	cs, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		if cm["name"] != "aws-network-policy-agent" {
			continue
		}
		as, _ := cm["args"].([]any)
		for _, a := range as {
			if s, _ := a.(string); s == "--enable-network-policy=true" {
				return true
			}
		}
	}
	return false
}

// args gives back every container's args and command in a DaemonSet's Pod template
func args(o *unstructured.Unstructured) []string {
	var out []string
	cs, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		for _, f := range []string{"command", "args"} {
			as, _ := cm[f].([]any)
			for _, a := range as {
				if s, ok := a.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Enforced tells if a Detect result names an engine
func Enforced(e string) bool { return e != None && e != Unknown }

// HowToEnforce says how a platform switches enforcement on, for the messages and RESTORE.md, by distribution name (eks, aks, gke), empty for another
func HowToEnforce(distribution string) string {
	switch distribution {
	case "eks":
		return "turn on the VPC CNI's network policy agent: on the VPC CNI add-on, set enableNetworkPolicy to \"true\" in its configuration; on EKS Auto Mode, create the ConfigMap amazon-vpc-cni in kube-system with enable-network-policy-controller: \"true\""
	case "aks":
		return "choose a network policy engine for the cluster (az aks create --network-policy cilium, calico or azure; an existing cluster can switch to one with az aks update --network-policy)"
	case "gke":
		return "use GKE Dataplane V2 (gcloud container clusters create --enable-dataplane-v2), or on an existing cluster without it, turn on network policy enforcement (gcloud container clusters update --update-addons=NetworkPolicy=ENABLED, then --enable-network-policy)"
	}
	return "run a network policy engine, like Calico or Cilium"
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
