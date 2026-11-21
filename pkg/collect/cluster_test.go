// tests metadata-only listing against a fake cluster, which objects get fetched in full and how, and that no stub reaches the snapshot

package collect_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	"setdrift.io/setdrift-engine/pkg/snapshot"
)

// the resources the fake dynamic client serves, for its list kinds
var (
	podsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	replicasetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
)

// obj builds a minimal object in namespace shop with a spec marker, owner when set is the controller owner's uid
func obj(apiVersion, kind, name, uid, owner string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetUID(types.UID(uid))
	if owner != "" {
		yes := true
		u.SetOwnerReferences([]metav1.OwnerReference{{Name: "owner", UID: types.UID(owner), Controller: &yes}})
	}
	_ = unstructured.SetNestedField(u.Object, "full", "spec", "marker")
	return u
}

// partial gives back the metadata-only view of u, as the metadata API serves it
func partial(u *unstructured.Unstructured) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: u.GetAPIVersion(), Kind: u.GetKind()},
		ObjectMeta: metav1.ObjectMeta{Namespace: u.GetNamespace(), Name: u.GetName(), UID: u.GetUID(), OwnerReferences: u.GetOwnerReferences()},
	}
}

// newCollector builds a fake cluster with a Deployment, its ReplicaSet, a Succeeded Pod owned by that ReplicaSet, a running Pod with no owner and a Failed Pod with no owner
func newCollector(t *testing.T) (*collect.ClusterCollector, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	deploy := obj("apps/v1", "Deployment", "web", "d1", "")
	rs := obj("apps/v1", "ReplicaSet", "web-abc", "r1", "d1")
	child := obj("v1", "Pod", "web-abc-1", "p1", "r1")
	bare1 := obj("v1", "Pod", "toolbox", "p2", "")
	bare2 := obj("v1", "Pod", "debug", "p3", "")
	phases := map[string]string{"web-abc-1": "Succeeded", "toolbox": "Running", "debug": "Failed"}
	for _, u := range []*unstructured.Unstructured{child, bare1, bare2} {
		_ = unstructured.SetNestedField(u.Object, phases[u.GetName()], "status", "phase")
	}

	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "list"}},
			{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: []string{"get", "list"}},
		}},
	}}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			podsGVR: "PodList", deploymentsGVR: "DeploymentList", replicasetsGVR: "ReplicaSetList",
		}, deploy, rs, child, bare1, bare2)

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pods := []*metav1.PartialObjectMetadata{partial(child), partial(bare1), partial(bare2)}
	md := metadatafake.NewSimpleMetadataClient(scheme, partial(rs), pods[0], pods[1], pods[2])
	// the fake ignores field selectors, so this applies status.phase like the API server does
	md.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sel := a.(clienttesting.ListAction).GetListRestrictions().Fields
		if sel == nil || sel.Empty() {
			return false, nil, nil
		}
		list := &metav1.List{}
		for _, p := range pods {
			if sel.Matches(fields.Set{"status.phase": phases[p.Name]}) {
				list.Items = append(list.Items, runtime.RawExtension{Object: p})
			}
		}
		return true, list, nil
	})

	return &collect.ClusterCollector{Discovery: disc, Dynamic: dyn, Metadata: md}, dyn
}

// summary is what a test checks about one collected object, its name, whether its a stub and whether it has full data
type summary struct {
	name    string
	partial bool
	full    bool
}

// collectSummary runs c and gives back its objects summarized
func collectSummary(t *testing.T, c *collect.ClusterCollector) []summary {
	t.Helper()
	res, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return summarize(res)
}

// summarize turns a result's objects into summaries sorted by name
func summarize(res *collect.Result) []summary {
	var out []summary
	for _, o := range res.Objects {
		_, full, _ := unstructured.NestedString(o.Object, "spec", "marker")
		out = append(out, summary{name: o.GetName(), partial: o.Partial, full: full})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// want is the expected summary, children of collected objects stay metadata-only and objects with no exported owner are fetched in full
var want = []summary{
	{name: "debug", full: true},
	{name: "toolbox", full: true},
	{name: "web", full: true},
	{name: "web-abc", partial: true},
	{name: "web-abc-1", partial: true},
}

// check that the bare Pods are fetched one GET each and Pods and ReplicaSets are never listed in full
func TestMetadataOnlyHydrateByGet(t *testing.T) {
	c, dyn := newCollector(t)
	got := collectSummary(t, c)
	assertEqual(t, got, want)

	// full data came from one Deployment list plus one GET per bare Pod, Pods and ReplicaSets were never listed in full
	var gets, lists []string
	for _, a := range dyn.Actions() {
		switch a.GetVerb() {
		case "get":
			gets = append(gets, a.GetResource().Resource)
		case "list":
			lists = append(lists, a.GetResource().Resource)
		}
	}
	assertEqual(t, lists, []string{"deployments"})
	assertEqual(t, gets, []string{"pods", "pods"})
}

// check that when the server refuses one Pod, the other Pods fetched one by one are still exported, only the refused one is dropped with HYDRATE_FAILED
func TestHydrateByGetKeepsFetched(t *testing.T) {
	c, dyn := newCollector(t)
	dyn.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		// refusing the debug Pod only, the toolbox Pod is served as usual
		if a.(clienttesting.GetAction).GetName() == "debug" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "debug", errors.New("denied"))
		}
		return false, nil, nil
	})
	res, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := summarize(res)
	assertEqual(t, got, []summary{
		{name: "toolbox", full: true},
		{name: "web", full: true},
		{name: "web-abc", partial: true},
		{name: "web-abc-1", partial: true},
	})
	warned := false
	for _, f := range res.Findings {
		warned = warned || f.Code == "HYDRATE_FAILED"
	}
	if !warned {
		t.Error("no HYDRATE_FAILED finding for the refused Pod")
	}
}

// check that above HydrateGetLimit the Pods are listed in full instead of fetched one by one
func TestMetadataOnlyHydrateByList(t *testing.T) {
	c, dyn := newCollector(t)
	c.HydrateGetLimit = 1 // two bare Pods exceed it, so Pods are listed in full
	assertEqual(t, collectSummary(t, c), want)

	for _, a := range dyn.Actions() {
		if a.GetVerb() == "get" {
			t.Errorf("unexpected GET of %s", a.GetResource().Resource)
		}
	}
}

// check that without a metadata client every object is listed in full
func TestFullListWithoutMetadataClient(t *testing.T) {
	c, _ := newCollector(t)
	c.Metadata = nil
	got := collectSummary(t, c)
	for _, s := range got {
		if s.partial || !s.full {
			t.Errorf("%s: want full object, got partial=%v full=%v", s.name, s.partial, s.full)
		}
	}
	if len(got) != 5 {
		t.Errorf("got %d objects, want 5", len(got))
	}
}

// assertEqual fails the test unless got and want hold the same items in the same order
func assertEqual[T comparable](t *testing.T, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// check that no metadata-only object reaches the snapshot, ownership drops every one hydrate left partial and snapshot.Build rejects any that remain
func TestPipelineDropsAllPartials(t *testing.T) {
	c, _ := newCollector(t)
	snap, err := pipeline.New(c).Run(context.Background(), snapshot.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range snap.Resources {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	// debug is Failed, so lifecycle skips it
	assertEqual(t, names, []string{"toolbox", "web"})

	// the metadata-only child Pod still counts as terminated
	var msgs []string
	for _, f := range snap.Findings {
		if f.Code == "TERMINATED_PODS_TO_CLEANUP" {
			msgs = append(msgs, f.Message)
		}
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "2 terminated Pods") || !strings.Contains(msgs[0], "1 Succeeded, 1 Failed") {
		t.Errorf("TERMINATED_PODS_TO_CLEANUP findings = %q", msgs)
	}
}

// check that metadata-only Pods carry their phase when terminated, from the status.phase filtered lists
func TestPodPhasesTagged(t *testing.T) {
	c, _ := newCollector(t)
	res, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range res.Objects {
		if o.GetName() != "web-abc-1" {
			continue
		}
		if !o.Partial {
			t.Fatal("web-abc-1 should be metadata-only")
		}
		if phase, _, _ := unstructured.NestedString(o.Object, "status", "phase"); phase != "Succeeded" {
			t.Errorf("web-abc-1 phase = %q, want Succeeded", phase)
		}
		return
	}
	t.Fatal("web-abc-1 not collected")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
