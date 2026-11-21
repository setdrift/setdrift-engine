// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Fields a controller wrote and no person or tool owns: removed, as a new
// cluster's controllers write them again.

package normalize

import (
	"sort"
	"strings"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/value"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// defaultControllerManagers are the field managers of Kubernetes' own
// components, whose fields are removed when no one else owns them.
var defaultControllerManagers = map[string]bool{
	"kube-controller-manager":  true,
	"cloud-controller-manager": true,
	"kube-scheduler":           true,
	"kubelet":                  true,
	"kube-apiserver":           true,
}

// pruneControllerFields removes the fields only controllers own.
func pruneControllerFields(o *resource.Object, controllers map[string]bool) {
	entries := o.GetManagedFields()
	if len(entries) == 0 {
		return
	}
	managers := map[string]bool{}
	controller, user := fieldpath.NewSet(), fieldpath.NewSet()
	for _, e := range entries {
		s, err := kube.FieldSet(e)
		if err != nil {
			// an unreadable entry might own anything: prune nothing
			return
		}
		if s == nil {
			continue
		}
		if controllers[e.Manager] {
			controller = controller.Union(s)
			managers[e.Manager] = true
		} else {
			user = user.Union(s)
		}
	}
	// Safety net: when cluster components wrote every field, the cluster
	// created the whole object (an unknown generated object), and pruning
	// would leave an empty, invalid shell. Keep it whole and say so.
	if user.Empty() {
		if !controller.Empty() {
			controllerCreated(o, managers)
		}
		return
	}

	var userPaths, userLeaves []fieldpath.Path
	user.Iterate(func(p fieldpath.Path) { userPaths = append(userPaths, p.Copy()) })
	// a leaf the user owns that is empty counts as user set everything in it
	user.Leaves().Iterate(func(p fieldpath.Path) { userLeaves = append(userLeaves, p.Copy()) })

	userUnder := func(p fieldpath.Path) bool {
		for _, up := range userPaths {
			if hasPrefix(up, p) {
				return true
			}
		}
		return false
	}

	var removed []fieldpath.Path
	controller.Difference(user).Leaves().Iterate(func(leaf fieldpath.Path) {
		p := liftToItem(leaf, controller, userUnder)
		if userUnder(p) {
			return // a user owns something under p
		}
		for _, ul := range userLeaves {
			if hasPrefix(p, ul) {
				return // p is inside a value a user set as a whole
			}
		}
		if keyField(p) {
			return // removing a key field alone would leave an item no one can address
		}
		for _, r := range removed {
			if hasPrefix(p, r) {
				return // inside a list item already removed
			}
		}
		if removePath(o.Object, p) {
			removed = append(removed, p.Copy())
		}
	})
}

// liftToItem returns the outermost list item on the way to leaf that a
// controller owns itself (the "." entry) with no user field beneath it, so
// the whole item is removed rather than its fields one by one: removing the
// key fields first would leave the rest unaddressable. Otherwise leaf.
func liftToItem(leaf fieldpath.Path, controller *fieldpath.Set, userUnder func(fieldpath.Path) bool) fieldpath.Path {
	for i, pe := range leaf {
		if pe.FieldName != nil {
			continue
		}
		item := leaf[:i+1]
		if controller.Has(item) && !userUnder(item) {
			return item
		}
	}
	return leaf
}

// keyField reports whether p names one of the key fields of the list item
// it is in, such as the name of k:{"name":"B"}.
func keyField(p fieldpath.Path) bool {
	if len(p) < 2 || p[len(p)-1].FieldName == nil || p[len(p)-2].Key == nil {
		return false
	}
	for _, f := range *p[len(p)-2].Key {
		if f.Name == *p[len(p)-1].FieldName {
			return true
		}
	}
	return false
}

// controllerCreated adds the finding for an object only cluster components
// wrote. Namespaces are left out: the API server creates the system
// namespaces, and a Namespace has nothing to lose.
func controllerCreated(o *resource.Object, managers map[string]bool) {
	if o.GroupVersionKind().GroupKind() == namespaceGK {
		return
	}
	names := make([]string, 0, len(managers))
	for m := range managers {
		names = append(names, m)
	}
	sort.Strings(names)
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "CONTROLLER_CREATED",
		"every field was written by %s, so the cluster created this object; it was exported whole, not pruned. "+
			"A new cluster likely creates it again: check whether it belongs in Git", strings.Join(names, ", "))
}

func hasPrefix(p, prefix fieldpath.Path) bool {
	if len(prefix) > len(p) {
		return false
	}
	for i := range prefix {
		if !prefix[i].Equals(p[i]) {
			return false
		}
	}
	return true
}

// removePath deletes the field or list item p names from obj, and any map or
// list it leaves empty. Reports whether something was removed.
func removePath(obj map[string]any, p fieldpath.Path) bool {
	if len(p) == 0 || p[0].FieldName == nil {
		return false
	}
	return removeIn(obj, p)
}

// removeIn removes p from node, which must be a map when p names a field and a list when it names a list item
func removeIn(node any, p fieldpath.Path) bool {
	pe := p[0]
	switch n := node.(type) {
	case map[string]any:
		if pe.FieldName == nil {
			return false
		}
		c, ok := n[*pe.FieldName]
		if !ok {
			return false
		}
		if len(p) == 1 {
			delete(n, *pe.FieldName)
			return true
		}
		// list item is removed by rewriting the slice in its map
		if list, ok := c.([]any); ok && len(p) == 2 {
			i := listIndex(list, p[1])
			if i < 0 {
				return false
			}
			list = append(list[:i:i], list[i+1:]...)
			if len(list) == 0 {
				delete(n, *pe.FieldName)
			} else {
				n[*pe.FieldName] = list
			}
			return true
		}
		if !removeIn(c, p[1:]) {
			return false
		}
		if empty(n[*pe.FieldName]) {
			delete(n, *pe.FieldName)
		}
		return true
	case []any:
		i := listIndex(n, pe)
		if i < 0 || len(p) == 1 {
			return false
		}
		return removeIn(n[i], p[1:])
	}
	return false
}

// listIndex finds the list item pe names: by key fields, by value or by index
func listIndex(list []any, pe fieldpath.PathElement) int {
	switch {
	case pe.Key != nil:
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			match := true
			for _, f := range *pe.Key {
				v, ok := m[f.Name]
				if !ok || !value.Equals(value.NewValueInterface(v), f.Value) {
					match = false
					break
				}
			}
			if match {
				return i
			}
		}
	case pe.Value != nil:
		for i, item := range list {
			if value.Equals(value.NewValueInterface(item), *pe.Value) {
				return i
			}
		}
	case pe.Index != nil:
		if *pe.Index >= 0 && *pe.Index < len(list) {
			return *pe.Index
		}
	}
	return -1
}

func empty(v any) bool {
	switch n := v.(type) {
	case map[string]any:
		return len(n) == 0
	case []any:
		return len(n) == 0
	}
	return false
}
