// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package discover asks an API server which APIs it serves: its version,
// its groups, and each group version's resources.
//
// It stands in for client-go's discovery package, which imports
// client-go/kubernetes/scheme and with it every built-in API type (about 15
// MiB of the binary) that SetDrift never uses: SetDrift reads objects as
// unstructured. It reads aggregated discovery (one request each for /api and
// /apis, Kubernetes 1.30+ by default) and falls back to a request per group
// version on servers that don't serve it.
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/rest"
)

// Interface is what SetDrift asks of discovery. client-go's fake discovery
// client implements it too, for tests.
type Interface interface {
	ServerVersionWithContext(ctx context.Context) (*version.Info, error)
	ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error)
	ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error)
}

// defaultTimeout bounds each discovery request when the config sets no
// timeout, as client-go's discovery client does.
const defaultTimeout = 32 * time.Second

// acceptAggregated asks for aggregated discovery, or the legacy documents.
const acceptAggregated = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,application/json"

// Client is a discovery client for one API server. It reads the groups once
// and keeps them, with the resources aggregated discovery gave.
type Client struct {
	rest *rest.RESTClient

	mu        sync.Mutex
	groups    *metav1.APIGroupList
	resources map[string]*metav1.APIResourceList // by group version, from aggregated discovery
	failed    map[string]error                   // stale group versions, from aggregated discovery
}

// New returns a Client for the API server cfg names.
func New(cfg *rest.Config) (*Client, error) {
	c := rest.CopyConfig(cfg)
	c.APIPath, c.GroupVersion = "", nil
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	// decodes the API server's Status errors (401, 403, 404) into typed errors
	s := runtime.NewScheme()
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	c.NegotiatedSerializer = serializer.NewCodecFactory(s).WithoutConversion()
	rc, err := rest.UnversionedRESTClientFor(c)
	if err != nil {
		return nil, err
	}
	return &Client{rest: rc}, nil
}

// RESTClient is the client discovery requests go through.
func (c *Client) RESTClient() rest.Interface { return c.rest }

// ServerVersionWithContext returns the API server's version (GET /version).
func (c *Client) ServerVersionWithContext(ctx context.Context) (*version.Info, error) {
	b, err := c.rest.Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var v version.Info
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("reading the server version: %w", err)
	}
	return &v, nil
}

// ServerGroupsWithContext returns the API groups, the core group ("") first,
// each with its versions in the server's order of preference.
func (c *Client) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.groups != nil {
		return c.groups, nil
	}
	list := &metav1.APIGroupList{}
	resources, failed := map[string]*metav1.APIResourceList{}, map[string]error{}
	for _, path := range []string{"/api", "/apis"} {
		groups, err := c.fetchGroups(ctx, path, resources, failed)
		if err != nil {
			return nil, err
		}
		list.Groups = append(list.Groups, groups...)
	}
	c.groups, c.resources, c.failed = list, resources, failed
	return list, nil
}

// fetchGroups reads /api or /apis: aggregated, adding the resources it
// lists to resources (and stale group versions to failed), or legacy.
func (c *Client) fetchGroups(ctx context.Context, path string, resources map[string]*metav1.APIResourceList, failed map[string]error) ([]metav1.APIGroup, error) {
	var contentType string
	b, err := c.rest.Get().AbsPath(path).SetHeader("Accept", acceptAggregated).Do(ctx).ContentType(&contentType).Raw()
	if err != nil {
		return nil, fmt.Errorf("discovering API groups (%s): %w", path, err)
	}
	if aggregated(contentType) {
		var l aggregatedList
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		return l.groups(resources, failed), nil
	}
	if path == "/api" {
		var v metav1.APIVersions
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("reading /api: %w", err)
		}
		if len(v.Versions) == 0 {
			return nil, nil
		}
		g := metav1.APIGroup{}
		for _, ver := range v.Versions {
			g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{GroupVersion: ver, Version: ver})
		}
		g.PreferredVersion = g.Versions[0]
		return []metav1.APIGroup{g}, nil
	}
	var l metav1.APIGroupList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("reading /apis: %w", err)
	}
	return l.Groups, nil
}

// aggregated reports whether a response's Content-Type is aggregated
// discovery's (its parameters in any order).
func aggregated(contentType string) bool {
	params := map[string]string{}
	for _, p := range strings.Split(contentType, ";")[1:] {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			params[k] = v
		}
	}
	return params["g"] == "apidiscovery.k8s.io" && params["v"] == "v2" && params["as"] == "APIGroupDiscoveryList"
}

// ServerResourcesForGroupVersionWithContext returns the resources of one
// group version, e.g. "apps/v1" or "v1".
func (c *Client) ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	if _, err := c.ServerGroupsWithContext(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	l, err := c.resources[groupVersion], c.failed[groupVersion]
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if l != nil {
		return l, nil
	}
	path := "/apis/" + groupVersion
	if !strings.Contains(groupVersion, "/") {
		path = "/api/" + groupVersion
	}
	b, err := c.rest.Get().AbsPath(path).Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	l = &metav1.APIResourceList{}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if l.GroupVersion == "" {
		l.GroupVersion = groupVersion
	}
	return l, nil
}

// aggregatedList is the part of an apidiscovery.k8s.io/v2
// APIGroupDiscoveryList SetDrift reads.
type aggregatedList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Versions []struct {
			Version   string `json:"version"`
			Freshness string `json:"freshness"`
			Resources []struct {
				Resource         string                   `json:"resource"`
				SingularResource string                   `json:"singularResource"`
				ResponseKind     *metav1.GroupVersionKind `json:"responseKind"`
				Scope            string                   `json:"scope"`
				Verbs            []string                 `json:"verbs"`
				ShortNames       []string                 `json:"shortNames"`
				Categories       []string                 `json:"categories"`
				Subresources     []struct {
					Subresource  string                   `json:"subresource"`
					ResponseKind *metav1.GroupVersionKind `json:"responseKind"`
					Verbs        []string                 `json:"verbs"`
				} `json:"subresources"`
			} `json:"resources"`
		} `json:"versions"`
	} `json:"items"`
}

// groups converts l to API groups (versions in the server's order of
// preference, the first preferred), adding each version's resources to
// resources and each stale version's error to failed.
func (l aggregatedList) groups(resources map[string]*metav1.APIResourceList, failed map[string]error) []metav1.APIGroup {
	var out []metav1.APIGroup
	for _, item := range l.Items {
		g := metav1.APIGroup{Name: item.Metadata.Name}
		for _, v := range item.Versions {
			gv := schema.GroupVersion{Group: g.Name, Version: v.Version}.String()
			g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{GroupVersion: gv, Version: v.Version})
			if v.Freshness == "Stale" {
				failed[gv] = fmt.Errorf("the API server's discovery of %s is stale (its aggregated API server may be unavailable)", gv)
				continue
			}
			list := &metav1.APIResourceList{GroupVersion: gv}
			for _, r := range v.Resources {
				if r.ResponseKind == nil {
					continue // client-go skips these too: no kind to read them as
				}
				list.APIResources = append(list.APIResources, metav1.APIResource{
					Name: r.Resource, SingularName: r.SingularResource, Namespaced: r.Scope == "Namespaced",
					Group: r.ResponseKind.Group, Version: r.ResponseKind.Version, Kind: r.ResponseKind.Kind,
					Verbs: r.Verbs, ShortNames: r.ShortNames, Categories: r.Categories,
				})
				for _, sub := range r.Subresources {
					res := metav1.APIResource{Name: r.Resource + "/" + sub.Subresource, Namespaced: r.Scope == "Namespaced", Verbs: sub.Verbs}
					if sub.ResponseKind != nil {
						res.Group, res.Version, res.Kind = sub.ResponseKind.Group, sub.ResponseKind.Version, sub.ResponseKind.Kind
					}
					list.APIResources = append(list.APIResources, res)
				}
			}
			resources[gv] = list
		}
		if len(g.Versions) > 0 {
			g.PreferredVersion = g.Versions[0]
			out = append(out, g)
		}
	}
	return out
}
