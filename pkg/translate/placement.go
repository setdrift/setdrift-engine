package translate

import (
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/capability"
)

// placement rewrites the node labels a Pod template selects on and the tolerations it carries
// a node pool label keeps its pool name under the target's key, the target creating pools of the same names
// a spot label becomes the target's, with the toleration the target's spot taint needs
// any other node label of another platform goes, since no node on the target carries it, and the README says the placement was relaxed
func (t *translator) placement(p *Patch, ref string, o *unstructured.Unstructured) {
	// invoking podSpecPath to find where the Pod spec is in this kind of object, its defined below
	base, ok := podSpecPath(p.Kind)
	if !ok {
		return
	}
	fields := append(append([]string{}, base...), "nodeSelector")
	sel, _, _ := unstructured.NestedStringMap(o.Object, fields...)
	tols, _, _ := unstructured.NestedSlice(o.Object, append(append([]string{}, base...), "tolerations")...)
	if len(sel) == 0 && len(tols) == 0 {
		return
	}
	tn := t.tgt.Translate.Nodes
	newSel := map[string]any{}
	needSpotToleration := false
	changed := false
	for _, k := range slices.Sorted(maps.Keys(sel)) {
		v := sel[k]
		switch {
		// invoking isPoolKey to tell a node pool label of any platform, its defined below
		case t.isPoolKey(k) && t.foreign(t.m.NodeLabelOwners(k)) && len(tn.Pools) > 0:
			newSel[tn.Pools[0]] = v
			changed = true
			p.Why = append(p.Why, fmt.Sprintf("node pool label %s=%s becomes %s=%s: create a %s pool named %s", k, v, tn.Pools[0], v, t.tgt.Name, v))
		// invoking spotOf to tell a spot label of any platform, its defined below
		case t.spotOf(k, v) && t.foreign(t.m.NodeLabelOwners(k)) && len(tn.Spot) > 0:
			s := tn.Spot[0]
			newSel[s.Key] = s.Value
			needSpotToleration = s.Taint
			changed = true
			p.Why = append(p.Why, fmt.Sprintf("spot label %s=%s becomes %s=%s", k, v, s.Key, s.Value))
		case t.foreign(t.m.NodeLabelOwners(k)):
			changed = true
			p.Why = append(p.Why, fmt.Sprintf("node label %s=%s removed: no %s node carries it", k, v, t.tgt.Name))
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: placement relaxed, %s=%s has no %s equivalent; add a %s node label to pin it again", ref, k, v, t.tgt.Name, t.tgt.Name))
		default:
			newSel[k] = v
		}
	}
	// tolerations of another platform's spot or pool taints go, the target's spot taint is tolerated when it has one
	var newTols []any
	for _, tol := range tols {
		tm, _ := tol.(map[string]any)
		key, _ := tm["key"].(string)
		if key != "" && t.foreign(t.m.NodeLabelOwners(key)) {
			changed = true
			continue
		}
		newTols = append(newTols, tol)
	}
	if needSpotToleration {
		s := tn.Spot[0]
		newTols = append(newTols, map[string]any{"key": s.Key, "operator": "Equal", "value": s.Value, "effect": "NoSchedule"})
		p.Why = append(p.Why, fmt.Sprintf("toleration for %s's spot taint %s=%s:NoSchedule added, so the Pods can schedule on spot nodes", t.tgt.Name, s.Key, s.Value))
	}
	if !changed {
		return
	}
	path := "/" + joinPointer(base)
	p.Ops = append(p.Ops, Op{Op: "add", Path: path + "/nodeSelector", Value: newSel})
	if len(newTols) > 0 {
		p.Ops = append(p.Ops, Op{Op: "add", Path: path + "/tolerations", Value: newTols})
	} else if len(tols) > 0 {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: path + "/tolerations"})
	}
}

// podSpecPath gives back the path to the Pod spec in an object of this kind
func podSpecPath(kind string) ([]string, bool) {
	switch kind {
	case "Pod":
		return []string{"spec"}, true
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "ReplicationController":
		return []string{"spec", "template", "spec"}, true
	case "CronJob":
		return []string{"spec", "jobTemplate", "spec", "template", "spec"}, true
	}
	return nil, false
}

// isPoolKey tells if a label key names a node pool on any platform
func (t *translator) isPoolKey(k string) bool {
	return slices.ContainsFunc(t.m.Platforms, func(p capability.Platform) bool { return slices.Contains(p.Translate.Nodes.Pools, k) })
}

// spotOf tells if a label marks spot capacity on any platform
func (t *translator) spotOf(k, v string) bool {
	return slices.ContainsFunc(t.m.Platforms, func(p capability.Platform) bool {
		return slices.ContainsFunc(p.Translate.Nodes.Spot, func(s capability.SpotLabel) bool { return s.Key == k && s.Value == v })
	})
}

// joinPointer joins path parts into a JSON pointer
func joinPointer(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "/"
		}
		out += escape(p)
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
