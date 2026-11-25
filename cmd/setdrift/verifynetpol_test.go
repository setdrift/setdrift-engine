// tests verify's network policy check, the engines themselves are tested in pkg/netpolicy

package main

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"setdrift.io/setdrift-engine/pkg/netpolicy"
)

// check what passes, warns and fails, by what the source and this cluster enforce
func TestCheckNetworkPolicies(t *testing.T) {
	cases := []struct {
		name                     string
		policies                 int
		source, target, accepted string
		fail, warn, none         bool
		says                     string
	}{
		{name: "no policies", policies: 0, source: "Calico", target: netpolicy.None, none: true},
		{name: "source didn't enforce", policies: 3, source: netpolicy.None, target: netpolicy.None, none: true},
		{name: "both enforce", policies: 3, source: "Calico", target: "Cilium", says: "3 NetworkPolicies enforced here by Cilium"},
		{name: "lost enforcement", policies: 3, source: "GKE Dataplane V2", target: netpolicy.None, fail: true, says: "turn on the VPC CNI's network policy agent"},
		{name: "can't tell", policies: 1, source: "Calico", target: netpolicy.Unknown, fail: true, says: "verify can't tell if this cluster enforces them: no access"},
		{name: "accepted", policies: 3, source: "Calico", target: netpolicy.None, accepted: "--accept-unenforced-network-policies", warn: true, says: "Accepted unenforced (--accept-unenforced-network-policies)"},
		{name: "older export", policies: 2, source: netpolicy.Unknown, target: netpolicy.None, warn: true, says: "export again with this version"},
	}
	for _, c := range cases {
		got := checkNetworkPolicies(c.policies, c.source, c.target, "no access", c.accepted, "eks")
		if c.none {
			if got != nil {
				t.Errorf("%s: %+v", c.name, got)
			}
			continue
		}
		if got == nil || got.Failed != c.fail || got.Warning != c.warn || !strings.Contains(got.Message, c.says) {
			t.Errorf("%s: %+v", c.name, got)
		}
		if c.fail && !strings.Contains(got.Message, "--accept-unenforced-network-policies") {
			t.Errorf("%s: a failure must say how to accept it on purpose: %s", c.name, got.Message)
		}
	}
}

// check this cluster's engine is read from its DaemonSets, and a credential that can't list them leaves it unknown, saying what's missing
func TestTargetNetworkPolicy(t *testing.T) {
	ds := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"namespace": "kube-system", "name": "anetd"}}}
	lists := map[schema.GroupVersionResource]string{daemonSetsGVR: "DaemonSetList", configMapsGVR: "ConfigMapList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, ds)
	if e, why := targetNetworkPolicy(context.Background(), dyn); e != "GKE Dataplane V2" || why != "" {
		t.Errorf("%q %q", e, why)
	}
	empty := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	if e, _ := targetNetworkPolicy(context.Background(), empty); e != netpolicy.None {
		t.Errorf("no engine: %q", e)
	}
	locked := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	locked.PrependReactor("list", "daemonsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, "", nil)
	})
	if e, why := targetNetworkPolicy(context.Background(), locked); e != netpolicy.Unknown || !strings.Contains(why, "list on daemonsets.apps") {
		t.Errorf("forbidden: %q %q", e, why)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
