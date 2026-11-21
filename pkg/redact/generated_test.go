package redact

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func secretWith(data map[string]any, owners map[string]string) *resource.Object {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": "argocd-secret", "namespace": "argocd"}, "data": data}}
	var mf []metav1.ManagedFieldsEntry
	byManager := map[string][]string{}
	for k, m := range owners {
		byManager[m] = append(byManager[m], k)
	}
	for m, keys := range byManager {
		fields := `{"f:data":{`
		for i, k := range keys {
			if i > 0 {
				fields += ","
			}
			fields += `"f:` + k + `":{}`
		}
		fields += "}}"
		mf = append(mf, metav1.ManagedFieldsEntry{Manager: m, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1",
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(fields)})
	}
	u.SetManagedFields(mf)
	return resource.New(u)
}

func TestGeneratedKeys(t *testing.T) {
	o := secretWith(map[string]any{"server.secretkey": "c2s=", "admin.password": "YXA=", "webhook.github.secret": "Z2g="},
		map[string]string{"server.secretkey": "argocd-server", "admin.password": "argocd-server", "webhook.github.secret": "kubectl-patch"})
	if _, _, err := (SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	data := o.Object["data"].(map[string]any)
	if len(data) != 1 || data["webhook.github.secret"] == "Z2g=" {
		t.Errorf("data %v: want only the person's key, redacted", data)
	}
	reasons := map[string]snapshotv1alpha1.RedactionReason{}
	for _, r := range o.Redactions {
		reasons[r.Path] = r.Reason
		if r.Reason == snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE && r.Controller != "argocd-server" {
			t.Errorf("%s controller %q", r.Path, r.Controller)
		}
	}
	if reasons["data.server.secretkey"] != snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE ||
		reasons["data.webhook.github.secret"] != snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
		t.Errorf("reasons %v", reasons)
	}

	// Without managedFields (manifests from files), every key is a person's.
	plain := secretWith(map[string]any{"password": "cA=="}, nil)
	_, _, _ = (SecretRedactor{}).Process(context.Background(), []*resource.Object{plain})
	if len(plain.Redactions) != 1 || plain.Redactions[0].Reason != snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
		t.Errorf("plain %v", plain.Redactions)
	}
	// Helm and a controller both wrote it: a person's tool counts.
	shared := secretWith(map[string]any{"token": "dA=="}, map[string]string{"token": "helm"})
	_, _, _ = (SecretRedactor{}).Process(context.Background(), []*resource.Object{shared})
	if shared.Redactions[0].Reason != snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
		t.Error("helm-written key counted as generated")
	}
}

// Tools a person applies configuration with count as a person, so the keys
// they write are redacted for someone to supply rather than left out as
// generated. The names are the ones each tool's source sets.
func TestToolManagers(t *testing.T) {
	for _, m := range []string{"kubectl-client-side-apply", "helm", "argocd-controller", "kustomize-controller",
		"agentk", "fleetagent", "provider-kubernetes/my-object"} {
		if !isTool(m) {
			t.Errorf("%s is not counted as a tool", m)
		}
	}
	for _, m := range []string{"cert-manager", "external-secrets", "argocd-server", "provider"} {
		if isTool(m) {
			t.Errorf("%s is counted as a tool", m)
		}
	}
}

// A Secret a tool created empty and a controller filled in gets a finding, so
// the restore creates it first; one the controller created whole does not.
func TestFilledByController(t *testing.T) {
	filled := secretWith(map[string]any{"server.secretkey": "c2s="}, map[string]string{"server.secretkey": "argocd-server"})
	// The install manifest's apply owns the labels, not a key.
	mf := append(filled.GetManagedFields(), metav1.ManagedFieldsEntry{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply,
		APIVersion: "v1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:app":{}}}}`)})
	filled.SetManagedFields(mf)
	whole := secretWith(map[string]any{"password": "cA=="}, map[string]string{"password": "argocd-server"})
	if _, _, err := (SecretRedactor{}).Process(context.Background(), []*resource.Object{filled, whole}); err != nil {
		t.Fatal(err)
	}
	if len(filled.Findings) != 1 || filled.Findings[0].Code != "SECRET_FILLED_BY_CONTROLLER" ||
		filled.Findings[0].Message != "was created by kubectl and its values written by argocd-server, which expects it to exist: the restore creates it empty" {
		t.Errorf("filled: %v", filled.Findings)
	}
	if len(whole.Findings) != 0 {
		t.Errorf("whole: %v", whole.Findings)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
