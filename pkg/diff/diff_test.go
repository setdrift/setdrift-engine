// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"reflect"
	"strings"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func res(apiVersion, kind, ns, name, manifest string, src snapshotv1alpha1.Source, manager string) *snapshotv1alpha1.Resource {
	return &snapshotv1alpha1.Resource{ApiVersion: apiVersion, Kind: kind, Namespace: ns, Name: name, ManifestJson: manifest,
		Provenance: &snapshotv1alpha1.Provenance{Source: src, Manager: manager}}
}

func snap(name, providerID, region string, rs ...*snapshotv1alpha1.Resource) *snapshotv1alpha1.Snapshot {
	return &snapshotv1alpha1.Snapshot{
		Cluster:   &snapshotv1alpha1.Cluster{Name: name, ProviderId: providerID, Classification: &snapshotv1alpha1.Platform{Region: region}},
		Resources: rs,
	}
}

const unmanaged = snapshotv1alpha1.Source_SOURCE_UNMANAGED

func TestCompare(t *testing.T) {
	deploy := func(replicas, image, env string) string {
		return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"shop"},"spec":{"replicas":` + replicas +
			`,"template":{"spec":{"containers":[{"name":"sidecar","image":"envoy:1"},{"name":"api","image":"` + image + `","env":[{"name":"QUEUE","value":"` + env + `"}]}]}}}}`
	}
	a := snap("staging", "arn:aws:eks:us-east-1:111111111111:cluster/staging", "",
		res("apps/v1", "Deployment", "shop", "api", deploy("2", "api:1.0", "arn:aws:sqs:us-east-1:111111111111:staging-jobs"), unmanaged, ""),
		res("v1", "ConfigMap", "shop", "same", `{"data":{"a":"1"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop", "env", `{"data":{"url":"https://staging.example.com"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop", "only-a", `{}`, unmanaged, ""),
		res("v1", "ConfigMap", "kube-system", "aws-auth", `{"data":{"x":"1"}}`, snapshotv1alpha1.Source_SOURCE_PLATFORM, "eks"),
	)
	b := snap("prod", "arn:aws:eks:eu-west-1:222222222222:cluster/prod", "",
		// containers in another order: paired by name, so only the real changes show
		res("apps/v1", "Deployment", "shop", "api",
			`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"shop"},"spec":{"replicas":4,"template":{"spec":{"containers":[`+
				`{"name":"api","image":"api:1.1","env":[{"name":"QUEUE","value":"arn:aws:sqs:eu-west-1:222222222222:prod-jobs"}]},{"name":"sidecar","image":"envoy:1"}]}}}}`,
			snapshotv1alpha1.Source_SOURCE_HELM, "shop/api"),
		res("v1", "ConfigMap", "shop", "same", `{"data":{"a":"1"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop", "env", `{"data":{"url":"https://prod.example.com"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop", "only-b", `{}`, snapshotv1alpha1.Source_SOURCE_ARGO_CD, "shop"),
		res("v1", "ConfigMap", "kube-system", "aws-auth", `{"data":{"x":"2"}}`, snapshotv1alpha1.Source_SOURCE_PLATFORM, "eks"),
	)
	r, err := Compare(a, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Identical != 1 {
		t.Errorf("identical = %d, want 1", r.Identical)
	}
	if r.A.PlatformObjects != 1 || r.B.PlatformObjects != 1 {
		t.Errorf("platform objects = %d, %d; want them left out", r.A.PlatformObjects, r.B.PlatformObjects)
	}
	if r.A.Account != "111111111111" || r.A.Region != "us-east-1" || r.B.Region != "eu-west-1" {
		t.Errorf("sides from the provider ID: %+v %+v", r.A, r.B)
	}
	if len(r.OnlyA) != 1 || r.OnlyA[0].Key.Name != "only-a" || r.OnlyA[0].Manager != "applied by hand" {
		t.Errorf("only in A = %+v", r.OnlyA)
	}
	if len(r.OnlyB) != 1 || r.OnlyB[0].Key.Name != "only-b" || r.OnlyB[0].Manager != "Argo CD shop" {
		t.Errorf("only in B = %+v", r.OnlyB)
	}
	if len(r.Changed) != 2 {
		t.Fatalf("changed = %+v", r.Changed)
	}
	byName := map[string]Change{}
	for _, c := range r.Changed {
		byName[c.A.Key.Name] = c
	}

	api := byName["api"]
	var paths []string
	for _, f := range api.Fields {
		paths = append(paths, f.Path)
	}
	want := []string{"spec.replicas", "spec.template.spec.containers[api].image"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("api fields = %v, want %v", paths, want)
	}
	// the queue ARN differs only in region, account and cluster name
	if len(api.EnvFields) != 1 || api.EnvFields[0].Path != "spec.template.spec.containers[api].env[QUEUE].value" {
		t.Errorf("api env fields = %+v", api.EnvFields)
	}
	if len(api.Reasons) != 1 || !strings.Contains(api.Reasons[0], "applied by hand in staging, Helm shop/api in prod") {
		t.Errorf("api reasons = %v", api.Reasons)
	}

	// staging.example.com vs prod.example.com: the cluster names
	if env := byName["env"]; !env.EnvOnly() {
		t.Errorf("env should differ only in cluster values: %+v", env.Fields)
	}
	if r.Same() {
		t.Error("Same() with differences")
	}
	if d, e := r.Counts(); d != 1 || e != 1 {
		t.Errorf("counts = %d, %d; want 1, 1", d, e)
	}
}

func TestNamespaceMap(t *testing.T) {
	a := snap("a", "", "",
		res("v1", "Namespace", "", "shop-staging", `{"metadata":{"name":"shop-staging"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop-staging", "cfg", `{"metadata":{"name":"cfg","namespace":"shop-staging"},"data":{"host":"db.shop-staging.svc"}}`, unmanaged, ""),
	)
	b := snap("b", "", "",
		res("v1", "Namespace", "", "shop", `{"metadata":{"name":"shop"}}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop", "cfg", `{"metadata":{"name":"cfg","namespace":"shop"},"data":{"host":"db.shop.svc"}}`, unmanaged, ""),
	)
	r, err := Compare(a, b, Options{NamespaceMap: map[string]string{"shop-staging": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.OnlyA)+len(r.OnlyB) != 0 {
		t.Fatalf("namespaces not paired: %+v %+v", r.OnlyA, r.OnlyB)
	}
	if !r.Same() {
		t.Errorf("mapped namespaces should leave no differences: %+v", r.Changed)
	}

	// without the map, nothing pairs
	r, _ = Compare(a, b, Options{})
	if len(r.OnlyA) != 2 || len(r.OnlyB) != 2 {
		t.Errorf("without the map: only A %d, only B %d; want 2, 2", len(r.OnlyA), len(r.OnlyB))
	}
}

func TestHelmChartReason(t *testing.T) {
	helm := snapshotv1alpha1.Source_SOURCE_HELM
	mk := func(version, replicas string) *snapshotv1alpha1.Snapshot {
		s := snap("c"+version, "", "", res("apps/v1", "Deployment", "web", "podinfo", `{"spec":{"replicas":`+replicas+`}}`, helm, "web/podinfo"))
		s.Inventory = &snapshotv1alpha1.Inventory{HelmReleases: []*snapshotv1alpha1.HelmRelease{{Namespace: "web", Name: "podinfo", Chart: "podinfo", ChartVersion: version}}}
		return s
	}
	r, _ := Compare(mk("6.9.0", "1"), mk("6.8.0", "3"), Options{})
	if len(r.Changed) != 1 || len(r.Changed[0].Reasons) != 1 || !strings.Contains(r.Changed[0].Reasons[0], "Helm chart podinfo 6.9.0 in c6.9.0, podinfo 6.8.0 in c6.8.0") {
		t.Errorf("reasons = %+v", r.Changed)
	}
	r, _ = Compare(mk("6.9.0", "1"), mk("6.9.0", "3"), Options{LabelA: "x", LabelB: "y"})
	if len(r.Changed) != 1 || !strings.Contains(r.Changed[0].Reasons[0], "same Helm chart") || r.A.Label != "x" {
		t.Errorf("same chart: %+v", r)
	}
}

func TestSubstitutionBoundaries(t *testing.T) {
	sub := substitutions(Side{Cluster: "prod", Region: "us-east-1"}, map[string]string{"shop-staging": "shop"}, false)
	for in, want := range map[string]string{
		"prod":                 "\x00\x01\x01\x01\x00",
		"production":           "production", // not a whole word
		"db.shop.svc":          "db.\x00\x01\x01\x01\x01\x00.svc",
		"shop-shop":            "\x00\x01\x01\x01\x01\x00-\x00\x01\x01\x01\x01\x00",
		"prod.us-east-1.x.com": "\x00\x01\x01\x01\x00.\x00\x01\x01\x00.x.com",
	} {
		if got := sub.apply(in); got != want {
			t.Errorf("apply(%q) = %q, want %q", in, got, want)
		}
	}
	// values too short to tell apart from other words are left alone
	if got := substitutions(Side{Cluster: "a"}, nil, true).apply("a-b"); got != "a-b" {
		t.Errorf("short cluster name substituted: %q", got)
	}
}
