package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
	"sigs.k8s.io/yaml"
)

// u builds an object from YAML, for the tests
func u(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &o.Object); err != nil {
		t.Fatal(err)
	}
	return o
}

// load reads the capability map
func load(t *testing.T) *capability.Map {
	t.Helper()
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// patchFor finds the patch for a kind and name
func patchFor(r *Result, kind, name string) *Patch {
	for i := range r.Patches {
		if r.Patches[i].Kind == kind && r.Patches[i].Name == name {
			return &r.Patches[i]
		}
	}
	return nil
}

// opAt finds an op by path
func opAt(p *Patch, path string) *Op {
	for i := range p.Ops {
		if p.Ops[i].Path == path {
			return &p.Ops[i]
		}
	}
	return nil
}

func TestStorageClassToGKEIsDynamicAndNotDefault(t *testing.T) {
	r, err := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: gp3
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: ebs.csi.eks.amazonaws.com
parameters:
  type: gp3`)}, platform.EKS, platform.GKE, load(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := patchFor(r, "StorageClass", "gp3")
	if p == nil {
		t.Fatal("no patch for gp3")
	}
	if op := opAt(p, "/provisioner"); op == nil || op.Value != "pd.csi.storage.gke.io" {
		t.Errorf("provisioner op %+v", op)
	}
	if op := opAt(p, "/parameters"); op == nil || op.Value.(map[string]any)["type"] != "dynamic" {
		t.Errorf("parameters op %+v", op)
	}
	if opAt(p, "/metadata/annotations/storageclass.kubernetes.io~1is-default-class") == nil {
		t.Errorf("the default-class annotation should go: %+v", p.Ops)
	}
}

// an AKS standard SSD class, written skuname as AKS does, is general-ssd and becomes EKS gp3
func TestStorageClassFromAKSLowercaseParameter(t *testing.T) {
	r, err := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: managed-csi}
provisioner: disk.csi.azure.com
parameters: {skuname: StandardSSD_LRS}`)}, platform.AKS, platform.EKS, load(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := patchFor(r, "StorageClass", "managed-csi")
	if p == nil || opAt(p, "/parameters").Value.(map[string]any)["type"] != "gp3" {
		t.Fatalf("got %+v", p)
	}
}

// an internal EKS load balancer stays internal on AKS, written explicitly, and loses its class, annotation and finalizer
func TestLoadBalancerStaysInternal(t *testing.T) {
	r, err := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: v1
kind: Service
metadata:
  name: web-lb
  namespace: shop
  finalizers: [service.eks.amazonaws.com/resources]
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-scheme: internal
spec:
  type: LoadBalancer
  loadBalancerClass: eks.amazonaws.com/nlb`)}, platform.EKS, platform.AKS, load(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := patchFor(r, "Service", "web-lb")
	if p == nil {
		t.Fatal("no patch")
	}
	for _, path := range []string{"/spec/loadBalancerClass", "/metadata/annotations/service.beta.kubernetes.io~1aws-load-balancer-scheme", "/metadata/finalizers"} {
		if op := opAt(p, path); op == nil || op.Op != "remove" {
			t.Errorf("%s not removed: %+v", path, p.Ops)
		}
	}
	if op := opAt(p, "/metadata/annotations/service.beta.kubernetes.io~1azure-load-balancer-internal"); op == nil || op.Value != "true" {
		t.Errorf("not written internal for AKS: %+v", p.Ops)
	}
}

// with no annotation, an EKS Auto Mode class means internal, and must not become public on GKE
func TestLoadBalancerClassDefaultInternal(t *testing.T) {
	r, _ := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: v1
kind: Service
metadata: {name: api, namespace: shop}
spec: {type: LoadBalancer, loadBalancerClass: eks.amazonaws.com/nlb}`)}, platform.EKS, platform.GKE, load(t), nil)
	p := patchFor(r, "Service", "api")
	if p == nil || opAt(p, "/metadata/annotations/networking.gke.io~1load-balancer-type") == nil {
		t.Fatalf("internal not kept: %+v", p)
	}
}

// a spot node selector becomes the target's, with the toleration its spot taint needs, and a pool label keeps its name
func TestSpotAndPoolPlacement(t *testing.T) {
	r, _ := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: worker, namespace: shop}
spec:
  template:
    spec:
      nodeSelector:
        eks.amazonaws.com/capacityType: SPOT
        eks.amazonaws.com/nodegroup: batch
        kubernetes.io/arch: arm64
      containers: [{name: w, image: w}]`)}, platform.EKS, platform.GKE, load(t), nil)
	p := patchFor(r, "Deployment", "worker")
	if p == nil {
		t.Fatal("no patch")
	}
	sel := opAt(p, "/spec/template/spec/nodeSelector").Value.(map[string]any)
	if sel["cloud.google.com/gke-spot"] != "true" || sel["cloud.google.com/gke-nodepool"] != "batch" || sel["kubernetes.io/arch"] != "arm64" || len(sel) != 3 {
		t.Errorf("selector %v", sel)
	}
	tols := opAt(p, "/spec/template/spec/tolerations")
	if tols == nil || !strings.Contains(strings.Join(func() []string {
		var s []string
		for _, x := range tols.Value.([]any) {
			s = append(s, x.(map[string]any)["key"].(string))
		}
		return s
	}(), ","), "cloud.google.com/gke-spot") {
		t.Errorf("no spot toleration: %+v", tols)
	}
}

// an object that creates cloud resources stops the translation until the user chooses, and a choice carries over
func TestCloudResourceObjectNeedsDecision(t *testing.T) {
	bucket := u(t, `
apiVersion: s3.services.k8s.aws/v1alpha1
kind: Bucket
metadata: {name: acme-orders, namespace: shop}
spec: {name: acme-orders}`)
	r, _ := Translate([]*unstructured.Unstructured{bucket}, platform.EKS, platform.GKE, load(t), nil)
	if len(r.Undecided()) != 1 {
		t.Fatalf("undecided %+v", r.Decisions)
	}
	r, _ = Translate([]*unstructured.Unstructured{bucket}, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "object/Bucket/shop/acme-orders", Choose: KeepSource}})
	if len(r.Undecided()) != 0 || patchFor(r, "Bucket", "acme-orders") == nil || !patchFor(r, "Bucket", "acme-orders").Delete {
		t.Errorf("keep-source should leave it to the source cluster: %+v", r)
	}
}

// verify counts a kept identity as explained, by the decision
func TestExplainerCitesDecision(t *testing.T) {
	sa := u(t, `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orders
  namespace: shop
  annotations: {eks.amazonaws.com/role-arn: "arn:aws:iam::111111111111:role/orders"}`)
	r, _ := Translate([]*unstructured.Unstructured{sa}, platform.EKS, platform.GKE, load(t), nil)
	got := Explainer(r.Decisions)(sa)
	if !strings.Contains(got, "identity/shop/orders is keep-source (the default)") {
		t.Errorf("got %q", got)
	}
}

func TestSamePlatformRefused(t *testing.T) {
	if _, err := Translate(nil, platform.GKE, platform.GKE, load(t), nil); err == nil {
		t.Error("translating GKE for GKE should be refused")
	}
}

// a choice its feature doesnt offer is refused, a decision for what the export no longer has is dropped and reported
func TestEarlierDecisionsChecked(t *testing.T) {
	sa := u(t, `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orders
  namespace: shop
  annotations: {eks.amazonaws.com/role-arn: "arn:aws:iam::111111111111:role/orders"}`)
	_, err := Translate([]*unstructured.Unstructured{sa}, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "identity/shop/orders", Choose: Equivalent}})
	if err == nil || !strings.Contains(err.Error(), "isn't an option here") {
		t.Errorf("got %v", err)
	}
	r, err := Translate([]*unstructured.Unstructured{sa}, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "identity/shop/gone", Choose: LeaveOut}})
	if err != nil || len(r.Stale) != 1 || r.Stale[0] != "identity/shop/gone" {
		t.Errorf("stale %v, %v", r, err)
	}
}

func TestReadDecisionsPlainErrors(t *testing.T) {
	for doc, want := range map[string]string{
		"this: [is: not valid":                 "isn't readable YAML",
		"- feature: a\n  chose: keep-source\n": `unknown key "chose"`,
		"- feature: a\n  choose: kep-source\n": "choose must be keep-source",
		"- choose: keep-source\n":              "has no feature",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, DecisionsFile), []byte(strings.ReplaceAll(doc, `\n`, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDecisions(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", doc, err, want)
		}
	}
	if ds, err := ReadDecisions(t.TempDir()); err != nil || ds != nil {
		t.Errorf("no file: %v, %v", ds, err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
