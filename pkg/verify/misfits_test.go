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

// a ServiceAccount keeping its cloud identity by decision stays clean and is listed with the step outside the cluster, a 10 object run with one passes
// with a foreign finalizer too its still wont work, the finalizer would hang its deletion
func TestMisfitsKeptIdentity(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	sa := func(name string, fin []any) *unstructured.Unstructured {
		md := map[string]any{"name": name, "namespace": "shop", "annotations": map[string]any{"eks.amazonaws.com/role-arn": "arn:aws:iam::111122223333:role/" + name}}
		if fin != nil {
			md["finalizers"] = fin
		}
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": md}}
	}
	kept := func(o *unstructured.Unstructured) string {
		if o.GetName() == "reports" {
			return "keeps its cloud identity, RESTORE.md's step"
		}
		return ""
	}
	objs := []*unstructured.Unstructured{sa("reports", nil), sa("orders", nil)}
	report := &Report{Total: 10, Clean: 10}
	DryRunVerifier{Target: platform.AKS, Capability: m, Kept: kept}.misfits(objs, []*Diff{nil, nil}, report)
	if report.Clean != 9 || len(report.Diffs) != 1 || report.Diffs[0].Ref != Ref(objs[1]) || report.Diffs[0].Kind != KindWontWork {
		t.Fatalf("only the identity not kept wont work: %+v", report)
	}
	if len(report.OutsideCluster) != 1 || report.OutsideCluster[0].Ref != Ref(objs[0]) || report.OutsideCluster[0].Kind != KindOutsideCluster || !strings.Contains(report.OutsideCluster[0].Explanation, "RESTORE.md") {
		t.Fatalf("outside the cluster: %+v", report.OutsideCluster)
	}

	// only the kept one, so the run passes at 100%
	report = &Report{Total: 10, Clean: 10}
	DryRunVerifier{Target: platform.AKS, Capability: m, Kept: kept}.misfits(objs[:1], []*Diff{nil}, report)
	if !report.Passed() || report.Clean != 10 {
		t.Errorf("a kept identity fails the run: %+v", report)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
