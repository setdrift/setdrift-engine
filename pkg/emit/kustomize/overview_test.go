// tests for the overview of what an export found

package kustomize

import (
	"strings"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// check that the overview counts what was applied by hand without the built-in namespaces, lists what was held back and the cleanup suggestions, and writes no section when theres nothing to say
func TestOverview(t *testing.T) {
	unmanaged := &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_UNMANAGED}
	helm := &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_HELM, Manager: "web/podinfo"}
	secretValue := []*snapshotv1alpha1.Redaction{{Path: "spec.template.spec.containers[api].env[API_KEY].value", Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE}}
	s := &snapshotv1alpha1.Snapshot{Resources: []*snapshotv1alpha1.Resource{
		{ApiVersion: "v1", Kind: "Namespace", Name: "kube-system", Provenance: unmanaged},
		{ApiVersion: "v1", Kind: "Namespace", Name: "shop", Provenance: unmanaged},
		{ApiVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "api", Provenance: unmanaged, Redactions: secretValue},
		{ApiVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "debug", Provenance: unmanaged},
		{ApiVersion: "apps/v1", Kind: "Deployment", Namespace: "web", Name: "podinfo", Provenance: helm},
		{ApiVersion: "batch/v1", Kind: "CronJob", Namespace: "shop", Name: "cleanup", Provenance: unmanaged,
			Findings: []*snapshotv1alpha1.Finding{{Code: "CRONJOB_HISTORY_LIMIT", Message: "keeps 100 Jobs"}}},
	}}
	o := NewOverview(s, false)
	if got := o.ByHandCount(); got != 4 {
		t.Errorf("by hand %d, want 4 (kube-system is built in): %v", got, o.ByHand)
	}
	if got := Grouped(o.ByHand["shop"]); got != "CronJob cleanup; Deployment api, debug" {
		t.Errorf("shop: %q", got)
	}
	if len(o.HeldBack) != 1 || o.HeldBack[0].Resource != "Deployment shop/api" || !strings.Contains(o.HeldBack[0].Fields[0], "API_KEY") {
		t.Errorf("held back %+v", o.HeldBack)
	}
	if len(o.Cleanup) != 1 {
		t.Errorf("cleanup %v", o.Cleanup)
	}
	md := o.markdown()
	for _, want := range []string{"Applied by hand: 4 resources", "| shop | CronJob cleanup; Deployment api, debug |", "Deployment shop/api", "keeps 100 Jobs"} {
		if !strings.Contains(md, want) {
			t.Errorf("README section lacks %q:\n%s", want, md)
		}
	}
	if NewOverview(&snapshotv1alpha1.Snapshot{}, false).markdown() != "" {
		t.Error("empty overview writes a section")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
