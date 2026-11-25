package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// a choice carries over while its object is as it was decided on; once the source changes it, translate waits for the user to choose again, with the earlier choice shown
func TestDecisionBasis(t *testing.T) {
	bucket := func(versioning string) *unstructured.Unstructured {
		return u(t, `
apiVersion: s3.services.k8s.aws/v1alpha1
kind: Bucket
metadata: {name: orders, namespace: shop}
spec: {name: orders, versioning: {status: `+versioning+`}}`)
	}
	src := func(v string) Source { return Source{Held: []*unstructured.Unstructured{bucket(v)}} }
	r, _ := Translate(nil, platform.EKS, platform.GKE, load(t), nil, src("Enabled"))
	ds := r.Decisions
	if ds[0].Basis == "" {
		t.Fatal("no basis")
	}
	ds[0].Choose = KeepSource
	// unchanged, the choice carries over
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), ds, src("Enabled"))
	if len(r.Undecided()) != 0 || r.Decisions[0].Choose != KeepSource {
		t.Fatalf("unchanged %+v", r.Decisions)
	}
	// changed at the source, translate waits, with the earlier choice in previous
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), ds, src("Suspended"))
	und := r.Undecided()
	if len(und) != 1 || !und[0].Waiting() || und[0].Previous != KeepSource || und[0].Basis == ds[0].Basis {
		t.Fatalf("changed %+v", r.Decisions)
	}
	// written and read back, still waiting, and choosing again ends it
	dir := t.TempDir()
	if err := WriteDecisions(dir, r.Decisions); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, DecisionsFile))
	if !strings.Contains(string(b), "previous: keep-source") || !strings.Contains(string(b), "basis: ") {
		t.Errorf("file:\n%s", b)
	}
	back, err := ReadDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), back, src("Suspended"))
	if len(r.Undecided()) != 1 {
		t.Errorf("still waiting %+v", r.Decisions)
	}
	back[0].Choose = LeaveOut
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), back, src("Suspended"))
	if len(r.Undecided()) != 0 || r.Decisions[0].Choose != LeaveOut || r.Decisions[0].Previous != "" {
		t.Errorf("chosen again %+v", r.Decisions)
	}
}

// a default the user never touched is taken again when its object changes, and said; a waiting decision explains nothing to verify
func TestDefaultBasisAndExplainer(t *testing.T) {
	sa := func(role string) *unstructured.Unstructured {
		return u(t, "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: orders, namespace: shop, annotations: {eks.amazonaws.com/role-arn: \""+role+"\"}}")
	}
	r, _ := Translate([]*unstructured.Unstructured{sa("arn:aws:iam::111122223333:role/a")}, platform.EKS, platform.GKE, load(t), nil, Source{})
	r2, _ := Translate([]*unstructured.Unstructured{sa("arn:aws:iam::111122223333:role/b")}, platform.EKS, platform.GKE, load(t), r.Decisions, Source{})
	if len(r2.Undecided()) != 0 || !strings.Contains(strings.Join(r2.Notes, " "), "ServiceAccount shop/orders has changed on the source since it was last translated. The default still applies, to keep ServiceAccount shop/orders using the source cloud (keep-source), so check that still fits") {
		t.Errorf("default %+v %v", r2.Decisions, r2.Notes)
	}
	waiting := []Decision{{Feature: "identity/shop/orders", Previous: LeaveOut, Default: KeepSource}}
	if got := Explainer(waiting)(sa("x")); got != "" {
		t.Errorf("a waiting decision explained %q", got)
	}
}

// a NetworkPolicy added since an accept-unenforced choice is a new question
func TestNetworkPolicyBasis(t *testing.T) {
	np := func(name string) *unstructured.Unstructured {
		return u(t, "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: "+name+", namespace: shop}\nspec: {podSelector: {}}")
	}
	one := []*unstructured.Unstructured{np("a")}
	r, _ := Translate(one, platform.GKE, platform.EKS, load(t), nil, Source{NetworkPolicy: "Calico"})
	r.Decisions[0].Choose = AcceptUnenforced
	r, _ = Translate(append(one, np("b")), platform.GKE, platform.EKS, load(t), r.Decisions, Source{NetworkPolicy: "Calico"})
	if len(r.Undecided()) != 1 || NetworkPolicyChoice(r.Decisions) != Require {
		t.Errorf("%+v", r.Decisions)
	}
}

// the translation's id is the same for the same translation, written twice, and new for a changed one
func TestTranslationID(t *testing.T) {
	svc := func(internal bool) []*unstructured.Unstructured {
		scheme := "internet-facing"
		if internal {
			scheme = "internal"
		}
		return []*unstructured.Unstructured{u(t, "apiVersion: v1\nkind: Service\nmetadata: {name: web, namespace: shop, annotations: {service.beta.kubernetes.io/aws-load-balancer-scheme: "+scheme+"}}\nspec: {type: LoadBalancer, loadBalancerClass: eks.amazonaws.com/nlb}")}
	}
	id := func(objs []*unstructured.Unstructured) string {
		r, _ := Translate(objs, platform.EKS, platform.GKE, load(t), nil, Source{})
		if err := Write(t.TempDir(), r, []string{"set"}, "test", RestoreInfo{}); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	a, b, c := id(svc(true)), id(svc(true)), id(svc(false))
	if len(a) != 8 || a != b || a == c {
		t.Errorf("ids %q %q %q", a, b, c)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
