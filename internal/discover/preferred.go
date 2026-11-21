// reads the resources of every group version and turns them into the preferred resource lists and the rest mapper the rest of SetDrift uses

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

// concurrency bounds how many per-group-version requests run at once, on servers without aggregated discovery
const concurrency = 16

// PartialError is given back along with the resources that were read when some group versions couldnt be
// thats usually an aggregated API server thats down, like metrics-server
type PartialError struct {
	// Failed holds each failed group version's error
	Failed map[string]error
}

// Error lists each failed group version with its error, sorted so the message reads the same every time
func (e *PartialError) Error() string {
	gvs := make([]string, 0, len(e.Failed))
	// collecting the failed group versions so they can be sorted
	for gv := range e.Failed {
		gvs = append(gvs, gv)
	}
	slices.Sort(gvs)
	msgs := make([]string, len(gvs))
	// pairing each group version with its error, in sorted order
	for i, gv := range gvs {
		msgs[i] = gv + ": " + e.Failed[gv].Error()
	}
	return "unable to retrieve the complete list of server APIs: " + strings.Join(msgs, ", ")
}

// IsPartial tells if err is a PartialError, meaning some resources were still read
func IsPartial(err error) bool {
	var p *PartialError
	return errors.As(err, &p)
}

// groupResources reads every version of every group, giving back the groups and the resources of each group version that could be read
// the failures come back as a *PartialError beside what was read
func groupResources(ctx context.Context, d Interface) ([]metav1.APIGroup, map[string]*metav1.APIResourceList, error) {
	// invoking ServerGroupsWithContext to list the groups, Client's is defined in discover.go
	gl, err := d.ServerGroupsWithContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	var gvs []string
	// collecting every group version of every group
	for _, g := range gl.Groups {
		// adding each of the group's versions
		for _, v := range g.Versions {
			gvs = append(gvs, v.GroupVersion)
		}
	}
	var mu sync.Mutex
	lists, failed := map[string]*metav1.APIResourceList{}, map[string]error{}
	var wg sync.WaitGroup
	// sem lets at most concurrency requests run at once
	sem := make(chan struct{}, concurrency)
	// reading each group version in its own goroutine
	for _, gv := range gvs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			// invoking ServerResourcesForGroupVersionWithContext to read the group version's resources, Client's is defined in discover.go
			l, err := d.ServerResourcesForGroupVersionWithContext(ctx, gv)
			mu.Lock()
			defer mu.Unlock()
			// checking if the read failed, if so the error is kept for the PartialError, otherwise the list is kept
			if err != nil {
				failed[gv] = err
			} else {
				lists[gv] = l
			}
		})
	}
	wg.Wait()
	// checking if the context was canceled, if so thats a terminal error rather than a partial result
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// checking if any group version failed, if so what was read goes back with a PartialError
	if len(failed) > 0 {
		return gl.Groups, lists, &PartialError{Failed: failed}
	}
	return gl.Groups, lists, nil
}

// PreferredResources gives back the resources the server serves, each in its group's preferred version, as client-go's discovery.ServerPreferredResources does
// a resource only another version serves is kept in the first version that does
// theres one list per group version read, and subresources are left out
// when some group versions couldnt be read it gives back the rest and a *PartialError
func PreferredResources(ctx context.Context, d Interface) ([]*metav1.APIResourceList, error) {
	// invoking groupResources to read every group version, its defined above
	groups, lists, err := groupResources(ctx, d)
	// checking if the error is anything but a partial one, if so its a terminal error
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	var out []*metav1.APIResourceList
	byGV := map[string]*metav1.APIResourceList{}
	// chosen maps each group resource to the group version kept for it
	chosen := map[schema.GroupResource]string{}
	// first pass, choosing the group version each resource is kept in
	for _, g := range groups {
		// going through each version of the group
		for _, v := range g.Versions {
			l := lists[v.GroupVersion]
			// checking if this group version couldnt be read, if so its skipped
			if l == nil {
				continue
			}
			byGV[v.GroupVersion] = &metav1.APIResourceList{GroupVersion: v.GroupVersion}
			out = append(out, byGV[v.GroupVersion])
			// going through each resource the group version serves
			for _, r := range l.APIResources {
				// checking if its a subresource, if so its left out
				if strings.Contains(r.Name, "/") {
					continue
				}
				gr := schema.GroupResource{Group: g.Name, Resource: r.Name}
				// checking if the resource already has a version chosen, if so only the preferred version replaces it
				if _, ok := chosen[gr]; ok && v.Version != g.PreferredVersion.Version {
					continue
				}
				chosen[gr] = v.GroupVersion
			}
		}
	}
	// second pass, adding each resource to the list of its chosen group version, in the order the server listed them within each group version
	for _, g := range groups {
		// going through each version of the group
		for _, v := range g.Versions {
			l := lists[v.GroupVersion]
			// checking if this group version couldnt be read, if so its skipped
			if l == nil {
				continue
			}
			// going through each resource the group version serves
			for _, r := range l.APIResources {
				gr := schema.GroupResource{Group: g.Name, Resource: r.Name}
				// checking if this is the group version chosen for the resource, if so its added once and dropped from chosen
				if chosen[gr] == v.GroupVersion {
					byGV[v.GroupVersion].APIResources = append(byGV[v.GroupVersion].APIResources, r)
					delete(chosen, gr)
				}
			}
		}
	}
	return out, err
}

// RESTMapper maps every kind the server serves, in every version, to its resource
// a kind asked for without a version maps to its group's preferred version
// group versions that cant be read are left out
func RESTMapper(ctx context.Context, d Interface) (meta.RESTMapper, error) {
	// invoking groupResources to read every group version, its defined above
	groups, lists, err := groupResources(ctx, d)
	// checking if the error is anything but a partial one, if so its a terminal error
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	// order holds the group versions with every group's preferred version first, so a kind without a version maps to it
	var order []schema.GroupVersion
	// adding each group's preferred version
	for _, g := range groups {
		order = append(order, schema.GroupVersion{Group: g.Name, Version: g.PreferredVersion.Version})
	}
	// adding the other versions after them
	for _, g := range groups {
		// going through each version of the group
		for _, v := range g.Versions {
			// checking if this isnt the preferred version, which is already in order
			if v.Version != g.PreferredVersion.Version {
				order = append(order, schema.GroupVersion{Group: g.Name, Version: v.Version})
			}
		}
	}
	m := meta.NewDefaultRESTMapper(order)
	// adding every resource of every group version that was read
	for _, gv := range order {
		l := lists[gv.String()]
		// checking if this group version couldnt be read, if so its skipped
		if l == nil {
			continue
		}
		// going through each resource the group version serves, to add it to the mapper
		for _, r := range l.APIResources {
			// checking if its a subresource or has no kind, if so theres nothing to map
			if strings.Contains(r.Name, "/") || r.Kind == "" {
				continue
			}
			scope := meta.RESTScopeRoot
			// checking if the resource is namespaced, if so it gets the namespace scope
			if r.Namespaced {
				scope = meta.RESTScopeNamespace
			}
			plural := gv.WithResource(r.Name)
			singular := gv.WithResource(r.SingularName)
			// checking if the server gave no singular name, if so the lowercased kind stands in for it
			if r.SingularName == "" {
				singular = gv.WithResource(strings.ToLower(r.Kind))
			}
			m.AddSpecific(gv.WithKind(r.Kind), plural, singular, scope)
		}
	}
	// checking if no group version at all could be read, if so thats a terminal error
	if len(lists) == 0 {
		// checking if some group versions failed, if so the error says which, otherwise the server listed none at all
		if err != nil {
			return nil, fmt.Errorf("discovering API resources: %w", err)
		}
		return nil, errors.New("discovering API resources: the API server lists no API groups")
	}
	return m, nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
