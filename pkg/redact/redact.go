// Package redact drops secret values right after collect, keeping only key names, so no later stage, snapshot or output ever sees a secret value
// each value is replaced with a placeholder the moment its recognized, nothing of it is kept, this file handles core Secrets and the last-applied annotation
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

// Placeholder is what every redacted value is replaced with
const Placeholder = "SETDRIFT_REDACTED"

// SecretFilledByController is the code of the finding on a Secret a tool created empty and a controller filled in, filledByController below adds it
const SecretFilledByController = "SECRET_FILLED_BY_CONTROLLER"

// lastAppliedAnnotation is the annotation kubectl apply leaves on objects, it can hold a plain-text copy of a Secret so its always removed
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// SecretRedactor is the pipeline stage that replaces the values of core Secrets with Placeholder, keeping the keys so the exported manifest stays valid and reviewable
// it also strips the last-applied-configuration annotation from every object, because that annotation can hold a plain-text copy of a Secret
// a redacted Secret must never be applied, or the placeholders would replace the real values, so the kustomize emitter writes every resource with a SECRET_VALUE redaction outside the applied set (kustomize.ReferenceOnly)
// secret values in other kinds, like literal env values, ConfigMap keys and custom resource fields, are redacted the same way in embedded.go
type SecretRedactor struct{}

// Name gives back the stage name, redact, as the resource.Stage interface asks
func (SecretRedactor) Name() string { return "redact" }

// Process takes the collected objects and drops the secret values in each one, in place, as the resource.Stage interface asks
// Secrets get their data and stringData replaced, every other kind goes through redactEmbedded
func (SecretRedactor) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	// going through every object, the secret values are gone from each before the next stage sees it
	for _, o := range objs {
		isSecret := o.GroupVersionKind().Group == "" && o.GetKind() == "Secret"

		// checking if the object has annotations at all
		if ann := o.GetAnnotations(); ann != nil {
			// checking if it carries the last-applied annotation, if so its removed from every kind
			if last, ok := ann[lastAppliedAnnotation]; ok {
				// invoking keepLastApplied to hold on to a Service's copy in memory for its node ports, its defined below
				keepLastApplied(o, last)
				delete(ann, lastAppliedAnnotation)
				o.SetAnnotations(ann)
				// checking if its a Secret, if so the removal is recorded as a redaction
				if isSecret {
					o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
						Path:   "metadata.annotations." + lastAppliedAnnotation,
						Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_LAST_APPLIED_CONFIGURATION,
					})
				}
			}
		}

		// checking if its anything but a Secret, if so its fields are searched for secret values instead
		if !isSecret {
			// invoking redactEmbedded to replace secret values found in its fields, its defined in embedded.go
			redactEmbedded(o)
			continue
		}
		secretType, _ := o.Object["type"].(string)
		// checking if its a service account token, if so all of its data is dropped
		if secretType == serviceAccountTokenType {
			// the token controller writes all of the data
			// without it the Secret is configuration only, its type and the service account annotation, and safe to apply
			delete(o.Object, "data")
			delete(o.Object, "stringData")
			continue
		}
		// checking if its a Helm release record, if so the chart it runs is read before the data is replaced
		// thats the only thing the restore playbook needs from it
		if secretType == helmReleaseType {
			// invoking helmRelease to read the chart metadata, never the values, its defined in helm.go
			o.HelmRelease = helmRelease(o)
		}
		// invoking redactMap on data, which is base64, and on stringData, which isnt, its defined below
		redactMap(o, "data", true)
		redactMap(o, "stringData", false)
		// invoking filledByController to flag a Secret a restore must create empty first, its defined below
		filledByController(o)
	}
	return objs, nil, nil
}

// filledByController adds a finding to a Secret whose values a controller wrote but which a tool created
// e.g. Argo CD's argocd-secret, its install manifest creates it empty and argocd-server adds its key
// the controller expects the Secret to exist and doesnt create it, so a restore must create it empty first
// a Secret the controller creates with its values, like argocd-initial-admin-secret, gets none, creating it first could stop the controller
func filledByController(o *resource.Object) {
	var generatedKeys, secretKeys int
	var controllers []string
	// counting the keys a controller generated and the ones a person wrote, from the redactions redactMap recorded
	for _, r := range o.Redactions {
		// checking why each key was redacted
		switch r.GetReason() {
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE:
			generatedKeys++
			// checking if this controller is already named, so each is listed once
			if !slices.Contains(controllers, r.Controller) {
				controllers = append(controllers, r.Controller)
			}
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE:
			secretKeys++
		}
	}
	// checking if no key was generated, or a person wrote some, if so theres nothing to flag
	// a Secret with values a person wrote is created when those are restored
	if generatedKeys == 0 || secretKeys > 0 {
		return
	}
	var tools []string
	// collecting the apply tools among the Secret's field managers, each one once
	for _, e := range o.GetManagedFields() {
		// invoking isTool and checking if this manager is a tool not yet listed, its defined in generated.go
		if isTool(e.Manager) && !slices.Contains(tools, e.Manager) {
			tools = append(tools, e.Manager)
		}
	}
	// checking if no tool touched it, if so the controller created it with its values and gets no finding
	if len(tools) == 0 {
		return
	}
	// invoking AddFinding to attach the finding the restore reads, its defined in pkg/resource/resource.go
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, SecretFilledByController,
		"was created by %s and its values written by %s, which expects it to exist: the restore creates it empty",
		strings.Join(tools, ", "), strings.Join(controllers, ", "))
}

// serviceAccountTokenType is the type of the Secrets the token controller fills, Process drops all their data
const serviceAccountTokenType = "kubernetes.io/service-account-token" //#nosec G101 -- a Secret type name, not a credential

// redactMap takes a Secret and one of its fields, data or stringData, and drops every value in it, encoded says the values are base64 like in data
// a key a controller generated is deleted outright, since the controller writes it again in a new cluster, every other key keeps its name with a placeholder value
func redactMap(o *resource.Object, field string, encoded bool) {
	m, ok := o.Object[field].(map[string]any)
	// checking if the field is missing or not a map, if so theres nothing to redact
	if !ok {
		return
	}
	keys := make([]string, 0, len(m))
	// collecting the keys so they can be sorted, the redactions then come out in the same order every run
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// invoking keyOwners to find the field managers of each key, its defined in generated.go
	owners := keyOwners(o, field)
	// replacing each key's value, in sorted order
	for _, k := range keys {
		// invoking generated and checking if only controllers own this key, if so its deleted, its defined in generated.go
		if controller, ok := generated(owners[k]); ok {
			// the controller writes it again in a new cluster
			delete(m, k)
			o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
				Path:       field + "." + k,
				Reason:     snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE,
				Controller: controller,
			})
			continue
		}
		// invoking placeholderFor to pick a placeholder of the shape this key needs, its defined below
		v := placeholderFor(k)
		// checking if the field holds base64, if so the placeholder is encoded the same way
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

// placeholderFor takes a Secret key and gives back the placeholder for its value, before base64
// keys the API server validates the content of get a placeholder of the right shape, image pull secrets must hold JSON
// nothing of the value is kept, not even its registry host names
func placeholderFor(key string) string {
	// checking if its one of the image pull secret keys, those get a JSON placeholder
	switch key {
	case ".dockerconfigjson":
		return DockerConfigJSONPlaceholder
	case ".dockercfg":
		return DockerCfgPlaceholder
	}
	return Placeholder
}

// DockerConfigJSONPlaceholder and DockerCfgPlaceholder replace an image pull secret's value
// both are valid JSON, as the API server requires, naming one registry, SETDRIFT_REDACTED, with the auth SETDRIFT_REDACTED in base64
const (
	DockerConfigJSONPlaceholder = `{"auths":{"SETDRIFT_REDACTED":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}}`
	DockerCfgPlaceholder        = `{"SETDRIFT_REDACTED":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}`
)

// keepLastApplied takes an object and its last-applied annotation, and keeps a Service's copy parsed on the object
// its the only record of which node ports a client-side kubectl apply set by hand, pkg/normalize/defaults.go reads it, and its never written out
// only Services keep it, other kinds' copies could hold secret values
func keepLastApplied(o *resource.Object, last string) {
	// checking if its anything but a core Service, if so the copy is not kept
	if gvk := o.GroupVersionKind(); gvk.Group != "" || gvk.Kind != "Service" {
		return
	}
	var m map[string]any
	// checking if the annotation parses as JSON, one that doesnt is left out
	if json.Unmarshal([]byte(last), &m) == nil {
		o.LastApplied = m
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
