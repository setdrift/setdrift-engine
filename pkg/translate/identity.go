package translate

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/capability"
)

// Identity is a ServiceAccount's cloud identity a translation keeps, keep-source, so its Pods call the same cloud's services from the target through federation
type Identity struct {
	Namespace, ServiceAccount string
	// Cloud is the id of the identity's cloud, like aws
	Cloud string
	// ID is the AWS role ARN, the Azure client ID, or the Google service account
	ID string
	// TenantID is the Azure tenant, when the ServiceAccount names it
	TenantID string
}

// federationToken names the projected token's volume a translation injects, federationMarker on a Pod template records what it injected, the env var names
// so a translation back to the identity's own cloud removes exactly those
const (
	federationToken  = "setdrift-federation-token"
	federationMarker = "setdrift.io/federation"
)

// keptIdentity reads the identity of a ServiceAccount from its annotations, the first cloud whose identity annotation it has, ok false when it has none
func keptIdentity(m *capability.Map, o *unstructured.Unstructured) (Identity, bool) {
	ann := o.GetAnnotations()
	id := Identity{Namespace: o.GetNamespace(), ServiceAccount: o.GetName()}
	for _, c := range m.Clouds {
		if v := ann[c.Identity.Annotation]; c.Identity.Annotation != "" && v != "" {
			id.Cloud, id.ID = c.ID, v
			if c.Identity.TenantAnnotation != "" {
				id.TenantID = ann[c.Identity.TenantAnnotation]
			}
			return id, true
		}
	}
	return id, false
}

// injectFederation adds to a Pod template using a kept identity what that cloud's webhook would have given it on its own platform
// a projected ServiceAccount token for the cloud's audience, mounted where its SDKs look, and the environment naming the identity and the token, as the cloud's YAML says
// a cloud that federates another way, like Google Cloud's credential file, gets nothing injected, its steps are in the translation's RESTORE.md
func (t *translator) injectFederation(p *Patch, o *unstructured.Unstructured, id Identity) bool {
	base, ok := podSpecPath(p.Kind)
	c := t.m.Cloud(id.Cloud)
	if !ok || c == nil || c.Identity.Federation != capability.ProjectedToken {
		return false
	}
	ci := c.Identity
	spec, _, _ := unstructured.NestedMap(o.Object, base...)
	path := "/" + joinPointer(base)
	tokenPath := ci.TokenDir + "/" + ci.TokenFile
	// the environment, each value with the identity's own filled in, an empty one left out or written as its placeholder
	var env []any
	for _, e := range ci.Env {
		v := strings.NewReplacer("{id}", id.ID, "{tenant}", id.TenantID, "{tokenPath}", tokenPath, "{region}", t.region).Replace(e.Value)
		if v == "" {
			if e.Placeholder == "" {
				continue
			}
			v = e.Placeholder
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s/%s: fill in %s in the overlay's kustomization.yaml (the ServiceAccount doesn't name its tenant)", id.Namespace, id.ServiceAccount, e.Name))
		}
		env = append(env, map[string]any{"name": e.Name, "value": v})
	}
	vol := map[string]any{"name": federationToken, "projected": map[string]any{"sources": []any{
		map[string]any{"serviceAccountToken": map[string]any{"audience": ci.Audience, "expirationSeconds": int64(86400), "path": ci.TokenFile}},
	}}}
	// the volume, appended when the Pod has volumes already
	if _, ok := spec["volumes"]; ok {
		p.Ops = append(p.Ops, Op{Op: "add", Path: path + "/volumes/-", Value: vol})
	} else {
		p.Ops = append(p.Ops, Op{Op: "add", Path: path + "/volumes", Value: []any{vol}})
	}
	var added []string
	// each container gets the mount and the environment, init containers too
	for _, field := range []string{"initContainers", "containers"} {
		cs, _ := spec[field].([]any)
		for i, c := range cs {
			cm, _ := c.(map[string]any)
			cpath := fmt.Sprintf("%s/%s/%d", path, field, i)
			mount := map[string]any{"name": federationToken, "mountPath": ci.TokenDir, "readOnly": true}
			if _, ok := cm["volumeMounts"]; ok {
				p.Ops = append(p.Ops, Op{Op: "add", Path: cpath + "/volumeMounts/-", Value: mount})
			} else {
				p.Ops = append(p.Ops, Op{Op: "add", Path: cpath + "/volumeMounts", Value: []any{mount}})
			}
			if existing, ok := cm["env"].([]any); ok {
				for _, e := range env {
					// skipping a variable the container sets itself, its own value stays
					name := e.(map[string]any)["name"]
					if slices.ContainsFunc(existing, func(x any) bool { xm, _ := x.(map[string]any); return xm["name"] == name }) {
						continue
					}
					p.Ops = append(p.Ops, Op{Op: "add", Path: cpath + "/env/-", Value: e})
					added = append(added, name.(string))
				}
			} else {
				p.Ops = append(p.Ops, Op{Op: "add", Path: cpath + "/env", Value: env})
				for _, e := range env {
					added = append(added, e.(map[string]any)["name"].(string))
				}
			}
		}
	}
	// the marker, on the Pod template, records the env vars added, so a translation back to the identity's own cloud removes only those
	slices.Sort(added)
	meta := metaPath(base)
	if anns, ok, _ := unstructured.NestedMap(o.Object, meta...); ok && anns != nil {
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/" + joinPointer(append(meta, federationMarker)), Value: strings.Join(slices.Compact(added), ",")})
	} else {
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/" + joinPointer(meta), Value: map[string]any{federationMarker: strings.Join(slices.Compact(added), ",")}})
	}
	p.Why = append(p.Why, fmt.Sprintf("uses %s/%s, which keeps its %s identity: given the projected token and environment %s's webhook gives on its own platform, so the %s SDK works unchanged", id.Namespace, id.ServiceAccount, c.Name, t.m.For(ci.Webhook).Name, c.Name))
	return true
}

// metaPath is the path to a Pod template's annotations, from its spec's path
func metaPath(base []string) []string {
	return append(append([]string{}, base[:len(base)-1]...), "metadata", "annotations")
}

// injected gives back the env var names an earlier translation injected into o's Pod template, ok false when it injected nothing
func injected(o *unstructured.Unstructured) (names []string, ok bool) {
	base, isPod := podSpecPath(o.GetKind())
	if !isPod {
		return nil, false
	}
	v, found, _ := unstructured.NestedString(o.Object, append(metaPath(base), federationMarker)...)
	if !found {
		return nil, false
	}
	return strings.Split(v, ","), true
}

// removeFederation takes out what an earlier translation injected, its token volume, the mounts, the env vars its marker names and the marker
// for a translation back to the identity's own cloud, whose webhook gives the Pods the same again, and would clash with a second mount at the same path
// a list left empty is removed whole, so the template is as it was before the first translation
func (t *translator) removeFederation(p *Patch, o *unstructured.Unstructured, names []string) {
	base, _ := podSpecPath(p.Kind)
	spec, _, _ := unstructured.NestedMap(o.Object, base...)
	path := "/" + joinPointer(base)
	// removeFrom adds the ops that remove the matching items of a list, from the last, or the whole list when nothing else is in it
	removeFrom := func(at string, list []any, match func(map[string]any) bool) {
		var hits []int
		for i, x := range list {
			if m, _ := x.(map[string]any); m != nil && match(m) {
				hits = append(hits, i)
			}
		}
		if len(hits) == 0 {
			return
		}
		if len(hits) == len(list) {
			p.Ops = append(p.Ops, Op{Op: "remove", Path: at})
			return
		}
		for i := len(hits) - 1; i >= 0; i-- {
			p.Ops = append(p.Ops, Op{Op: "remove", Path: fmt.Sprintf("%s/%d", at, hits[i])})
		}
	}
	for _, field := range []string{"initContainers", "containers"} {
		cs, _ := spec[field].([]any)
		for i, c := range cs {
			cm, _ := c.(map[string]any)
			cpath := fmt.Sprintf("%s/%s/%d", path, field, i)
			env, _ := cm["env"].([]any)
			removeFrom(cpath+"/env", env, func(m map[string]any) bool { n, _ := m["name"].(string); return slices.Contains(names, n) })
			mounts, _ := cm["volumeMounts"].([]any)
			removeFrom(cpath+"/volumeMounts", mounts, func(m map[string]any) bool { return m["name"] == federationToken })
		}
	}
	vols, _ := spec["volumes"].([]any)
	removeFrom(path+"/volumes", vols, func(m map[string]any) bool { return m["name"] == federationToken })
	meta := metaPath(base)
	anns, _, _ := unstructured.NestedMap(o.Object, meta...)
	if len(anns) == 1 {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/" + joinPointer(meta)})
	} else {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/" + joinPointer(append(meta, federationMarker))})
	}
	p.Why = append(p.Why, fmt.Sprintf("the cloud identity token and settings an earlier translation added are taken out: on %s, its own webhook gives them again", t.tgt.Name))
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
