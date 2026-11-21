package collect_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// apiType is one served type for fakeCluster.
type apiType struct {
	gv, resource, kind string
	namespaced         bool
}

var (
	podType        = apiType{"v1", "pods", "Pod", true}
	namespaceType  = apiType{"v1", "namespaces", "Namespace", false}
	configMapType  = apiType{"v1", "configmaps", "ConfigMap", true}
	replicaSetType = apiType{"apps/v1", "replicasets", "ReplicaSet", true}
	jobType        = apiType{"batch/v1", "jobs", "Job", true}
	clusterRole    = apiType{"rbac.authorization.k8s.io/v1", "clusterroles", "ClusterRole", false}
)

func (a apiType) gvr() schema.GroupVersionResource {
	gv, _ := schema.ParseGroupVersion(a.gv)
	return gv.WithResource(a.resource)
}

// fakeCluster serves objs for types through discovery and the dynamic client.
func fakeCluster(apiTypes []apiType, objs ...*unstructured.Unstructured) (*fakediscovery.FakeDiscovery, *dynamicfake.FakeDynamicClient) {
	byGV := map[string][]metav1.APIResource{}
	var order []string
	listKinds := map[schema.GroupVersionResource]string{}
	for _, a := range apiTypes {
		if _, ok := byGV[a.gv]; !ok {
			order = append(order, a.gv)
		}
		byGV[a.gv] = append(byGV[a.gv], metav1.APIResource{Name: a.resource, Kind: a.kind, Namespaced: a.namespaced, Verbs: []string{"get", "list"}})
		listKinds[a.gvr()] = a.kind + "List"
	}
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	for _, gv := range order {
		disc.Resources = append(disc.Resources, &metav1.APIResourceList{GroupVersion: gv, APIResources: byGV[gv]})
	}
	var ros []runtime.Object
	for _, o := range objs {
		ros = append(ros, o)
	}
	// the access checks list Namespaces to name the existing ones
	if _, ok := listKinds[schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}]; !ok {
		listKinds[schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}] = "NamespaceList"
	}
	return disc, dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, ros...)
}

// owned builds an object in namespace shop with a controller owner.
func owned(apiVersion, kind, name, uid string, owner *metav1.OwnerReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	u.SetUID(types.UID(uid))
	if kind != "Namespace" && kind != "ClusterRole" {
		u.SetNamespace("shop")
	}
	if owner != nil {
		yes := true
		owner.Controller = &yes
		u.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	return u
}

func ref(apiVersion, kind, name, uid string) *metav1.OwnerReference {
	return &metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid)}
}

func codes(s *snapshotv1alpha1.Snapshot) map[string]string {
	out := map[string]string{}
	for _, f := range s.Findings {
		out[f.Code] = f.Message
	}
	for _, r := range s.Resources {
		for _, f := range r.Findings {
			out[f.Code] = f.Message
		}
	}
	return out
}

func names(s *snapshotv1alpha1.Snapshot) []string {
	var out []string
	for _, r := range s.Resources {
		out = append(out, r.Kind+"/"+r.Name)
	}
	return out
}

// TestListErrorPolicy: an error the API server returns for one type skips that
// type; credential and transport errors abort.
func TestListErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		abort bool
	}{
		{"conversion webhook down", apierrors.NewInternalError(errors.New("conversion webhook failed")), false},
		{"aggregated API unavailable", apierrors.NewServiceUnavailable("unavailable"), false},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("no")), false},
		{"unauthorized", apierrors.NewUnauthorized("token expired"), true},
		{"unreachable", fmt.Errorf("dial tcp 10.0.0.1:443: connect: connection refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disc, dyn := fakeCluster([]apiType{podType, configMapType},
				owned("v1", "Pod", "web", "p1", nil), owned("v1", "ConfigMap", "settings", "c1", nil))
			dyn.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			res, err := (&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Collect(context.Background())
			if tc.abort {
				if err == nil {
					t.Fatal("export succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Objects) != 1 || len(res.Findings) != 1 || res.Findings[0].Code != "LIST_FAILED" {
				t.Errorf("got %d objects and findings %v, want the Pod and one LIST_FAILED", len(res.Objects), res.Findings)
			}
		})
	}
}

// TestRequestedNamespaces: with namespaces requested, their Namespace objects
// are exported, cluster-scoped types are not, and a missing one is reported.
func TestRequestedNamespaces(t *testing.T) {
	disc, dyn := fakeCluster([]apiType{namespaceType, configMapType, clusterRole},
		owned("v1", "Namespace", "shop", "n1", nil),
		owned("v1", "ConfigMap", "settings", "c1", nil),
		owned("rbac.authorization.k8s.io/v1", "ClusterRole", "viewer", "r1", nil))
	c := &collect.ClusterCollector{Discovery: disc, Dynamic: dyn, Namespaces: []string{"shop", "shop", "typo"}}
	snap, err := pipeline.New(c).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names(snap), " "), "ConfigMap/settings Namespace/shop"; got != want {
		t.Errorf("exported %s, want %s", got, want)
	}
	if msg := codes(snap)["NAMESPACE_NOT_FOUND"]; !strings.Contains(msg, `"typo"`) {
		t.Errorf("NAMESPACE_NOT_FOUND = %q, want it to name typo", msg)
	}
}

// TestMissingOwners covers the three reasons a controller owner can be
// missing from the lists.
func TestMissingOwners(t *testing.T) {
	// Created after ReplicaSets were listed (a rollout): the list doesn't
	// have it, a GET does.
	lateRS := owned("apps/v1", "ReplicaSet", "web-new", "rs-new", nil)
	rolloutPod := owned("v1", "Pod", "web-new-1", "p1", ref("apps/v1", "ReplicaSet", "web-new", "rs-new"))
	// Its Job was deleted: garbage.
	orphan := owned("v1", "Pod", "migrate-1", "p2", ref("batch/v1", "Job", "migrate", "job-gone"))
	// Its Job was deleted and recreated under the same name: garbage too.
	newJob := owned("batch/v1", "Job", "reindex", "job-new", nil)
	stale := owned("v1", "Pod", "reindex-1", "p3", ref("batch/v1", "Job", "reindex", "job-old"))
	// Owned by a type the cluster doesn't serve to us: kept.
	widgetPod := owned("v1", "Pod", "widget-1", "p4", ref("example.com/v1", "Widget", "w", "w1"))

	disc, dyn := fakeCluster([]apiType{podType, replicaSetType, jobType}, lateRS, rolloutPod, orphan, newJob, stale, widgetPod)
	dyn.PrependReactor("list", "replicasets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSetList"}}, nil
	})
	dyn.PrependReactor("list", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "batch/v1", "kind": "JobList"}}, nil
	})
	snap, err := pipeline.New(&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names(snap), " "), "Pod/widget-1 ReplicaSet/web-new"; got != want {
		t.Errorf("exported %s, want %s", got, want)
	}
	c := codes(snap)
	if msg := c["OWNER_DELETED_SKIPPED"]; !strings.HasPrefix(msg, "2 objects") {
		t.Errorf("OWNER_DELETED_SKIPPED = %q, want 2 objects", msg)
	}
	if msg := c["OWNER_NOT_EXPORTED"]; !strings.Contains(msg, "was not collected") {
		t.Errorf("OWNER_NOT_EXPORTED = %q, want the not-collected reason", msg)
	}
}

// TestMirrorPodsNotFetched: a static Pod's mirror is a child of its Node,
// which is never collected. It stays metadata-only and is dropped.
func TestMirrorPodsNotFetched(t *testing.T) {
	mirror := owned("v1", "Pod", "etcd-node1", "p1", ref("v1", "Node", "node1", "node-uid"))
	mirror.SetAnnotations(map[string]string{"kubernetes.io/config.mirror": "abc"})
	disc, dyn := fakeCluster([]apiType{podType}, mirror)

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pom := &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: mirror.GetName(), UID: mirror.GetUID(), OwnerReferences: mirror.GetOwnerReferences(), Annotations: mirror.GetAnnotations()},
	}
	md := metadatafake.NewSimpleMetadataClient(scheme, pom)
	// The fake ignores field selectors; the Pod is running.
	md.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if sel := a.(clienttesting.ListAction).GetListRestrictions().Fields; sel == nil || sel.Empty() {
			return false, nil, nil
		}
		return true, &metav1.List{}, nil
	})
	snap, err := pipeline.New(&collect.ClusterCollector{Discovery: disc, Dynamic: dyn, Metadata: md}).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Resources) != 0 || len(codes(snap)) != 0 {
		t.Errorf("got %v and findings %v, want nothing", names(snap), codes(snap))
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "get" {
			t.Errorf("fetched %v; mirror Pods should stay metadata-only", a)
		}
	}
}

// TestConcurrentListingDeterministic: the result doesn't depend on how many
// types are listed at once or which finishes first.
func TestConcurrentListingDeterministic(t *testing.T) {
	var apiTypes []apiType
	var objs []*unstructured.Unstructured
	for i := range 40 {
		a := apiType{fmt.Sprintf("g%02d.example.com/v1", i), "things", "Thing", true}
		apiTypes = append(apiTypes, a)
		for j := range 3 {
			objs = append(objs, owned(a.gv, "Thing", fmt.Sprintf("t%d", j), fmt.Sprintf("u-%d-%d", i, j), nil))
		}
	}
	run := func(concurrency int) string {
		disc, dyn := fakeCluster(apiTypes, objs...)
		res, err := (&collect.ClusterCollector{Discovery: disc, Dynamic: dyn, Concurrency: concurrency}).Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, o := range res.Objects {
			b.WriteString(o.Ref() + "\n")
		}
		return b.String()
	}
	want := run(1)
	if strings.Count(want, "\n") != len(objs) {
		t.Fatalf("collected %d objects, want %d", strings.Count(want, "\n"), len(objs))
	}
	for range 5 {
		if got := run(16); got != want {
			t.Fatalf("order differs with concurrency 16:\n%s\nwant:\n%s", got, want)
		}
	}
}

// TestNamespaceScopedAccess: a caller who may not list Namespaces gets the
// fallback namespace, with a finding naming what they can list there; with
// no fallback, a ScopeError.
func TestNamespaceScopedAccess(t *testing.T) {
	setup := func() (*fakediscovery.FakeDiscovery, *dynamicfake.FakeDynamicClient) {
		other := owned("v1", "ConfigMap", "other", "c2", nil)
		other.SetNamespace("payments")
		disc, dyn := fakeCluster([]apiType{namespaceType, configMapType, clusterRole},
			owned("v1", "Namespace", "shop", "n1", nil), owned("v1", "ConfigMap", "settings", "c1", nil), other)
		forbidden := func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.GetNamespace() == "shop" {
				return false, nil, nil
			}
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New("no"))
		}
		dyn.PrependReactor("list", "*", forbidden)
		dyn.PrependReactor("get", "namespaces", forbidden)
		dyn.PrependReactor("create", "selfsubjectrulesreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectRulesReview",
				"status": map[string]any{"resourceRules": []any{
					map[string]any{"verbs": []any{"get", "list"}, "apiGroups": []any{""}, "resources": []any{"configmaps", "pods", "pods/log"}},
					map[string]any{"verbs": []any{"list"}, "apiGroups": []any{"apps"}, "resources": []any{"deployments"}},
					map[string]any{"verbs": []any{"create"}, "apiGroups": []any{""}, "resources": []any{"secrets"}},
				}},
			}}, nil
		})
		return disc, dyn
	}

	disc, dyn := setup()
	c := &collect.ClusterCollector{Discovery: disc, Dynamic: dyn, FallbackNamespace: "shop"}
	snap, err := pipeline.New(c).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(snap), " "); got != "ConfigMap/settings" {
		t.Errorf("exported %s, want only shop's ConfigMap", got)
	}
	want := `only namespace "shop" was exported; in it they can list configmaps, deployments.apps, pods`
	if msg := codes(snap)["NAMESPACE_SCOPED_ACCESS"]; !strings.Contains(msg, want) {
		t.Errorf("NAMESPACE_SCOPED_ACCESS = %q, want it to contain %q", msg, want)
	}
	if len(c.Namespaces) != 0 {
		t.Error("Collect changed the collector's Namespaces")
	}

	disc, dyn = setup()
	_, err = (&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Collect(context.Background())
	var scopeErr *collect.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Errorf("error %v, want a ScopeError", err)
	}
}

// TestNothingListed: an export where every list failed is an error, not an
// empty repository.
func TestNothingListed(t *testing.T) {
	disc, dyn := fakeCluster([]apiType{configMapType, podType})
	dyn.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New("no"))
	})
	if _, err := (&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Collect(context.Background()); err == nil {
		t.Error("no error when nothing could be listed")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
