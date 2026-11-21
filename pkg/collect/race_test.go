// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package collect_test

// Race tests for hydration. Every corpus in testdata/corpus is served through
// fake cluster clients and exported with ClusterCollector, while objects are
// deleted, recreated or made unreadable between the metadata list and the
// fetch that follows it.
//
// The oracle: when objects vanish after the metadata list, the export must
// equal an export of a cluster where those objects never existed, collected
// without the metadata-only path (so with no hydration, and no race). Both
// clusters serve the same API types and namespaces, so owner lookups agree.
// The run must never fail, and never let a metadata-only object reach the
// snapshot.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"setdrift.io/setdrift-engine/pkg/ownership"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// raceConfig is one way of running the collector.
type raceConfig struct {
	name string
	// metadataOnly chooses the types listed metadata-only. "all" makes every
	// type metadata-only, so owner chains of any depth go through hydration
	// (Deployment -> ReplicaSet -> Pod, CronJob -> Job -> Pod).
	metadataOnly func(schema.GroupResource) bool
	// getLimit is HydrateGetLimit: positive fetches with GETs, negative lists.
	getLimit int
	// namespaced runs with -n set to every namespace in the corpus.
	namespaced bool
}

var raceConfigs = func() []raceConfig {
	var out []raceConfig
	for _, md := range []struct {
		name string
		fn   func(schema.GroupResource) bool
	}{
		{"default", func(gr schema.GroupResource) bool { return collect.DefaultMetadataOnly(gr) }},
		{"all", func(schema.GroupResource) bool { return true }},
	} {
		for _, fetch := range []struct {
			name  string
			limit int
		}{{"get", 100}, {"list", -1}} {
			for _, ns := range []bool{false, true} {
				name := md.name + "/" + fetch.name
				if ns {
					name += "/namespaced"
				}
				out = append(out, raceConfig{name: name, metadataOnly: md.fn, getLimit: fetch.limit, namespaced: ns})
			}
		}
	}
	return out
}()

// mutation changes the cluster after the metadata lists, before hydration.
type mutation struct {
	vanish   map[types.UID]bool // deleted
	recreate map[types.UID]bool // deleted and recreated under the same name
	forbid   *schema.GroupVersionResource
}

func TestHydrationRaces(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/corpus/*/input")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no corpus found: %v", err)
	}
	for _, dir := range dirs {
		corpus := filepath.Base(filepath.Dir(dir))
		objs := loadCorpus(t, dir)
		for _, cfg := range raceConfigs {
			t.Run(corpus+"/"+cfg.name, func(t *testing.T) {
				testRaces(t, objs, cfg)
			})
		}
	}
}

func testRaces(t *testing.T, objs []*unstructured.Unstructured, cfg raceConfig) {
	scope := objs
	if cfg.namespaced {
		scope = slices.DeleteFunc(slices.Clone(objs), func(u *unstructured.Unstructured) bool { return u.GetNamespace() == "" })
	}

	// No race: the cluster export equals the static export.
	assertSameExport(t, "no race", runCluster(t, objs, cfg, mutation{}), runStatic(t, objs, objs, cfg))

	// Hydration fetches the metadata-only objects with no exported
	// controller owner. Race each of them.
	for _, x := range hydrated(scope, cfg) {
		ref := refOf(x)
		children := descendants(objs, x)

		// The object vanishes. Its children were not fetched (their owner
		// was present at list time), so they must now be.
		assertSameExport(t, "vanish "+ref,
			runCluster(t, objs, cfg, mutation{vanish: uidSet(x)}),
			runStatic(t, objs, without(objs, uidSet(x)), cfg))

		// The object and everything it owns vanish, as with cascading
		// deletion: every fetch in every round finds nothing.
		subtree := uidSet(append([]*unstructured.Unstructured{x}, children...)...)
		assertSameExport(t, "vanish subtree of "+ref,
			runCluster(t, objs, cfg, mutation{vanish: subtree}),
			runStatic(t, objs, without(objs, subtree), cfg))

		// The object is deleted and recreated under the same name. The new
		// object was not part of this capture.
		assertSameExport(t, "recreate "+ref,
			runCluster(t, objs, cfg, mutation{recreate: uidSet(x)}),
			runStatic(t, objs, without(objs, uidSet(x)), cfg))
	}

	// A type that can be listed but not fetched: every object of it that
	// needs fetching is dropped with HYDRATE_FAILED, and their children are
	// fetched instead. The run must not fail.
	forbidden := map[schema.GroupVersionResource]bool{}
	for _, x := range hydrated(scope, cfg) {
		forbidden[gvrOf(x)] = true
	}
	for gvr := range forbidden {
		snap, err := runClusterErr(objs, objs, cfg, mutation{forbid: &gvr}, true)
		if err != nil {
			t.Errorf("forbid %s: export failed: %v", gvr, err)
			continue
		}
		if !hasFinding(snap, "HYDRATE_FAILED") {
			t.Errorf("forbid %s: no HYDRATE_FAILED finding", gvr)
		}
	}
}

// hydrated returns the objects the collector fetches in full in its first
// round: metadata-only objects whose controller owner is not collected.
func hydrated(scope []*unstructured.Unstructured, cfg raceConfig) []*unstructured.Unstructured {
	present := map[types.UID]bool{}
	for _, u := range scope {
		present[u.GetUID()] = true
	}
	var out []*unstructured.Unstructured
	for _, u := range scope {
		if !cfg.metadataOnly(gvrOf(u).GroupResource()) {
			continue
		}
		if !ownership.IsChild(resource.New(u), present) {
			out = append(out, u)
		}
	}
	return out
}

func descendants(objs []*unstructured.Unstructured, root *unstructured.Unstructured) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	parents := uidSet(root)
	for changed := true; changed; {
		changed = false
		for _, u := range objs {
			if owner := metav1.GetControllerOfNoCopy(u); owner != nil && parents[owner.UID] && !parents[u.GetUID()] {
				parents[u.GetUID()] = true
				out = append(out, u)
				changed = true
			}
		}
	}
	return out
}

// runCluster exports objs through fake cluster clients with m applied.
func runCluster(t *testing.T, objs []*unstructured.Unstructured, cfg raceConfig, m mutation) string {
	t.Helper()
	snap, err := runClusterErr(objs, objs, cfg, m, true)
	if err != nil {
		t.Fatalf("cluster export: %v", err)
	}
	return canonical(t, snap)
}

// runClusterErr exports served through fake cluster clients. all sets the
// API types and the namespaces requested, so a cluster with objects removed
// still serves the same types. With metadataOnly false the collector has no
// metadata client.
func runClusterErr(all, served []*unstructured.Unstructured, cfg raceConfig, m mutation, metadataOnly bool) (*snapshotv1alpha1.Snapshot, error) {
	resources := map[schema.GroupVersionResource]metav1.APIResource{}
	for _, u := range all {
		gvr := gvrOf(u)
		r := resources[gvr]
		r.Name, r.Kind, r.Verbs = gvr.Resource, u.GetKind(), []string{"get", "list"}
		r.Namespaced = r.Namespaced || u.GetNamespace() != ""
		resources[gvr] = r
	}

	// The metadata client serves the cluster as it was when listed; the
	// dynamic client, used for fetches, serves it after the mutation.
	var listed, fetched []runtime.Object
	phases := map[types.UID]string{}
	for _, u := range served {

		listed = append(listed, partialOf(u))
		if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok {
			phases[u.GetUID()] = phase
		}
		switch {
		case m.vanish[u.GetUID()]:
		case m.recreate[u.GetUID()]:
			n := u.DeepCopy()
			n.SetUID(u.GetUID() + "-recreated")
			fetched = append(fetched, n)
		default:
			fetched = append(fetched, u.DeepCopy())
		}
	}

	byGV := map[string][]metav1.APIResource{}
	listKinds := map[schema.GroupVersionResource]string{}
	var namespaces []string
	for gvr, r := range resources {
		gv := gvr.GroupVersion().String()
		byGV[gv] = append(byGV[gv], r)
		listKinds[gvr] = r.Kind + "List"
	}
	for _, u := range all {
		if ns := u.GetNamespace(); ns != "" && !slices.Contains(namespaces, ns) {
			namespaces = append(namespaces, ns)
		}
	}
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	for gv, rs := range byGV {
		disc.Resources = append(disc.Resources, &metav1.APIResourceList{GroupVersion: gv, APIResources: rs})
	}

	// the access checks list Namespaces to name the existing ones
	if _, ok := listKinds[schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}]; !ok {
		listKinds[schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}] = "NamespaceList"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, fetched...)
	if m.forbid != nil {
		gvr := *m.forbid
		deny := func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.GetResource() != gvr {
				return false, nil, nil
			}
			return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "", fmt.Errorf("denied by test"))
		}
		// GET for the get strategy, list for the list strategy. Full lists of
		// types that are not metadata-only stay allowed.
		dyn.PrependReactor("get", "*", deny)
		if cfg.metadataOnly(gvr.GroupResource()) {
			dyn.PrependReactor("list", "*", deny)
		}
	}

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		return nil, err
	}
	md := metadatafake.NewSimpleMetadataClient(scheme, listed...)
	// The fake ignores field selectors; apply status.phase like the API server.
	md.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		la := a.(clienttesting.ListAction)
		sel := la.GetListRestrictions().Fields
		if sel == nil || sel.Empty() {
			return false, nil, nil
		}
		list := &metav1.List{}
		for _, o := range listed {
			p := o.(*metav1.PartialObjectMetadata)
			if p.Kind != "Pod" || (la.GetNamespace() != "" && p.Namespace != la.GetNamespace()) {
				continue
			}
			if sel.Matches(fields.Set{"status.phase": phases[p.UID]}) {
				list.Items = append(list.Items, runtime.RawExtension{Object: p})
			}
		}
		return true, list, nil
	})

	c := &collect.ClusterCollector{
		Discovery:       disc,
		Dynamic:         dyn,
		MetadataOnly:    cfg.metadataOnly,
		HydrateGetLimit: cfg.getLimit,
	}
	if metadataOnly {
		c.Metadata = md
	}
	if cfg.namespaced {
		c.Namespaces = namespaces
	}
	return pipeline.New(c).Run(context.Background(), raceMeta())
}

// runStatic exports served (a cluster of all's types with only served in
// it) without the metadata-only path: no hydration, so no race.
func runStatic(t *testing.T, all, served []*unstructured.Unstructured, cfg raceConfig) string {
	t.Helper()
	snap, err := runClusterErr(all, served, cfg, mutation{}, false)
	if err != nil {
		t.Fatalf("static export: %v", err)
	}
	return canonical(t, snap)
}

func raceMeta() snapshot.Meta {
	return snapshot.Meta{ID: "race", Cluster: &snapshotv1alpha1.Cluster{Name: "race", KubernetesVersion: "v1.35.0"}}
}

// canonical renders a snapshot for comparison. Capture-level findings are
// sorted: their order follows collection order, which differs between the
// two collectors.
func canonical(t *testing.T, s *snapshotv1alpha1.Snapshot) string {
	t.Helper()
	sort.SliceStable(s.Findings, func(i, j int) bool {
		a, b := s.Findings[i], s.Findings[j]
		return a.Code+a.Message < b.Code+b.Message
	})
	b, err := snapshot.MarshalInspectJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertSameExport(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: cluster export differs from expected\n%s", what, firstDiff(got, want))
	}
}

func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d:\n  got:  %s\n  want: %s", i+1, gl, wl)
		}
	}
	return ""
}

func hasFinding(s *snapshotv1alpha1.Snapshot, code string) bool {
	for _, f := range s.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// loadCorpus reads a corpus as a cluster would hold it: one object per
// identity (the first, as snapshot.Build keeps), each with a UID.
func loadCorpus(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	res, err := collect.FileSource{Dir: dir}.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	seen := map[string]bool{}
	for _, o := range res.Objects {
		if seen[o.Ref()] {
			continue
		}
		seen[o.Ref()] = true
		u := o.Unstructured
		if u.GetUID() == "" {
			u.SetUID(types.UID("uid-" + o.Ref()))
		}
		out = append(out, u)
	}
	return out
}

func gvrOf(u *unstructured.Unstructured) schema.GroupVersionResource {
	gvr, _ := meta.UnsafeGuessKindToResource(u.GroupVersionKind())
	return gvr
}

func partialOf(u *unstructured.Unstructured) *metav1.PartialObjectMetadata {
	p := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: u.GetAPIVersion(), Kind: u.GetKind()}}
	m, _ := u.Object["metadata"].(map[string]any)
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &p.ObjectMeta); err != nil {
		panic(err)
	}
	return p
}

func refOf(u *unstructured.Unstructured) string { return resource.New(u).Ref() }

func uidSet(objs ...*unstructured.Unstructured) map[types.UID]bool {
	out := map[types.UID]bool{}
	for _, u := range objs {
		out[u.GetUID()] = true
	}
	return out
}

func without(objs []*unstructured.Unstructured, drop map[types.UID]bool) []*unstructured.Unstructured {
	return slices.DeleteFunc(slices.Clone(objs), func(u *unstructured.Unstructured) bool { return drop[u.GetUID()] })
}
