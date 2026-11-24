package verify

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// an EKS load balancer Service that applied cleanly on GKE becomes a wont-work-here diff, and one that already differs gets the reasons
func TestMisfitsMarkWhatWontWork(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	lb := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web-lb", "namespace": "shop"},
		"spec":     map[string]any{"type": "LoadBalancer", "loadBalancerClass": "eks.amazonaws.com/nlb"},
	}}
	sa := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{"name": "orders", "namespace": "shop", "annotations": map[string]any{"eks.amazonaws.com/role-arn": "arn:aws:iam::111111111111:role/orders"}},
	}}
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "settings", "namespace": "shop"},
	}}
	objs := []*unstructured.Unstructured{lb, sa, cm}
	changed := &Diff{Ref: Ref(sa), Kind: KindChanged, Paths: []string{"metadata.labels"}}
	diffs := []*Diff{nil, changed, nil}
	report := &Report{Total: 3, Clean: 2, Diffs: []Diff{*changed}}

	v := DryRunVerifier{Target: platform.GKE, Capability: m}
	v.misfits(objs, diffs, report)

	if report.Clean != 1 {
		t.Errorf("clean %d, want 1 (the ConfigMap)", report.Clean)
	}
	var lbDiff, saDiff *Diff
	for i := range report.Diffs {
		switch report.Diffs[i].Ref {
		case Ref(lb):
			lbDiff = &report.Diffs[i]
		case Ref(sa):
			saDiff = &report.Diffs[i]
		}
	}
	if lbDiff == nil || lbDiff.Kind != KindWontWork || !strings.Contains(strings.Join(lbDiff.Paths, " "), "eks.amazonaws.com/nlb belongs to EKS") {
		t.Errorf("load balancer: %+v", lbDiff)
	}
	if saDiff == nil || saDiff.Kind != KindChanged || len(saDiff.Paths) != 2 {
		t.Errorf("service account kept its kind and got the reason added: %+v", saDiff)
	}
	if report.Unexplained() == 0 {
		t.Errorf("a wont-work-here diff must count as unexplained")
	}
}

// with no target or no map, nothing changes
func TestMisfitsSkippedWithoutTarget(t *testing.T) {
	m, _ := capability.Load()
	report := &Report{Total: 1, Clean: 1}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"loadBalancerClass": "eks.amazonaws.com/nlb"}}}
	DryRunVerifier{Capability: m}.misfits([]*unstructured.Unstructured{obj}, []*Diff{nil}, report)
	DryRunVerifier{Target: platform.GKE}.misfits([]*unstructured.Unstructured{obj}, []*Diff{nil}, report)
	if report.Clean != 1 || len(report.Diffs) != 0 {
		t.Errorf("checked without a target or map: %+v", report)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
