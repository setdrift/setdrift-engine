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
	r = mustCompare(t, a, b, Options{})
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
	r := mustCompare(t, mk("6.9.0", "1"), mk("6.8.0", "3"), Options{})
	if len(r.Changed) != 1 || len(r.Changed[0].Reasons) != 1 || !strings.Contains(r.Changed[0].Reasons[0], "Helm chart podinfo 6.9.0 in c6.9.0, podinfo 6.8.0 in c6.8.0") {
		t.Errorf("reasons = %+v", r.Changed)
	}
	r = mustCompare(t, mk("6.9.0", "1"), mk("6.9.0", "3"), Options{LabelA: "x", LabelB: "y"})
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

func TestVolumesAndHPA(t *testing.T) {
	pvc := `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"shop"},"spec":{"resources":{"requests":{"storage":"1Gi"}}}}`
	deploy := `{"spec":{"template":{"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"data"}}]}}}}`
	mk := func(name, class string, hpa bool) *snapshotv1alpha1.Snapshot {
		web := res("apps/v1", "Deployment", "shop", "web", `{"spec":{"replicas":3}}`, unmanaged, "")
		if hpa {
			web = res("apps/v1", "Deployment", "shop", "web", `{"spec":{}}`, unmanaged, "")
			web.Findings = []*snapshotv1alpha1.Finding{{Code: "REPLICAS_FROM_HPA"}}
		}
		s := snap(name, "", "",
			res("v1", "PersistentVolumeClaim", "shop", "data", pvc, unmanaged, ""),
			res("apps/v1", "Deployment", "shop", "db", deploy, unmanaged, ""),
			web)
		s.Inventory = &snapshotv1alpha1.Inventory{Volumes: []*snapshotv1alpha1.Volume{{Namespace: "shop", Claim: "data", StorageClass: class, Capacity: "1Gi"}}}
		return s
	}
	r, err := Compare(mk("staging", "gp3", true), mk("prod", "io2", false), Options{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Change{}
	for _, c := range r.Changed {
		byName[c.A.Key.Name] = c
	}
	data := byName["data"]
	if len(data.Fields) != 1 || data.Fields[0].Path != "(volume) storageClass" || data.Fields[0].A != "gp3" {
		t.Errorf("volume fields = %+v", data.Fields)
	}
	if strings.Join(data.Reasons, "; ") != "the claim names no storage class, so each cluster's default applied: gp3 in staging, io2 in prod; mounted by Deployment db" {
		t.Errorf("volume reasons = %q", data.Reasons)
	}
	web := byName["web"]
	if len(web.Reasons) != 1 || web.Reasons[0] != "a HorizontalPodAutoscaler scales it in staging only, so its replicas are left out there" {
		t.Errorf("hpa reasons = %q", web.Reasons)
	}
	// both scaled by an HPA: the replicas are left out on both sides, so nothing differs
	r = mustCompare(t, mk("staging", "gp3", true), mk("prod", "gp3", true), Options{})
	if !r.Same() {
		t.Errorf("same classes, HPA on both sides: %+v", r.Changed)
	}
}

func TestNamespaceCollision(t *testing.T) {
	a := snap("a", "", "", res("v1", "ConfigMap", "shop", "cfg", `{}`, unmanaged, ""))
	b := snap("b", "", "",
		res("v1", "ConfigMap", "shop", "cfg", `{}`, unmanaged, ""),
		res("v1", "ConfigMap", "shop-prod", "cfg", `{}`, unmanaged, ""))
	_, err := Compare(a, b, Options{NamespaceMap: map[string]string{"shop": "shop-prod"}})
	if err == nil || !strings.Contains(err.Error(), "both namespace shop and namespace shop-prod") {
		t.Errorf("err = %v", err)
	}
}

func TestNodes(t *testing.T) {
	a := snap("a", "", "")
	b := snap("b", "", "")
	a.Inventory = &snapshotv1alpha1.Inventory{Nodes: &snapshotv1alpha1.NodeSummary{Count: 3, InstanceTypes: map[string]int32{"m5.large": 3}, Cpu: "6", Memory: "24Gi", Pods: 87}}
	b.Inventory = &snapshotv1alpha1.Inventory{Nodes: &snapshotv1alpha1.NodeSummary{Count: 4, InstanceTypes: map[string]int32{"m5.large": 2, "m5.xlarge": 2}, Cpu: "12", Memory: "48Gi", Pods: 116}}
	r := mustCompare(t, a, b, Options{})
	got := map[string]string{}
	for _, n := range r.Nodes {
		got[n.What] = n.A + " -> " + n.B
	}
	want := map[string]string{
		"nodes":              "3 -> 4",
		"instance types":     "m5.large x3 -> m5.large x2, m5.xlarge x2",
		"allocatable CPU":    "6 -> 12",
		"allocatable memory": "24.0Gi -> 48.0Gi",
		"allocatable pods":   "87 -> 116",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
	if !r.Same() {
		t.Error("node differences count as differences")
	}
	r = mustCompare(t, a, snap("c", "", ""), Options{})
	if len(r.NodesUnknown) != 1 || r.NodesUnknown[0] != "c" {
		t.Errorf("nodes unknown = %v", r.NodesUnknown)
	}
}

// A side that read only some namespaces, or could not list a type, limits
// both: nothing shows as in one side only for not having been read.
func TestCompareOnlyWhatBothRead(t *testing.T) {
	full := snap("prod", "", "",
		res("v1", "ConfigMap", "stage", "a", `{}`, unmanaged, ""),
		res("v1", "ConfigMap", "prod", "b", `{}`, unmanaged, ""),
		res("rbac.authorization.k8s.io/v1", "RoleBinding", "stage", "rb", `{}`, unmanaged, ""),
		res("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cr", `{}`, unmanaged, ""),
		res("v1", "Namespace", "", "stage", `{}`, unmanaged, ""))
	limited := snap("prod", "", "",
		res("v1", "ConfigMap", "stage", "a", `{}`, unmanaged, ""),
		res("v1", "Namespace", "", "stage", `{}`, unmanaged, ""))
	limited.Scope = &snapshotv1alpha1.ReadScope{Mode: "readable-namespaces", Namespaces: []string{"stage"}, Unreadable: []string{"prod"},
		Unread: []*snapshotv1alpha1.Unread{{Namespace: "stage", Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}}}
	r, err := Compare(full, limited, Options{LabelA: "then", LabelB: "now"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Same() || r.Identical != 2 {
		t.Errorf("only in A %v, only in B %v, identical %d; want the two read in both, the same", r.OnlyA, r.OnlyB, r.Identical)
	}
	notes := strings.Join(r.Limited, "\n")
	for _, want := range []string{"now read only namespaces stage (the ones its credentials can read); not readable there: prod",
		"compared namespaces stage only, and no cluster-scoped resources", "left out where a side could not list them: RoleBinding (1)"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	// -n alike on both sides needs no note
	a, b := snap("x", "", ""), snap("y", "", "")
	a.Scope = &snapshotv1alpha1.ReadScope{Mode: "namespaces", Namespaces: []string{"shop"}}
	b.Scope = &snapshotv1alpha1.ReadScope{Mode: "namespaces", Namespaces: []string{"shop"}}
	if r := mustCompare(t, a, b, Options{}); len(r.Limited) != 0 {
		t.Errorf("-n on both sides: %v", r.Limited)
	}
}

// mustCompare is Compare, failing the test on an error.
func mustCompare(t *testing.T, a, b *snapshotv1alpha1.Snapshot, opts Options) Result {
	t.Helper()
	r, err := Compare(a, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
