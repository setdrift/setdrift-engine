// Package redact removes secret values. It runs immediately after collection,
// so no later stage, snapshot or output ever sees a secret value.
package redact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Placeholder replaces every redacted value.
const Placeholder = "SETDRIFT_REDACTED"

// SecretFilledByController is the code of the finding on a Secret a tool
// created empty and a controller filled in (see filledByController).
const SecretFilledByController = "SECRET_FILLED_BY_CONTROLLER"

const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// SecretRedactor replaces the values of core Secrets with Placeholder, keeping
// the keys so the exported manifest stays valid and reviewable. It also strips
// the last-applied-configuration annotation from every object, because that
// annotation can hold a plain-text copy of a Secret.
//
// A redacted Secret must never be applied, or the placeholders would replace
// the real values. The kustomize emitter writes every resource with a
// SECRET_VALUE redaction outside the applied set (kustomize.ReferenceOnly).
//
// Secret values in other kinds (literal env values, ConfigMap keys, custom
// resource fields) are redacted the same way; see embedded.go.
type SecretRedactor struct{}

// Name implements resource.Stage.
func (SecretRedactor) Name() string { return "redact" }

// Process implements resource.Stage.
func (SecretRedactor) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	for _, o := range objs {
		isSecret := o.GroupVersionKind().Group == "" && o.GetKind() == "Secret"

		if ann := o.GetAnnotations(); ann != nil {
			if last, ok := ann[lastAppliedAnnotation]; ok {
				keepLastApplied(o, last)
				delete(ann, lastAppliedAnnotation)
				o.SetAnnotations(ann)
				if isSecret {
					o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
						Path:   "metadata.annotations." + lastAppliedAnnotation,
						Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_LAST_APPLIED_CONFIGURATION,
					})
				}
			}
		}

		if !isSecret {
			redactEmbedded(o)
			continue
		}
		secretType, _ := o.Object["type"].(string)
		if secretType == serviceAccountTokenType {
			// The token controller writes all of the data. Without it the
			// Secret is plain configuration (its type and the service
			// account annotation), and safe to apply.
			delete(o.Object, "data")
			delete(o.Object, "stringData")
			continue
		}
		if secretType == helmReleaseType {
			// Before the data is replaced: the chart it runs is the only
			// thing the restore playbook needs from it.
			o.HelmRelease = helmRelease(o)
		}
		redactMap(o, "data", true)
		redactMap(o, "stringData", false)
		filledByController(o)
	}
	return objs, nil, nil
}

// filledByController adds a finding to a Secret whose values a controller wrote but which a tool created (e.g.
// Argo CD's argocd-secret: its install manifest creates it empty, argocd-server adds its key). The controller
// expects the Secret to exist and does not create it, so a restore must create it empty first. A Secret the
// controller creates whole (argocd-initial-admin-secret) gets none: creating it first could stop the controller.
func filledByController(o *resource.Object) {
	var generatedKeys, secretKeys int
	var controllers []string
	for _, r := range o.Redactions {
		switch r.GetReason() {
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE:
			generatedKeys++
			if !slices.Contains(controllers, r.Controller) {
				controllers = append(controllers, r.Controller)
			}
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE:
			secretKeys++
		}
	}
	// A Secret with values a person wrote is created when those are restored.
	if generatedKeys == 0 || secretKeys > 0 {
		return
	}
	var tools []string
	for _, e := range o.GetManagedFields() {
		if isTool(e.Manager) && !slices.Contains(tools, e.Manager) {
			tools = append(tools, e.Manager)
		}
	}
	if len(tools) == 0 {
		return
	}
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, SecretFilledByController,
		"was created by %s and its values written by %s, which expects it to exist: the restore creates it empty",
		strings.Join(tools, ", "), strings.Join(controllers, ", "))
}

const serviceAccountTokenType = "kubernetes.io/service-account-token" //#nosec G101 -- a Secret type name, not a credential

// redactMap replaces every value in o's field. Values are base64 in data.
func redactMap(o *resource.Object, field string, encoded bool) {
	m, ok := o.Object[field].(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	owners := keyOwners(o, field)
	for _, k := range keys {
		if controller, ok := generated(owners[k]); ok {
			// The controller writes it again in a new cluster.
			delete(m, k)
			o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
				Path:       field + "." + k,
				Reason:     snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE,
				Controller: controller,
			})
			continue
		}
		v := placeholderFor(k)
		if encoded {
			v = base64.StdEncoding.EncodeToString([]byte(v))
		}
		m[k] = v
		o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
			Path:   field + "." + k,
			Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
		})
	}
}

// placeholderFor returns the placeholder for one value, before base64. Keys
// the API server validates the content of get a placeholder of the right
// shape: image pull secrets must hold JSON. Nothing of the value is kept, not
// even its registry host names.
func placeholderFor(key string) string {
	switch key {
	case ".dockerconfigjson":
		return DockerConfigJSONPlaceholder
	case ".dockercfg":
		return DockerCfgPlaceholder
	}
	return Placeholder
}

// DockerConfigJSONPlaceholder and DockerCfgPlaceholder replace an image pull
// secret's value: valid JSON, as the API server requires, naming one registry,
// SETDRIFT_REDACTED, with the auth SETDRIFT_REDACTED (base64).
const (
	DockerConfigJSONPlaceholder = `{"auths":{"SETDRIFT_REDACTED":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}}`
	DockerCfgPlaceholder        = `{"SETDRIFT_REDACTED":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}`
)

// keepLastApplied keeps a Service's last-applied configuration, parsed, on
// the object: it is the only record of which node ports a client-side
// kubectl apply set by hand (see normalize). It is never written out. Only
// Services: other kinds' copies could hold secret values.
func keepLastApplied(o *resource.Object, last string) {
	if gvk := o.GroupVersionKind(); gvk.Group != "" || gvk.Kind != "Service" {
		return
	}
	var m map[string]any
	if json.Unmarshal([]byte(last), &m) == nil {
		o.LastApplied = m
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
