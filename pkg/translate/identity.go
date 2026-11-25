package translate

import (
	"fmt"
	"slices"

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
				}
			} else {
				p.Ops = append(p.Ops, Op{Op: "add", Path: cpath + "/env", Value: env})
			}
		}
	}
	cloud := map[string]string{"aws": "AWS", "azure": "Azure"}[id.Cloud]
	home := map[string]string{"aws": "EKS", "azure": "AKS"}[id.Cloud]
	p.Why = append(p.Why, fmt.Sprintf("uses %s/%s, which keeps its %s identity: given the projected token and environment %s's webhook gives on its own platform, so the %s SDK works unchanged", id.Namespace, id.ServiceAccount, cloud, home, cloud))
	return true
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
