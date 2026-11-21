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

var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// poolLabels name the node group or pool a node belongs to, per platform.
var poolLabels = []string{
	"eks.amazonaws.com/nodegroup",
	"karpenter.sh/nodepool",
	"cloud.google.com/gke-nodepool",
	"kubernetes.azure.com/agentpool",
}

// Nodes summarizes the cluster's nodes: their count and shape, without
// their names. The collector skips nodes as runtime state; their shape is
// what tells two clusters' capacity apart.
func Nodes(ctx context.Context, dyn dynamic.Interface) (*snapshotv1alpha1.NodeSummary, error) {
	list, err := dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return SummarizeNodes(list.Items), nil
}

// SummarizeNodes summarizes nodes as read from the API.
func SummarizeNodes(nodes []unstructured.Unstructured) *snapshotv1alpha1.NodeSummary {
	s := &snapshotv1alpha1.NodeSummary{
		InstanceTypes:   map[string]int32{},
		Zones:           map[string]int32{},
		Platforms:       map[string]int32{},
		KubeletVersions: map[string]int32{},
		Pools:           map[string]int32{},
	}
	cpu, memory := resource.Quantity{}, resource.Quantity{}
	for _, n := range nodes {
		s.Count++
		labels := n.GetLabels()
		count := func(m map[string]int32, key string) {
			if key != "" {
				m[key]++
			}
		}
		count(s.InstanceTypes, labels["node.kubernetes.io/instance-type"])
		count(s.Zones, labels["topology.kubernetes.io/zone"])
		info, _, _ := unstructured.NestedStringMap(n.Object, "status", "nodeInfo")
		if info["operatingSystem"] != "" || info["architecture"] != "" {
			count(s.Platforms, info["operatingSystem"]+"/"+info["architecture"])
		}
		count(s.KubeletVersions, info["kubeletVersion"])
		for _, l := range poolLabels {
			if v := labels[l]; v != "" {
				count(s.Pools, v)
				break
			}
		}
		alloc, _, _ := unstructured.NestedStringMap(n.Object, "status", "allocatable")
		if q, err := resource.ParseQuantity(alloc["cpu"]); err == nil {
			cpu.Add(q)
		}
		if q, err := resource.ParseQuantity(alloc["memory"]); err == nil {
			memory.Add(q)
		}
		if q, err := resource.ParseQuantity(alloc["pods"]); err == nil {
			s.Pods += q.Value()
		}
	}
	if s.Count > 0 {
		s.Cpu, s.Memory = cpu.String(), memory.String()
	}
	return s
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
