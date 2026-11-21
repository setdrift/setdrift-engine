// removes the fields a controller wrote and no person or tool owns, since a new cluster's controllers write them again
// it reads the managedFields of each object to tell the controllers' fields from everyone else's

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

// defaultControllerManagers are the field managers of Kubernetes' own components, whose fields are removed when no one else owns them
var defaultControllerManagers = map[string]bool{
	"kube-controller-manager":  true,
	"cloud-controller-manager": true,
	"kube-scheduler":           true,
	"kubelet":                  true,
	"kube-apiserver":           true,
}

// pruneControllerFields takes an object and the controller managers and removes the fields only those controllers own
// fields a user also owns, or that sit under or inside what a user set, stay
func pruneControllerFields(o *resource.Object, controllers map[string]bool) {
	entries := o.GetManagedFields()
	// checking if the object has no managedFields, if so theres no way to tell who owns what
	if len(entries) == 0 {
		return
	}
	managers := map[string]bool{}
	controller, user := fieldpath.NewSet(), fieldpath.NewSet()
	// going through each managedFields entry to add its fields to the controller set or the user set
	for _, e := range entries {
		// invoking kube.FieldSet to read the entry's fields, its defined in internal/kube/kube.go
		s, err := kube.FieldSet(e)
		// checking if the entry couldnt be read, an unreadable entry might own anything so nothing is pruned
		if err != nil {
			return
		}
		// checking if the entry has no fields, if so theres nothing to add
		if s == nil {
			continue
		}
		// checking if the entry's manager is a controller, if so its fields go in the controller set, otherwise in the user set
		if controllers[e.Manager] {
			controller = controller.Union(s)
			managers[e.Manager] = true
		} else {
			user = user.Union(s)
		}
	}
	// checking if no user owns any field, a safety net for when cluster components wrote every field
	// then the cluster created the object, like an unknown generated object, and pruning would leave an empty invalid shell, so its kept as is
	if user.Empty() {
		// checking if controllers did write fields, if so invoking controllerCreated to say the object was kept as is, its defined below
		if !controller.Empty() {
			controllerCreated(o, managers)
		}
		return
	}

	var userPaths, userLeaves []fieldpath.Path
	user.Iterate(func(p fieldpath.Path) { userPaths = append(userPaths, p.Copy()) })
	// an empty leaf the user owns counts as the user setting everything in it
	user.Leaves().Iterate(func(p fieldpath.Path) { userLeaves = append(userLeaves, p.Copy()) })

	// userUnder tells if a user owns p or anything under it
	userUnder := func(p fieldpath.Path) bool {
		// checking each user path for one at or under p, hasPrefix is defined below
		for _, up := range userPaths {
			// checking if this user path is at or under p
			if hasPrefix(up, p) {
				return true
			}
		}
		return false
	}

	var removed []fieldpath.Path
	// going through each leaf only a controller owns, to remove it or the list item it belongs to
	controller.Difference(user).Leaves().Iterate(func(leaf fieldpath.Path) {
		// invoking liftToItem to remove an entire list item when a controller owns all of it, its defined below
		p := liftToItem(leaf, controller, userUnder)
		// checking if a user owns something under p, if so it stays
		if userUnder(p) {
			return
		}
		// checking if p is inside a value a user set in full, if so it stays
		for _, ul := range userLeaves {
			// checking if p is inside this value, if so it stays
			if hasPrefix(p, ul) {
				return
			}
		}
		// invoking keyField to check if p is a list item's key field, removing one alone would leave an item no one can address, its defined below
		if keyField(p) {
			return
		}
		// checking if p is inside a list item already removed, if so theres nothing left to remove
		for _, r := range removed {
			// checking if p is inside this removed item, if so its gone already
			if hasPrefix(p, r) {
				return
			}
		}
		// invoking removePath to delete p from the object, and remembering it when something was removed, its defined below
		if removePath(o.Object, p) {
			removed = append(removed, p.Copy())
		}
	})
}

// liftToItem takes a leaf and gives back the outermost list item on the way to it that a controller owns itself, the "." entry, with no user field beneath it, otherwise the leaf
// that way the entire item is removed rather than its fields one by one, removing the key fields first would leave the rest unaddressable
func liftToItem(leaf fieldpath.Path, controller *fieldpath.Set, userUnder func(fieldpath.Path) bool) fieldpath.Path {
	// going through each element of the path, looking for list items
	for i, pe := range leaf {
		// checking if the element is a field name, if so its not a list item
		if pe.FieldName != nil {
			continue
		}
		item := leaf[:i+1]
		// checking if a controller owns the item itself and no user owns anything under it
		if controller.Has(item) && !userUnder(item) {
			return item
		}
	}
	return leaf
}

// keyField tells if p names one of the key fields of the list item its in, like the name field of a container item keyed by name
func keyField(p fieldpath.Path) bool {
	// checking if p ends in a field name right under a keyed list item, if not it cant be a key field
	if len(p) < 2 || p[len(p)-1].FieldName == nil || p[len(p)-2].Key == nil {
		return false
	}
	// checking each of the item's key fields for the one p names
	for _, f := range *p[len(p)-2].Key {
		// checking if this key field is the one p names
		if f.Name == *p[len(p)-1].FieldName {
			return true
		}
	}
	return false
}

// controllerCreated takes an object only cluster components wrote and adds a finding saying it was exported as is, naming those managers
// namespaces are left out, the API server creates the system namespaces and a Namespace has nothing to lose
func controllerCreated(o *resource.Object, managers map[string]bool) {
	// checking if its a Namespace, if so theres no finding, namespaceGK is defined in defaults.go
	if o.GroupVersionKind().GroupKind() == namespaceGK {
		return
	}
	names := make([]string, 0, len(managers))
	// collecting the manager names, sorted below so the finding reads the same every run
	for m := range managers {
		names = append(names, m)
	}
	sort.Strings(names)
	// invoking AddFinding to warn that the object was kept as is, its defined in pkg/resource/resource.go
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "CONTROLLER_CREATED",
		"every field was written by %s, so the cluster created this object; it was exported whole, not pruned. "+
			"A new cluster likely creates it again: check whether it belongs in Git", strings.Join(names, ", "))
}

// hasPrefix tells if p starts with prefix, so p is prefix itself or something under it
func hasPrefix(p, prefix fieldpath.Path) bool {
	// checking if prefix is longer than p, if so it cant be a prefix
	if len(prefix) > len(p) {
		return false
	}
	// comparing each element of prefix with the same one in p
	for i := range prefix {
		// checking if this element differs, if so prefix isnt a prefix of p
		if !prefix[i].Equals(p[i]) {
			return false
		}
	}
	return true
}

// removePath deletes the field or list item p names from obj, and any map or list it leaves empty, and tells if something was removed
func removePath(obj map[string]any, p fieldpath.Path) bool {
	// checking if p is empty or doesnt start with a field name, if so it cant name anything in an object
	if len(p) == 0 || p[0].FieldName == nil {
		return false
	}
	// invoking removeIn to walk down and remove it, its defined below
	return removeIn(obj, p)
}

// removeIn removes p from node, which must be a map when p names a field and a list when it names a list item, and tells if something was removed
// it calls itself to walk down the path, dropping the maps it leaves empty on the way back
func removeIn(node any, p fieldpath.Path) bool {
	pe := p[0]
	// checking if node is a map or a list, anything else cant hold p
	switch n := node.(type) {
	case map[string]any:
		// checking if the element isnt a field name, if so it cant name anything in a map
		if pe.FieldName == nil {
			return false
		}
		c, ok := n[*pe.FieldName]
		// checking if the field is missing, if so theres nothing to remove
		if !ok {
			return false
		}
		// checking if this is the last element, if so the field itself goes
		if len(p) == 1 {
			delete(n, *pe.FieldName)
			return true
		}
		// checking if the field is a list and p names one of its items, if so the item is removed by rewriting the slice in its map
		if list, ok := c.([]any); ok && len(p) == 2 {
			// invoking listIndex to find the item, its defined below
			i := listIndex(list, p[1])
			// checking if the item isnt in the list, if so theres nothing to remove
			if i < 0 {
				return false
			}
			list = append(list[:i:i], list[i+1:]...)
			// checking if the list is left empty, if so the field goes too
			if len(list) == 0 {
				delete(n, *pe.FieldName)
			} else {
				n[*pe.FieldName] = list
			}
			return true
		}
		// invoking removeIn again one level down, if nothing was removed there nothing changes here
		if !removeIn(c, p[1:]) {
			return false
		}
		// invoking empty to check if the field was left empty, if so it goes too, its defined below
		if empty(n[*pe.FieldName]) {
			delete(n, *pe.FieldName)
		}
		return true
	case []any:
		// invoking listIndex to find the item p names, its defined below
		i := listIndex(n, pe)
		// checking if the item isnt there, or p ends at it, an entire item inside a nested list isnt removed here
		if i < 0 || len(p) == 1 {
			return false
		}
		// invoking removeIn again inside the item
		return removeIn(n[i], p[1:])
	}
	return false
}

// listIndex finds the list item pe names, by key fields, by value or by index, and gives back its index or -1 when its not there
func listIndex(list []any, pe fieldpath.PathElement) int {
	// checking which way pe names the item
	switch {
	case pe.Key != nil:
		// going through the items for the one whose key fields all match
		for i, item := range list {
			m, ok := item.(map[string]any)
			// checking if the item isnt a map, if so it has no key fields
			if !ok {
				continue
			}
			match := true
			// comparing each key field with the item's value for it
			for _, f := range *pe.Key {
				v, ok := m[f.Name]
				// checking if the item lacks the key field or has another value, if so its not the one
				if !ok || !value.Equals(value.NewValueInterface(v), f.Value) {
					match = false
					break
				}
			}
			// checking if every key field matched
			if match {
				return i
			}
		}
	case pe.Value != nil:
		// going through the items for the one equal to the value
		for i, item := range list {
			// checking if this item equals the value, if so its index is the answer
			if value.Equals(value.NewValueInterface(item), *pe.Value) {
				return i
			}
		}
	case pe.Index != nil:
		// checking if the index is inside the list
		if *pe.Index >= 0 && *pe.Index < len(list) {
			return *pe.Index
		}
	}
	return -1
}

// empty tells if v is a map or list with nothing in it, so removeIn can drop what a removal left empty
func empty(v any) bool {
	// checking if v is a map or a list, anything else doesnt count as empty
	switch n := v.(type) {
	case map[string]any:
		return len(n) == 0
	case []any:
		return len(n) == 0
	}
	return false
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
