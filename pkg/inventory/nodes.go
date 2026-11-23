// summarizes the cluster's nodes, their count and shape, for the snapshot

package inventory

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// nodesGVR is how the dynamic client finds nodes, Nodes lists them through it
var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// poolLabels are the labels that name the node group or pool a node belongs to, one per platform
var poolLabels = []string{
	"eks.amazonaws.com/nodegroup",
	"karpenter.sh/nodepool",
	"cloud.google.com/gke-nodepool",
	"kubernetes.azure.com/agentpool",
}

// Nodes lists the cluster's nodes and summarizes them, their count and shape without their names
// the collector skips nodes as runtime state, but their shape is what tells two clusters' capacity apart
func Nodes(ctx context.Context, dyn dynamic.Interface) (*snapshotv1alpha1.NodeSummary, error) {
	// invoking ListNodes to read them, its defined below
	nodes, err := ListNodes(ctx, dyn)
	if err != nil {
		return nil, err
	}
	// invoking SummarizeNodes to add them up, its defined below
	return SummarizeNodes(nodes), nil
}

// ListNodes lists the cluster's nodes as the API gives them, for the summary and the profile, neither of which keeps a node's name
func ListNodes(ctx context.Context, dyn dynamic.Interface) ([]unstructured.Unstructured, error) {
	// listing every node in the cluster
	list, err := dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// SummarizeNodes takes nodes as read from the API and adds them up, counts per instance type, zone, platform, kubelet version and pool, and the total allocatable cpu, memory and pods
func SummarizeNodes(nodes []unstructured.Unstructured) *snapshotv1alpha1.NodeSummary {
	s := &snapshotv1alpha1.NodeSummary{
		InstanceTypes:   map[string]int32{},
		Zones:           map[string]int32{},
		Platforms:       map[string]int32{},
		KubeletVersions: map[string]int32{},
		Pools:           map[string]int32{},
	}
	cpu, memory := resource.Quantity{}, resource.Quantity{}
	// going over each node, counting it and adding up its capacity
	for _, n := range nodes {
		s.Count++
		labels := n.GetLabels()
		// count adds one to key in m, a node missing that label or field isnt counted
		count := func(m map[string]int32, key string) {
			// checking if theres a key to count
			if key != "" {
				m[key]++
			}
		}
		count(s.InstanceTypes, labels["node.kubernetes.io/instance-type"])
		count(s.Zones, labels["topology.kubernetes.io/zone"])
		info, _, _ := unstructured.NestedStringMap(n.Object, "status", "nodeInfo")
		// checking if the node reports its os or architecture, if so its counted as os/architecture
		if info["operatingSystem"] != "" || info["architecture"] != "" {
			count(s.Platforms, info["operatingSystem"]+"/"+info["architecture"])
		}
		count(s.KubeletVersions, info["kubeletVersion"])
		// going over the pool labels, only the first one the node carries counts
		for _, l := range poolLabels {
			// checking if the node carries this label
			if v := labels[l]; v != "" {
				count(s.Pools, v)
				break
			}
		}
		alloc, _, _ := unstructured.NestedStringMap(n.Object, "status", "allocatable")
		// checking if each allocatable quantity parses, one that doesnt is left out of the totals
		if q, err := resource.ParseQuantity(alloc["cpu"]); err == nil {
			cpu.Add(q)
		}
		// checking if the memory parses, if so its added to the total
		if q, err := resource.ParseQuantity(alloc["memory"]); err == nil {
			memory.Add(q)
		}
		// checking if the pod count parses, if so its added to the total
		if q, err := resource.ParseQuantity(alloc["pods"]); err == nil {
			s.Pods += q.Value()
		}
	}
	// checking if there were any nodes, if not cpu and memory stay empty instead of 0
	if s.Count > 0 {
		s.Cpu, s.Memory = cpu.String(), memory.String()
	}
	return s
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
