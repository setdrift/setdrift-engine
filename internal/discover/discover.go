// Package discover asks an API server which APIs it serves, its version, its groups and the resources of each group version
// it stands in for client-go's discovery package, which imports client-go/kubernetes/scheme and with it every built-in API type, about 15 MiB of the binary
// SetDrift never uses those types since it reads objects as unstructured
// it reads aggregated discovery, one request each for /api and /apis, on by default since Kubernetes 1.30
// on servers that dont serve aggregated discovery it falls back to one request per group version
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

// Interface is what SetDrift asks of discovery, Client below implements it
// client-go's fake discovery client implements it too, so tests can use that one
type Interface interface {
	ServerVersionWithContext(ctx context.Context) (*version.Info, error)
	ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error)
	ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error)
}

// defaultTimeout bounds each discovery request when the config sets no timeout, the same as client-go's discovery client does
const defaultTimeout = 32 * time.Second

// acceptAggregated is the Accept header asking for aggregated discovery, with the legacy documents as the fallback
const acceptAggregated = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,application/json"

// Client is a discovery client for one API server
// it reads the groups once and keeps them, along with the resources aggregated discovery gave
type Client struct {
	rest *rest.RESTClient

	// guards the cached fields below, since several goroutines ask for resources at once
	mu sync.Mutex
	// the groups read on the first call, nil until then
	groups *metav1.APIGroupList
	// the resources aggregated discovery gave, keyed by group version
	resources map[string]*metav1.APIResourceList
	// the group versions aggregated discovery marked stale, with the error to give back for each
	failed map[string]error
}

// New takes a rest config and gives back a Client for the API server it names
// the config is copied, so the caller's one isnt changed
func New(cfg *rest.Config) (*Client, error) {
	c := rest.CopyConfig(cfg)
	c.APIPath, c.GroupVersion = "", nil
	// checking if the config sets no timeout, if so each request gets the default one
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	// setting up a scheme that decodes the API server's Status errors, like 401, 403 and 404, into typed errors
	s := runtime.NewScheme()
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	c.NegotiatedSerializer = serializer.NewCodecFactory(s).WithoutConversion()
	rc, err := rest.UnversionedRESTClientFor(c)
	if err != nil {
		return nil, err
	}
	return &Client{rest: rc}, nil
}

// RESTClient gives back the client discovery requests go through
func (c *Client) RESTClient() rest.Interface { return c.rest }

// ServerVersionWithContext gives back the API server's version, read from GET /version
func (c *Client) ServerVersionWithContext(ctx context.Context) (*version.Info, error) {
	b, err := c.rest.Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var v version.Info
	// checking if the response decodes as a version, an unexpected body is a terminal error
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("reading the server version: %w", err)
	}
	return &v, nil
}

// ServerGroupsWithContext gives back the API groups, the core group "" first, each with its versions in the server's order of preference
// the first call reads them and keeps them, later calls give back the kept ones
func (c *Client) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// checking if the groups were already read, if so theres no need to ask again
	if c.groups != nil {
		return c.groups, nil
	}
	list := &metav1.APIGroupList{}
	resources, failed := map[string]*metav1.APIResourceList{}, map[string]error{}
	// reading the core group from /api first, then the named groups from /apis
	for _, path := range []string{"/api", "/apis"} {
		// invoking fetchGroups to read the groups at path and collect any resources aggregated discovery gives, its defined below
		groups, err := c.fetchGroups(ctx, path, resources, failed)
		if err != nil {
			return nil, err
		}
		list.Groups = append(list.Groups, groups...)
	}
	c.groups, c.resources, c.failed = list, resources, failed
	return list, nil
}

// fetchGroups takes /api or /apis and reads the groups there, in the aggregated form when the server serves it or else the legacy one
// with the aggregated form it also adds the resources it lists to resources, and the stale group versions to failed
func (c *Client) fetchGroups(ctx context.Context, path string, resources map[string]*metav1.APIResourceList, failed map[string]error) ([]metav1.APIGroup, error) {
	var contentType string
	b, err := c.rest.Get().AbsPath(path).SetHeader("Accept", acceptAggregated).Do(ctx).ContentType(&contentType).Raw()
	if err != nil {
		return nil, fmt.Errorf("discovering API groups (%s): %w", path, err)
	}
	// invoking aggregated and checking if the server answered with aggregated discovery, its defined below
	if aggregated(contentType) {
		var l aggregatedList
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		// invoking groups to turn the aggregated list into groups and fill resources and failed, its defined below
		return l.groups(resources, failed), nil
	}
	// checking if this is the legacy /api document, which lists only the core group's versions
	if path == "/api" {
		var v metav1.APIVersions
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("reading /api: %w", err)
		}
		// checking if the server lists no core versions, if so theres no core group to add
		if len(v.Versions) == 0 {
			return nil, nil
		}
		g := metav1.APIGroup{}
		// building the core group from each version listed
		for _, ver := range v.Versions {
			g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{GroupVersion: ver, Version: ver})
		}
		// the first version listed is the preferred one
		g.PreferredVersion = g.Versions[0]
		return []metav1.APIGroup{g}, nil
	}
	var l metav1.APIGroupList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("reading /apis: %w", err)
	}
	return l.Groups, nil
}

// aggregated takes a response's Content-Type and tells if its aggregated discovery's, with its parameters in any order
func aggregated(contentType string) bool {
	params := map[string]string{}
	// reading each key=value parameter after the media type
	for _, p := range strings.Split(contentType, ";")[1:] {
		// checking if the parameter has an =, one without is skipped
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			params[k] = v
		}
	}
	return params["g"] == "apidiscovery.k8s.io" && params["v"] == "v2" && params["as"] == "APIGroupDiscoveryList"
}

// ServerResourcesForGroupVersionWithContext takes one group version, like "apps/v1" or "v1", and gives back its resources
// it uses what aggregated discovery gave when it can, otherwise it asks the server for that group version
func (c *Client) ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	// invoking ServerGroupsWithContext so the groups and any aggregated resources are read first, its defined above
	if _, err := c.ServerGroupsWithContext(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	l, err := c.resources[groupVersion], c.failed[groupVersion]
	c.mu.Unlock()
	// checking if aggregated discovery marked this group version stale, if so its error goes back
	if err != nil {
		return nil, err
	}
	// checking if aggregated discovery already gave its resources, if so theres no request to make
	if l != nil {
		return l, nil
	}
	path := "/apis/" + groupVersion
	// checking if the group version has no group, if so its in the core group under /api
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
	// checking if the server left the group version out of the list, if so its filled in from the one asked for
	if l.GroupVersion == "" {
		l.GroupVersion = groupVersion
	}
	return l, nil
}

// aggregatedList is the part of an apidiscovery.k8s.io/v2 APIGroupDiscoveryList that SetDrift reads
// each item is a group, with its versions in the server's order of preference and the resources of each
type aggregatedList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Versions []struct {
			Version string `json:"version"`
			// set to Stale when the server couldnt refresh this version, usually an aggregated API server thats down
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

// groups turns l into API groups, versions in the server's order of preference with the first one preferred
// it adds each version's resources to resources and each stale version's error to failed, so later lookups need no request
func (l aggregatedList) groups(resources map[string]*metav1.APIResourceList, failed map[string]error) []metav1.APIGroup {
	var out []metav1.APIGroup
	// going through each group the server listed
	for _, item := range l.Items {
		g := metav1.APIGroup{Name: item.Metadata.Name}
		// going through each version of the group
		for _, v := range item.Versions {
			gv := schema.GroupVersion{Group: g.Name, Version: v.Version}.String()
			g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{GroupVersion: gv, Version: v.Version})
			// checking if the version is stale, if so it gets an error instead of resources
			if v.Freshness == "Stale" {
				failed[gv] = fmt.Errorf("the API server's discovery of %s is stale (its aggregated API server may be unavailable)", gv)
				continue
			}
			list := &metav1.APIResourceList{GroupVersion: gv}
			// going through each resource of the version
			for _, r := range v.Resources {
				// checking if the resource has no kind, if so its skipped since theres no kind to read it as, client-go skips these too
				if r.ResponseKind == nil {
					continue
				}
				list.APIResources = append(list.APIResources, metav1.APIResource{
					Name: r.Resource, SingularName: r.SingularResource, Namespaced: r.Scope == "Namespaced",
					Group: r.ResponseKind.Group, Version: r.ResponseKind.Version, Kind: r.ResponseKind.Kind,
					Verbs: r.Verbs, ShortNames: r.ShortNames, Categories: r.Categories,
				})
				// adding each subresource as resource/subresource, the way the legacy documents list them
				for _, sub := range r.Subresources {
					res := metav1.APIResource{Name: r.Resource + "/" + sub.Subresource, Namespaced: r.Scope == "Namespaced", Verbs: sub.Verbs}
					// checking if the subresource has its own kind, if so it gets recorded
					if sub.ResponseKind != nil {
						res.Group, res.Version, res.Kind = sub.ResponseKind.Group, sub.ResponseKind.Version, sub.ResponseKind.Kind
					}
					list.APIResources = append(list.APIResources, res)
				}
			}
			resources[gv] = list
		}
		// checking if the group has any versions, if so the first is preferred and the group is kept
		if len(g.Versions) > 0 {
			g.PreferredVersion = g.Versions[0]
			out = append(out, g)
		}
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
