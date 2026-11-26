package diff

import (
	"strings"
	"testing"
)

// key builds a test key
func key(kind, ns, name string) Key { return Key{Kind: kind, Namespace: ns, Name: name} }

// a field the translation changed is sorted out, a change on either side since stays real with why, a decided object the target changed is real too, an exported object left out is translated, a Secret is not moved
func TestApplyTranslation(t *testing.T) {
	svc := key("Service", "shop", "web")
	dep := key("Deployment", "shop", "api")
	sa := key("ServiceAccount", "shop", "orders")
	sec := key("Secret", "shop", "db")
	cm := key("ConfigMap", "kube-system", "amazon-vpc-cni")
	eq := key("StorageBucket", "shop", "orders")
	f := func(path string, keys []string, a, b any) Field { return Field{Path: path, Keys: keys, A: a, B: b} }
	tr := Translation{
		Code: "te2g",
		Exported: map[Key]any{
			svc: map[string]any{"spec": map[string]any{"loadBalancerClass": "eks.amazonaws.com/nlb"}},
			dep: map[string]any{"spec": map[string]any{"replicas": 3.0, "paused": false}},
			sa:  map[string]any{"metadata": map[string]any{"name": "orders"}},
			sec: map[string]any{}, cm: map[string]any{},
		},
		Translated: map[Key]any{
			svc: map[string]any{"spec": map[string]any{}},
			dep: map[string]any{"spec": map[string]any{"replicas": 3.0, "paused": false}},
			sa:  map[string]any{"metadata": map[string]any{"name": "orders", "labels": map[string]any{"x": "y"}}},
			eq:  map[string]any{},
		},
		NotMoved: func(k Key) string {
			if k == sec {
				return "secret values"
			}
			return ""
		},
		Explain: func(m any) string {
			if md, _ := m.(map[string]any)["metadata"].(map[string]any); md != nil && md["name"] == "orders" {
				return "decided: keep-source"
			}
			return ""
		},
	}
	r := Result{
		A: Side{Label: "eks"}, B: Side{Label: "gke"},
		Changed: []Change{
			{A: Object{Key: svc}, B: Object{Key: svc}, Fields: []Field{f("spec.loadBalancerClass", []string{"f:spec", "f:loadBalancerClass"}, "eks.amazonaws.com/nlb", nil)}},
			{A: Object{Key: dep}, B: Object{Key: dep}, Fields: []Field{
				f("spec.replicas", []string{"f:spec", "f:replicas"}, 5.0, 3.0),
				f("spec.paused", []string{"f:spec", "f:paused"}, false, true),
			}},
			{A: Object{Key: sa}, B: Object{Key: sa}, Fields: []Field{f("metadata.labels.z", []string{"f:metadata", "f:labels", "f:z"}, nil, "1")}},
		},
		OnlyA: []Object{{Key: sec}, {Key: cm}, {Key: key("ConfigMap", "shop", "new")}},
		OnlyB: []Object{{Key: eq}, {Key: key("ConfigMap", "shop", "extra")}},
	}
	r.ApplyTranslation(tr)
	// the decided ServiceAccount the target changed since is real: a decision holds only while the target is as translated
	if r.Translation != "te2g" || r.AsTranslated != 1 || len(r.Changed) != 2 || r.Changed[0].A.Key != dep || len(r.Changed[0].Fields) != 2 || r.Changed[1].A.Key != sa {
		t.Fatalf("changed %+v, as translated %d", r.Changed, r.AsTranslated)
	}
	if got := strings.Join(r.Changed[0].Reasons, "|"); !strings.Contains(got, "changed on the source after the export") || !strings.Contains(got, "differs on the target from the translation") {
		t.Errorf("reasons %q", got)
	}
	if len(r.Translated) != 3 || len(r.Decided) != 0 || len(r.NotMoved) != 1 {
		t.Errorf("translated %+v decided %+v not moved %+v", r.Translated, r.Decided, r.NotMoved)
	}
	if len(r.OnlyA) != 1 || r.OnlyA[0].Key.Name != "new" || len(r.OnlyB) != 1 || r.OnlyB[0].Key.Name != "extra" {
		t.Errorf("only a %+v only b %+v", r.OnlyA, r.OnlyB)
	}
	if r.Same() {
		t.Error("real differences remain")
	}
}

// the translation's stamp on a CronJob's job template, and the empty maps it alone was in, are no difference
func TestTranslationMarkOnJobTemplate(t *testing.T) {
	a := map[string]any{"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{}}}}}}
	b := map[string]any{"spec": map[string]any{"jobTemplate": map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"setdrift.io/translation": "te2g"}},
		"spec":     map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"setdrift.io/translation": "te2g"}}, "spec": map[string]any{}}},
	}}}
	if f := Fields(WithoutTranslationMark(a), WithoutTranslationMark(b)); len(f) != 0 {
		t.Errorf("%+v", f)
	}
}

// a claim that names no StorageClass lands on each cluster's default, which a translation to another platform expects, so its bound volume differing isn't counted
func TestClaimWithoutClass(t *testing.T) {
	pvc := Key{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "scratch"}
	claim := map[string]any{"spec": map[string]any{"resources": map[string]any{}}}
	tr := Translation{Code: "te2g", Exported: map[Key]any{pvc: claim}, Translated: map[Key]any{pvc: claim}}
	r := Result{Changed: []Change{{A: Object{Key: pvc}, B: Object{Key: pvc}, Fields: []Field{{Path: "(volume) storageClass", A: "gp3", B: "standard-rwo"}}}}}
	r.ApplyTranslation(tr)
	if len(r.Changed) != 0 || r.AsTranslated != 1 {
		t.Errorf("%+v", r)
	}
}

// what wont work on the source was already so before the translation, so its shown apart and not counted
func TestSourceWontWork(t *testing.T) {
	sa := Key{Kind: "ServiceAccount", Namespace: "shop", Name: "orders"}
	r := Result{A: Side{Label: "aks"}, B: Side{Label: "eks"}, WontWork: []WontWork{{Side: "aks", Object: Object{Key: sa}, Reasons: []string{"names EKS"}}}}
	r.ApplyTranslation(Translation{Code: "ta2e", Exported: map[Key]any{}, Translated: map[Key]any{}})
	if len(r.WontWork) != 0 || len(r.OnSource) != 1 || !r.Same() {
		t.Errorf("%+v", r)
	}
}

// a kept identity named only by its annotation is decided, and says the step outside the cluster, not that nothing gives its Pods credentials
// one with another reason, like a foreign finalizer, keeps every reason
func TestKeptIdentityDecided(t *testing.T) {
	sa := key("ServiceAccount", "shop", "reports")
	other := key("ServiceAccount", "shop", "orders")
	idReason := "identity annotation eks.amazonaws.com/role-arn belongs to EKS, and on AKS nothing gives its Pods those cloud credentials"
	tr := Translation{
		Code:       "te2a",
		Exported:   map[Key]any{sa: map[string]any{}, other: map[string]any{}},
		Translated: map[Key]any{sa: map[string]any{}, other: map[string]any{}},
		Explain:    func(any) string { return "decided: keep-source" },
		Kept:       func(any) string { return "keeps its cloud identity, RESTORE.md's step" },
	}
	r := Result{A: Side{Label: "eks"}, B: Side{Label: "aks"}, WontWork: []WontWork{
		{Side: "aks", Object: Object{Key: sa}, Reasons: []string{idReason}},
		{Side: "aks", Object: Object{Key: other}, Reasons: []string{idReason, "finalizer x belongs to EKS"}},
	}}
	r.ApplyTranslation(tr)
	if len(r.Decided) != 2 || len(r.WontWork) != 0 || !r.Same() {
		t.Fatalf("%+v", r)
	}
	for _, d := range r.Decided {
		switch d.Object.Key {
		case sa:
			if len(d.Why) != 1 || !strings.Contains(d.Why[0], "RESTORE.md") {
				t.Errorf("kept identity says %q", d.Why)
			}
		case other:
			if len(d.Why) != 3 {
				t.Errorf("an identity with another reason keeps them all, got %q", d.Why)
			}
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
