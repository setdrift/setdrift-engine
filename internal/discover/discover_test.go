package discover

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// a cluster's APIs: group -> versions (preferred first) -> resources
type fakeVersion struct {
	version   string
	resources []metav1.APIResource
	down      bool // an aggregated API server that is unavailable
}

type fakeGroup struct {
	name     string
	versions []fakeVersion
}

func res(name, kind string, namespaced bool, verbs ...string) metav1.APIResource {
	return metav1.APIResource{Name: name, SingularName: strings.ToLower(kind), Kind: kind, Namespaced: namespaced, Verbs: verbs}
}

var cluster = []fakeGroup{
	{"", []fakeVersion{{version: "v1", resources: []metav1.APIResource{
		res("pods", "Pod", true, "get", "list"), {Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
		res("namespaces", "Namespace", false, "get", "list"), res("bindings", "Binding", true, "create")}}}},
	{"apps", []fakeVersion{{version: "v1", resources: []metav1.APIResource{res("deployments", "Deployment", true, "get", "list")}}}},
	// a group whose preferred version lacks a resource an older version serves
	{"example.com", []fakeVersion{
		{version: "v2", resources: []metav1.APIResource{res("widgets", "Widget", true, "get", "list")}},
		{version: "v1", resources: []metav1.APIResource{res("widgets", "Widget", true, "get", "list"), res("gadgets", "Gadget", false, "get", "list")}},
	}},
	{"metrics.k8s.io", []fakeVersion{{version: "v1beta1", down: true, resources: []metav1.APIResource{res("pods", "PodMetrics", true, "get", "list")}}}},
}

// server serves cluster with aggregated discovery or the legacy documents.
func server(t *testing.T, aggregated bool, groups []fakeGroup) *rest.Config {
	t.Helper()
	gv := func(g, v string) string { return schema.GroupVersion{Group: g, Version: v}.String() }
	write := func(w http.ResponseWriter, contentType string, v any) {
		w.Header().Set("Content-Type", contentType)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "application/json", map[string]string{"gitVersion": "v1.37.0", "major": "1", "minor": "37"})
	})
	const v2 = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if aggregated && strings.Contains(r.Header.Get("Accept"), "as=APIGroupDiscoveryList") {
			write(w, v2, aggregatedDoc(groups, true))
			return
		}
		write(w, "application/json", metav1.APIVersions{Versions: []string{"v1"}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		if aggregated && strings.Contains(r.Header.Get("Accept"), "as=APIGroupDiscoveryList") {
			write(w, v2, aggregatedDoc(groups, false))
			return
		}
		l := metav1.APIGroupList{}
		for _, g := range groups {
			if g.name == "" {
				continue
			}
			ag := metav1.APIGroup{Name: g.name}
			for _, v := range g.versions {
				ag.Versions = append(ag.Versions, metav1.GroupVersionForDiscovery{GroupVersion: gv(g.name, v.version), Version: v.version})
			}
			ag.PreferredVersion = ag.Versions[0]
			l.Groups = append(l.Groups, ag)
		}
		write(w, "application/json", l)
	})
	for _, g := range groups {
		for _, v := range g.versions {
			path := "/apis/" + gv(g.name, v.version)
			if g.name == "" {
				path = "/api/" + v.version
			}
			mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
				if v.down {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"service unavailable","reason":"ServiceUnavailable","code":503}`))
					return
				}
				write(w, "application/json", metav1.APIResourceList{GroupVersion: gv(g.name, v.version), APIResources: v.resources})
			})
		}
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return &rest.Config{Host: s.URL}
}

// aggregatedDoc is the aggregated discovery document of /api (core) or /apis.
func aggregatedDoc(groups []fakeGroup, core bool) map[string]any {
	var items []any
	for _, g := range groups {
		if (g.name == "") != core {
			continue
		}
		var versions []any
		for _, v := range g.versions {
			var rs []any
			for _, r := range v.resources {
				if strings.Contains(r.Name, "/") {
					continue
				}
				scope := "Cluster"
				if r.Namespaced {
					scope = "Namespaced"
				}
				var subs []any
				for _, s := range v.resources {
					if base, sub, ok := strings.Cut(s.Name, "/"); ok && base == r.Name {
						subs = append(subs, map[string]any{"subresource": sub, "verbs": s.Verbs,
							"responseKind": map[string]string{"group": g.name, "version": v.version, "kind": s.Kind}})
					}
				}
				rs = append(rs, map[string]any{"resource": r.Name, "singularResource": r.SingularName, "scope": scope, "verbs": r.Verbs,
					"responseKind": map[string]string{"group": g.name, "version": v.version, "kind": r.Kind}, "subresources": subs})
			}
			fresh := "Current"
			if v.down {
				fresh, rs = "Stale", nil
			}
			versions = append(versions, map[string]any{"version": v.version, "resources": rs, "freshness": fresh})
		}
		items = append(items, map[string]any{"metadata": map[string]string{"name": g.name}, "versions": versions})
	}
	return map[string]any{"kind": "APIGroupDiscoveryList", "apiVersion": "apidiscovery.k8s.io/v2", "items": items}
}

// flat lists group version, resource, kind and scope, sorted, to compare.
func flat(lists []*metav1.APIResourceList) []string {
	var out []string
	for _, l := range lists {
		for _, r := range l.APIResources {
			out = append(out, strings.Join([]string{l.GroupVersion, r.Name, r.Kind, map[bool]string{true: "ns", false: "cluster"}[r.Namespaced], strings.Join(r.Verbs, ",")}, " "))
		}
	}
	slices.Sort(out)
	return out
}

// PreferredResources gives what client-go's ServerPreferredResources gives,
// from aggregated and from legacy discovery, including a group version that
// is down.
func TestPreferredResourcesMatchesClientGo(t *testing.T) {
	for _, aggregated := range []bool{true, false} {
		cfg := server(t, aggregated, cluster)
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		want, wantErr := discovery.ServerPreferredResources(dc)
		if !discovery.IsGroupDiscoveryFailedError(wantErr) {
			t.Fatalf("client-go (aggregated %v): %v, want a group discovery failure", aggregated, wantErr)
		}
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, err := PreferredResources(context.Background(), c)
		if !IsPartial(err) || !strings.Contains(err.Error(), "metrics.k8s.io/v1beta1") {
			t.Errorf("aggregated %v: error %v, want a partial one naming metrics.k8s.io/v1beta1", aggregated, err)
		}
		if g, w := flat(got), flat(want); len(w) < 6 || !slices.Equal(g, w) {
			t.Errorf("aggregated %v:\n got %q\nwant %q", aggregated, g, w)
		}
	}
}

func TestServerVersionAndGroups(t *testing.T) {
	for _, aggregated := range []bool{true, false} {
		c, err := New(server(t, aggregated, cluster))
		if err != nil {
			t.Fatal(err)
		}
		v, err := c.ServerVersionWithContext(context.Background())
		if err != nil || v.GitVersion != "v1.37.0" {
			t.Errorf("version %v, %v", v, err)
		}
		gl, err := c.ServerGroupsWithContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, g := range gl.Groups {
			names = append(names, g.Name+"@"+g.PreferredVersion.Version)
		}
		if want := []string{"@v1", "apps@v1", "example.com@v2", "metrics.k8s.io@v1beta1"}; !slices.Equal(names, want) {
			t.Errorf("aggregated %v: groups %v, want %v", aggregated, names, want)
		}
	}
}

// The mapper maps kinds in every served version, defaulting to the
// preferred one, as client-go's discovery REST mapper does.
func TestRESTMapper(t *testing.T) {
	for _, aggregated := range []bool{true, false} {
		c, err := New(server(t, aggregated, cluster))
		if err != nil {
			t.Fatal(err)
		}
		m, err := RESTMapper(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			gk       schema.GroupKind
			version  string
			resource string
			scope    meta.RESTScopeName
		}{
			{schema.GroupKind{Kind: "Pod"}, "v1", "v1/pods", meta.RESTScopeNameNamespace},
			{schema.GroupKind{Kind: "Namespace"}, "", "v1/namespaces", meta.RESTScopeNameRoot},
			{schema.GroupKind{Group: "apps", Kind: "Deployment"}, "v1", "apps/v1/deployments", meta.RESTScopeNameNamespace},
			{schema.GroupKind{Group: "example.com", Kind: "Widget"}, "", "example.com/v2/widgets", meta.RESTScopeNameNamespace},
			{schema.GroupKind{Group: "example.com", Kind: "Widget"}, "v1", "example.com/v1/widgets", meta.RESTScopeNameNamespace},
			{schema.GroupKind{Group: "example.com", Kind: "Gadget"}, "", "example.com/v1/gadgets", meta.RESTScopeNameRoot},
		} {
			var versions []string
			if tc.version != "" {
				versions = []string{tc.version}
			}
			mp, err := m.RESTMapping(tc.gk, versions...)
			if err != nil {
				t.Errorf("aggregated %v: %v %s: %v", aggregated, tc.gk, tc.version, err)
				continue
			}
			if got := mp.Resource.GroupVersion().String() + "/" + mp.Resource.Resource; got != tc.resource || mp.Scope.Name() != tc.scope {
				t.Errorf("aggregated %v: %v %s maps to %s (%s), want %s (%s)", aggregated, tc.gk, tc.version, got, mp.Scope.Name(), tc.resource, tc.scope)
			}
		}
		if _, err := m.RESTMapping(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "v1beta9"); !meta.IsNoMatchError(err) {
			t.Errorf("an unserved version: %v, want no match", err)
		}
	}
}

// An API server's 401 arrives as a typed error, which the CLI explains.
func TestErrorsAreTyped(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`))
	}))
	defer s.Close()
	c, err := New(&rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ServerVersionWithContext(context.Background())
	var status interface{ Status() metav1.Status }
	if !errors.As(err, &status) || status.Status().Code != http.StatusUnauthorized {
		t.Errorf("error %T %v, want a 401 status error", err, err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
