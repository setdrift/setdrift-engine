// tests the ownership stage and the rules for objects the cluster generates

package ownership

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// obj builds a core Object of the given kind, name and uid in namespace shop, with owner as its controller when given
func obj(kind, name, uid string, owner *metav1.OwnerReference) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind(kind)
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetUID(types.UID(uid))
	if owner != nil {
		yes := true
		owner.Controller = &yes
		u.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	return resource.New(u)
}

// check that children of collected objects, garbage and generated objects are dropped, and an object whose owner wasnt collected is kept with a warning
func TestProcess(t *testing.T) {
	parent := obj("ConfigMap", "parent", "p", nil)
	child := obj("ConfigMap", "child", "c", &metav1.OwnerReference{Kind: "ConfigMap", Name: "parent", UID: "p"})
	garbage := obj("Pod", "garbage", "g", &metav1.OwnerReference{Kind: "Job", Name: "gone", UID: "x"})
	garbage.OwnerStatus = resource.OwnerDeleted
	notCollected := obj("Pod", "w", "w", &metav1.OwnerReference{Kind: "Widget", Name: "w", UID: "y"})
	notCollected.OwnerStatus = resource.OwnerNotCollected
	rootCA := obj("ConfigMap", "kube-root-ca.crt", "r", nil)

	out, findings, err := ControllerOwnerResolver{}.Process(context.Background(),
		[]*resource.Object{parent, child, garbage, notCollected, rootCA})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != parent || out[1] != notCollected {
		t.Fatalf("kept %d objects, want parent and the one with an uncollected owner", len(out))
	}
	if len(notCollected.Findings) != 1 || notCollected.Findings[0].Code != "OWNER_NOT_EXPORTED" {
		t.Errorf("findings %v", notCollected.Findings)
	}
	codes := map[string]bool{}
	for _, f := range findings {
		codes[f.Code] = true
	}
	if !codes["OWNER_DELETED_SKIPPED"] || !codes["GENERATED_SKIPPED"] {
		t.Errorf("capture findings %v", findings)
	}
}

// check that a default ServiceAccount counts as generated only until someone adds pull secrets to it
func TestUntouchedDefaultServiceAccount(t *testing.T) {
	sa := obj("ServiceAccount", "default", "s", nil)
	if generatedBy(sa, index{}) == "" {
		t.Error("untouched default ServiceAccount not recognised")
	}
	sa.Object["imagePullSecrets"] = []any{map[string]any{"name": "ecr"}}
	if generatedBy(sa, index{}) != "" {
		t.Error("default ServiceAccount with pull secrets is configuration")
	}
}

// check that only claims named after a claim template, the StatefulSet and a numeric ordinal count as generated
func TestStatefulSetClaims(t *testing.T) {
	idx := newIndex([]*resource.Object{resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": "web-db", "namespace": "shop"},
		"spec":     map[string]any{"volumeClaimTemplates": []any{map[string]any{"metadata": map[string]any{"name": "data"}}}},
	}})})
	for name, want := range map[string]bool{"data-web-db-0": true, "data-web-db-12": true, "data-web-db-": false, "data-web-db-a": false, "data-web-0": false} {
		pvc := resource.New(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": name, "namespace": "shop"}}})
		if got := generatedBy(pvc, idx) != ""; got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// check that the controllers' ServiceAccounts and the API server's built-in objects count as generated, and look-alikes dont
func TestKubernetesGenerated(t *testing.T) {
	mk := func(apiVersion, kind, ns, name string, managed bool) *resource.Object {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
		u.SetNamespace(ns)
		u.SetName(name)
		if managed {
			u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
		}
		return resource.New(u)
	}
	for _, tc := range []struct {
		o    *resource.Object
		want bool
	}{
		{mk("v1", "ServiceAccount", "kube-system", "deployment-controller", false), true},
		{mk("v1", "ServiceAccount", "kube-system", "aws-cloud-provider", false), true},
		{mk("v1", "ServiceAccount", "kube-system", "podcertificaterequestcleaner", false), true},
		{mk("v1", "ServiceAccount", "kube-system", "deployment-controller", true), false},
		{mk("v1", "ServiceAccount", "kube-system", "my-app", false), false},
		{mk("v1", "ServiceAccount", "shop", "web-controller", false), false},
		{mk("networking.k8s.io/v1", "ServiceCIDR", "", "kubernetes", true), true},
		{mk("discovery.k8s.io/v1", "EndpointSlice", "default", "kubernetes", true), true},
		{mk("certificates.k8s.io/v1", "ClusterTrustBundle", "", "kubernetes.io:kube-apiserver-serving:abc", true), true},
		{mk("scheduling.k8s.io/v1", "PriorityClass", "", "system-node-critical", true), true},
		{mk("scheduling.k8s.io/v1", "PriorityClass", "", "high", true), false},
		{mk("v1", "ConfigMap", "kube-system", "kube-apiserver-legacy-service-account-token-tracking", true), true},
		{mk("cilium.io/v2", "CiliumEndpoint", "shop", "web-6d55c5b86-8wfsc", true), true},
		{mk("cilium.io/v2alpha1", "CiliumEndpointSlice", "", "ces-7bsnrybbw-72qrx", true), true},
		{mk("cilium.io/v2", "CiliumIdentity", "", "12345", true), true},
		{mk("cilium.io/v2", "CiliumNetworkPolicy", "shop", "web", true), false},
	} {
		if got := generatedBy(tc.o, index{}) != ""; got != tc.want {
			t.Errorf("%s: generated %v, want %v", tc.o.Ref(), got, tc.want)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
