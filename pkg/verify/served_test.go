package verify

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// a StorageClass of a cloud driver the cluster doesn't run, and an Ingress of a class nowhere to be found, wont work, a local driver and a class the export brings are fine
func TestUnserved(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	obj := func(apiVersion, kind, name string, extra map[string]any) *unstructured.Unstructured {
		o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "shop"}}
		for k, v := range extra {
			o[k] = v
		}
		return &unstructured.Unstructured{Object: o}
	}
	auto := obj("storage.k8s.io/v1", "StorageClass", "gp3", map[string]any{"provisioner": "ebs.csi.eks.amazonaws.com"})
	local := obj("storage.k8s.io/v1", "StorageClass", "local", map[string]any{"provisioner": "rancher.io/local-path"})
	intree := obj("storage.k8s.io/v1", "StorageClass", "gp2", map[string]any{"provisioner": "kubernetes.io/aws-ebs"})
	missing := obj("networking.k8s.io/v1", "Ingress", "web", map[string]any{"spec": map[string]any{"ingressClassName": "alb"}})
	brought := obj("networking.k8s.io/v1", "Ingress", "api", map[string]any{"spec": map[string]any{"ingressClassName": "internal"}})
	class := obj("networking.k8s.io/v1", "IngressClass", "internal", nil)
	objs := []*unstructured.Unstructured{auto, local, intree, missing, brought, class}

	v := DryRunVerifier{Target: platform.EKS, Capability: m, Served: &Served{
		CSIDrivers:     map[string]bool{"ebs.csi.aws.com": true},
		IngressClasses: map[string]bool{"nginx": true},
	}}
	got := v.unserved(objs)
	if len(got) != 2 || got[0].Ref != Ref(auto) || got[1].Ref != Ref(missing) {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got[0].Reasons[0], "runs the EBS CSI add-on (ebs.csi.aws.com), not EKS Auto Mode") {
		t.Errorf("the EBS variant isn't named: %s", got[0].Reasons[0])
	}
	// a list it couldn't read skips that part of the check
	v.Served = &Served{}
	if got := v.unserved(objs); len(got) != 0 {
		t.Errorf("unreadable lists: %+v", got)
	}
	// and through misfits, a clean object becomes a wont-work diff
	report := &Report{Total: len(objs), Clean: len(objs)}
	v.Served = &Served{CSIDrivers: map[string]bool{"ebs.csi.aws.com": true}}
	v.misfits(objs, make([]*Diff, len(objs)), report)
	if len(report.Diffs) != 1 || report.Diffs[0].Kind != KindWontWork || report.Clean != len(objs)-1 {
		t.Errorf("%+v", report)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
