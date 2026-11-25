package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// writeSet writes objects as a one-file export a translation can build on, and gives back its root
func writeSet(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	root := t.TempDir()
	var docs []string
	for _, o := range objs {
		b, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(b))
	}
	files := map[string]string{
		"kustomization.yaml":     "resources:\n- set\n",
		"set/kustomization.yaml": "resources:\n- all.yaml\n",
		"set/all.yaml":           strings.Join(docs, "---\n"),
	}
	for f, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// translated translates objects from one platform to another with every default, writes the overlay and builds it as kustomize applies it
func translated(t *testing.T, objs []*unstructured.Unstructured, from, to platform.Distribution) []*unstructured.Unstructured {
	t.Helper()
	root := writeSet(t, objs)
	r, err := Translate(objs, from, to, load(t), nil, Source{Region: "us-east-2"})
	if err != nil {
		t.Fatal(err)
	}
	if und := r.Undecided(); len(und) > 0 {
		t.Fatalf("%s to %s stopped for decisions: %+v", from, to, und)
	}
	if err := Write(root, r, []string{"set"}, "test", RestoreInfo{}); err != nil {
		t.Fatal(err)
	}
	out, err := verify.Build(root, filepath.Join(root, filepath.FromSlash(r.Dir)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// volumes gives back a workload's Pod volumes, read without a copy, since kustomize's output holds plain ints
func volumes(o *unstructured.Unstructured) []any {
	v, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "template", "spec", "volumes")
	l, _ := v.([]any)
	return l
}

// same tells if two objects are equal as JSON, so number types don't matter, leaving out the translation's stamp as diff does
func same(a, b *unstructured.Unstructured) bool {
	ja, _ := json.Marshal(diff.WithoutTranslationMark(a.Object))
	jb, _ := json.Marshal(diff.WithoutTranslationMark(b.Object))
	var x, y any
	_ = json.Unmarshal(ja, &x)
	_ = json.Unmarshal(jb, &y)
	return reflect.DeepEqual(x, y)
}

// find gives back the object of a kind and name
func find(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

// a workload using an AWS identity, translated to GKE and back, is as it was: the token translate gave it on GKE is taken out again, since EKS's webhook gives it
func TestRoundTripKeptIdentity(t *testing.T) {
	sa := u(t, `
apiVersion: v1
kind: ServiceAccount
metadata: {name: reports, namespace: shop, annotations: {eks.amazonaws.com/role-arn: "arn:aws:iam::111122223333:role/reports"}}`)
	dep := u(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: reports, namespace: shop}
spec:
  selector: {matchLabels: {app: reports}}
  template:
    metadata: {labels: {app: reports}}
    spec:
      serviceAccountName: reports
      containers: [{name: reports, image: busybox, ports: [{containerPort: 8080}], env: [{name: MODE, value: prod}]}]`)
	onGKE := translated(t, []*unstructured.Unstructured{sa, dep}, platform.EKS, platform.GKE)
	g := find(onGKE, "Deployment", "reports")
	if vols := volumes(g); len(vols) != 1 {
		t.Fatalf("on GKE the Pods need the token: %+v", g.Object)
	}
	if got, _, _ := unstructured.NestedString(g.Object, "metadata", "annotations", "setdrift.io/translation"); !strings.HasPrefix(got, "te2g-") || len(got) != len("te2g-")+8 {
		t.Errorf("on GKE the object carries the translation's code: %q", got)
	}
	back := translated(t, onGKE, platform.GKE, platform.EKS)
	if got := find(back, "Deployment", "reports"); !same(got, dep) {
		gb, _ := yaml.Marshal(got.Object)
		t.Errorf("back on EKS the Deployment isn't as it was:\n%s", gb)
	}
	// to a third cloud, the token stays and isn't added twice
	onAKS := translated(t, onGKE, platform.GKE, platform.AKS)
	a := find(onAKS, "Deployment", "reports")
	if vols := volumes(a); len(vols) != 1 {
		t.Errorf("on AKS, one token volume: %+v", vols)
	}
}

// a translation for EKS writes for Auto Mode: its EBS driver, and its load balancer class on a load balancer Service
func TestForEKSAutoMode(t *testing.T) {
	r, err := Translate([]*unstructured.Unstructured{u(t, `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: standard-rwo}
provisioner: pd.csi.storage.gke.io
parameters: {type: pd-balanced}`), u(t, `
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop, annotations: {networking.gke.io/load-balancer-type: Internal}}
spec: {type: LoadBalancer, ports: [{port: 80}]}`)}, platform.GKE, platform.EKS, load(t), nil, Source{})
	if err != nil {
		t.Fatal(err)
	}
	if p := patchFor(r, "StorageClass", "standard-rwo"); p == nil || opAt(p, "/provisioner").Value != "ebs.csi.eks.amazonaws.com" {
		t.Errorf("storage class %+v", p)
	}
	p := patchFor(r, "Service", "web")
	if p == nil || opAt(p, "/spec/loadBalancerClass") == nil || opAt(p, "/spec/loadBalancerClass").Value != "eks.amazonaws.com/nlb" ||
		opAt(p, "/metadata/annotations/service.beta.kubernetes.io~1aws-load-balancer-scheme").Value != "internal" {
		t.Errorf("service %+v", p)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
