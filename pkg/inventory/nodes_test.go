// tests the node summary, the counts and the allocatable totals

package inventory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// node builds a node with the given instance type, zone, node group and allocatable cpu and memory
func node(instanceType, zone, pool, cpu, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "n", "labels": map[string]any{
			"node.kubernetes.io/instance-type": instanceType, "topology.kubernetes.io/zone": zone, "eks.amazonaws.com/nodegroup": pool}},
		"status": map[string]any{
			"allocatable": map[string]any{"cpu": cpu, "memory": memory, "pods": "29"},
			"nodeInfo":    map[string]any{"operatingSystem": "linux", "architecture": "arm64", "kubeletVersion": "v1.37.0-eks"},
		},
	}}
}

// check that nodes are counted per type, zone, pool, platform and kubelet version, their allocatable adds up, and no nodes leave cpu empty
func TestSummarizeNodes(t *testing.T) {
	s := SummarizeNodes([]unstructured.Unstructured{
		node("m7g.large", "us-east-1a", "general", "1930m", "7Gi"),
		node("m7g.large", "us-east-1b", "general", "1930m", "7Gi"),
		node("c7g.xlarge", "us-east-1a", "compute", "3920m", "6Gi"),
	})
	if s.Count != 3 || s.InstanceTypes["m7g.large"] != 2 || s.Zones["us-east-1a"] != 2 || s.Pools["compute"] != 1 ||
		s.Platforms["linux/arm64"] != 3 || s.KubeletVersions["v1.37.0-eks"] != 3 {
		t.Errorf("summary = %+v", s)
	}
	if s.Cpu != "7780m" || s.Memory != "20Gi" || s.Pods != 87 {
		t.Errorf("allocatable = %s %s %d", s.Cpu, s.Memory, s.Pods)
	}
	if e := SummarizeNodes(nil); e.Count != 0 || e.Cpu != "" {
		t.Errorf("no nodes = %+v", e)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
