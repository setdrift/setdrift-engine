package translate

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Identity is a ServiceAccount's cloud identity a translation keeps, keep-source, so its Pods call the same cloud's services from the target through federation
type Identity struct {
	Namespace, ServiceAccount string
	// Cloud is aws, azure or gcp
	Cloud string
	// ID is the AWS role ARN, the Azure client ID, or the Google service account
	ID string
	// TenantID is the Azure tenant, when the ServiceAccount names it
	TenantID string
}

// what each cloud's own webhook injects on its platform, and a translation injects itself on another, since no webhook there does
const (
	awsTokenDir     = "/var/run/secrets/eks.amazonaws.com/serviceaccount"
	awsAudience     = "sts.amazonaws.com"
	azureTokenDir   = "/var/run/secrets/azure/tokens"
	azureTokenFile  = "azure-identity-token"
	azureAudience   = "api://AzureADTokenExchange"
	federationToken = "setdrift-federation-token"
	// federationMarker on a Pod template records what a translation injected, the env var names, so a translation back to the identity's own cloud removes exactly those
	federationMarker = "setdrift.io/federation"
)

// keptIdentity reads the identity of a ServiceAccount from its annotations, the cloud it belongs to and its id, ok false when it has none the translation injects
func keptIdentity(o *unstructured.Unstructured) (Identity, bool) {
	ann := o.GetAnnotations()
	id := Identity{Namespace: o.GetNamespace(), ServiceAccount: o.GetName()}
	switch {
	case ann["eks.amazonaws.com/role-arn"] != "":
		id.Cloud, id.ID = "aws", ann["eks.amazonaws.com/role-arn"]
	case ann["azure.workload.identity/client-id"] != "":
		id.Cloud, id.ID, id.TenantID = "azure", ann["azure.workload.identity/client-id"], ann["azure.workload.identity/tenant-id"]
	case ann["iam.gke.io/gcp-service-account"] != "":
		id.Cloud, id.ID = "gcp", ann["iam.gke.io/gcp-service-account"]
	default:
		return id, false
	}
	return id, true
}

// injectFederation adds to a Pod template using a kept identity what that cloud's webhook would have given it on its own platform
// a projected ServiceAccount token for the cloud's audience, mounted where its SDKs look, and the environment naming the identity and the token
// Google Cloud's federation needs a credential configuration file instead, so its steps are in the translation's RESTORE.md and nothing is injected
func (t *translator) injectFederation(p *Patch, o *unstructured.Unstructured, id Identity) bool {
	base, ok := podSpecPath(p.Kind)
	if !ok || id.Cloud == "gcp" {
		return false
	}
	spec, _, _ := unstructured.NestedMap(o.Object, base...)
	path := "/" + joinPointer(base)
	audience, dir, file := awsAudience, awsTokenDir, "token"
	env := []any{
		map[string]any{"name": "AWS_ROLE_ARN", "value": id.ID},
		map[string]any{"name": "AWS_WEB_IDENTITY_TOKEN_FILE", "value": awsTokenDir + "/token"},
		map[string]any{"name": "AWS_STS_REGIONAL_ENDPOINTS", "value": "regional"},
	}
	if t.region != "" {
		env = append(env, map[string]any{"name": "AWS_REGION", "value": t.region}, map[string]any{"name": "AWS_DEFAULT_REGION", "value": t.region})
	}
	if id.Cloud == "azure" {
		audience, dir, file = azureAudience, azureTokenDir, azureTokenFile
		tenant := id.TenantID
		if tenant == "" {
			tenant = "<Azure tenant ID>"
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s/%s: fill in AZURE_TENANT_ID in the overlay's kustomization.yaml (the ServiceAccount doesn't name its tenant)", id.Namespace, id.ServiceAccount))
		}
		env = []any{
			map[string]any{"name": "AZURE_CLIENT_ID", "value": id.ID},
			map[string]any{"name": "AZURE_TENANT_ID", "value": tenant},
			map[string]any{"name": "AZURE_FEDERATED_TOKEN_FILE", "value": azureTokenDir + "/" + azureTokenFile},
			map[string]any{"name": "AZURE_AUTHORITY_HOST", "value": "https://login.microsoftonline.com/"},
		}
	}
	vol := map[string]any{"name": federationToken, "projected": map[string]any{"sources": []any{
		map[string]any{"serviceAccountToken": map[string]any{"audience": audience, "expirationSeconds": int64(86400), "path": file}},
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
			mount := map[string]any{"name": federationToken, "mountPath": dir, "readOnly": true}
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
	cloud := map[string]string{"aws": "AWS", "azure": "Azure"}[id.Cloud]
	home := map[string]string{"aws": "EKS", "azure": "AKS"}[id.Cloud]
	p.Why = append(p.Why, fmt.Sprintf("uses %s/%s, which keeps its %s identity: given the projected token and environment %s's webhook gives on its own platform, so the %s SDK works unchanged", id.Namespace, id.ServiceAccount, cloud, home, cloud))
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
