package discover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// concurrency bounds the per-group-version requests made on servers
// without aggregated discovery.
const concurrency = 16

// PartialError is returned with the resources that were read when some
// group versions could not be (an aggregated API server that is down, such
// as metrics-server).
type PartialError struct {
	// Failed holds each group version's error.
	Failed map[string]error
}

func (e *PartialError) Error() string {
	gvs := make([]string, 0, len(e.Failed))
	for gv := range e.Failed {
		gvs = append(gvs, gv)
	}
	slices.Sort(gvs)
	msgs := make([]string, len(gvs))
	for i, gv := range gvs {
		msgs[i] = gv + ": " + e.Failed[gv].Error()
	}
	return "unable to retrieve the complete list of server APIs: " + strings.Join(msgs, ", ")
}

// IsPartial reports whether err is a PartialError.
func IsPartial(err error) bool {
	var p *PartialError
	return errors.As(err, &p)
}

// groupResources reads every version of every group: the groups, and the
// resources of each group version that could be read. Failures are returned
// as a *PartialError beside what was read.
func groupResources(ctx context.Context, d Interface) ([]metav1.APIGroup, map[string]*metav1.APIResourceList, error) {
	gl, err := d.ServerGroupsWithContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	var gvs []string
	for _, g := range gl.Groups {
		for _, v := range g.Versions {
			gvs = append(gvs, v.GroupVersion)
		}
	}
	var mu sync.Mutex
	lists, failed := map[string]*metav1.APIResourceList{}, map[string]error{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, gv := range gvs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			l, err := d.ServerResourcesForGroupVersionWithContext(ctx, gv)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed[gv] = err
			} else {
				lists[gv] = l
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(failed) > 0 {
		return gl.Groups, lists, &PartialError{Failed: failed}
	}
	return gl.Groups, lists, nil
}

// PreferredResources returns the resources the server serves, each in its
// group's preferred version (or, for a resource only another version
// serves, the first version that does), as client-go's
// discovery.ServerPreferredResources does: one list per group version read,
// subresources left out. When some group versions could not be read, it
// returns the rest and a *PartialError.
func PreferredResources(ctx context.Context, d Interface) ([]*metav1.APIResourceList, error) {
	groups, lists, err := groupResources(ctx, d)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	var out []*metav1.APIResourceList
	byGV := map[string]*metav1.APIResourceList{}
	chosen := map[schema.GroupResource]string{} // group resource -> the group version kept
	for _, g := range groups {
		for _, v := range g.Versions {
			l := lists[v.GroupVersion]
			if l == nil {
				continue
			}
			byGV[v.GroupVersion] = &metav1.APIResourceList{GroupVersion: v.GroupVersion}
			out = append(out, byGV[v.GroupVersion])
			for _, r := range l.APIResources {
				if strings.Contains(r.Name, "/") {
					continue
				}
				gr := schema.GroupResource{Group: g.Name, Resource: r.Name}
				if _, ok := chosen[gr]; ok && v.Version != g.PreferredVersion.Version {
					continue // only the preferred version replaces one already chosen
				}
				chosen[gr] = v.GroupVersion
			}
		}
	}
	// in the order the server listed them, within each group version
	for _, g := range groups {
		for _, v := range g.Versions {
			l := lists[v.GroupVersion]
			if l == nil {
				continue
			}
			for _, r := range l.APIResources {
				gr := schema.GroupResource{Group: g.Name, Resource: r.Name}
				if chosen[gr] == v.GroupVersion {
					byGV[v.GroupVersion].APIResources = append(byGV[v.GroupVersion].APIResources, r)
					delete(chosen, gr)
				}
			}
		}
	}
	return out, err
}

// RESTMapper maps every kind the server serves, in every version, to its
// resource; a kind asked for without a version maps to its group's
// preferred version. Group versions that can't be read are left out.
func RESTMapper(ctx context.Context, d Interface) (meta.RESTMapper, error) {
	groups, lists, err := groupResources(ctx, d)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	var order []schema.GroupVersion // preferred versions first
	for _, g := range groups {
		order = append(order, schema.GroupVersion{Group: g.Name, Version: g.PreferredVersion.Version})
	}
	for _, g := range groups {
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				order = append(order, schema.GroupVersion{Group: g.Name, Version: v.Version})
			}
		}
	}
	m := meta.NewDefaultRESTMapper(order)
	for _, gv := range order {
		l := lists[gv.String()]
		if l == nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || r.Kind == "" {
				continue
			}
			scope := meta.RESTScopeRoot
			if r.Namespaced {
				scope = meta.RESTScopeNamespace
			}
			plural := gv.WithResource(r.Name)
			singular := gv.WithResource(r.SingularName)
			if r.SingularName == "" {
				singular = gv.WithResource(strings.ToLower(r.Kind))
			}
			m.AddSpecific(gv.WithKind(r.Kind), plural, singular, scope)
		}
	}
	if len(lists) == 0 {
		return nil, fmt.Errorf("discovering API resources: %w", err)
	}
	return m, nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
