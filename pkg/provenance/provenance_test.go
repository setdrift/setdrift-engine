// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func object(apiVersion, kind, ns, name string, labels, ann map[string]string) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(labels)
	u.SetAnnotations(ann)
	return resource.New(u)
}

func TestClassifyPrecedence(t *testing.T) {
	const (
		argo  = snapshotv1alpha1.Source_SOURCE_ARGO_CD
		flux  = snapshotv1alpha1.Source_SOURCE_FLUX
		helm  = snapshotv1alpha1.Source_SOURCE_HELM
		unman = snapshotv1alpha1.Source_SOURCE_UNMANAGED
	)
	helmAnn := map[string]string{helmReleaseName: "web", helmReleaseNS: "shop"}
	for _, tc := range []struct {
		name        string
		classifier  LabelClassifier
		labels, ann map[string]string
		source      snapshotv1alpha1.Source
		manager     string
		low         bool
	}{
		{name: "no markers", source: unman},
		{name: "flux kustomization over helm",
			labels: map[string]string{fluxKustomizeName: "apps", fluxKustomizeNS: "flux-system", managedByLabel: "Helm"}, ann: helmAnn,
			source: flux, manager: "flux-system/apps"},
		{name: "flux helmrelease over helm annotations",
			labels: map[string]string{fluxHelmName: "redis", fluxHelmNS: "flux-system"}, ann: helmAnn,
			source: flux, manager: "flux-system/redis"},
		{name: "argo tracking id over helm annotations",
			ann:    map[string]string{argoTrackingID: "payments:apps/Deployment:shop/web", helmReleaseName: "web"},
			source: argo, manager: "payments"},
		{name: "argo apps in any namespace",
			ann:    map[string]string{argoTrackingID: "team-a_web:apps/Deployment:shop/web"},
			source: argo, manager: "team-a/web"},
		{name: "copied tracking id falls through",
			ann:    map[string]string{argoTrackingID: "payments:apps/Deployment:shop/other"},
			source: unman},
		{name: "helm annotations", ann: helmAnn, source: helm, manager: "shop/web"},
		{name: "helm annotation without namespace uses the object's",
			ann: map[string]string{helmReleaseName: "web"}, source: helm, manager: "shop/web"},
		{name: "argo label tracking of a helm-templated chart is argo, not helm",
			labels: map[string]string{DefaultArgoInstanceLabel: "billing", managedByLabel: "Helm"},
			source: argo, manager: "billing", low: true},
		{name: "custom argo instance label",
			classifier: LabelClassifier{ArgoInstanceLabel: "argocd.argoproj.io/instance"},
			labels:     map[string]string{"argocd.argoproj.io/instance": "billing", DefaultArgoInstanceLabel: "chart-default"},
			source:     argo, manager: "billing", low: true},
		{name: "managed-by helm label alone is weak",
			labels: map[string]string{managedByLabel: "Helm"}, source: helm, low: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := object("apps/v1", "Deployment", "shop", "web", tc.labels, tc.ann)
			p := tc.classifier.Classify(o)
			if p.Source != tc.source || p.Manager != tc.manager {
				t.Errorf("got %s %q, want %s %q", p.Source, p.Manager, tc.source, tc.manager)
			}
			if low := p.Evidence[confidenceKey] == lowConfidence; low != tc.low {
				t.Errorf("low confidence = %v, want %v", low, tc.low)
			}
		})
	}
}

func TestTrackingIDMatches(t *testing.T) {
	for _, tc := range []struct {
		id            string
		kind, ns, api string
		want          bool
	}{
		{"app:/ConfigMap:shop/web", "ConfigMap", "shop", "v1", true},
		{"app:/ConfigMap:other/web", "ConfigMap", "shop", "v1", false},
		{"app:apps/ConfigMap:shop/web", "ConfigMap", "shop", "v1", false},
		{"app:/Secret:shop/web", "ConfigMap", "shop", "v1", false},
		{"app:rbac.authorization.k8s.io/ClusterRole:/web", "ClusterRole", "", "rbac.authorization.k8s.io/v1", true},
		{"garbage", "ConfigMap", "shop", "v1", false},
		{"app:/ConfigMap", "ConfigMap", "shop", "v1", false},
	} {
		tid, ok := parseTrackingID(tc.id)
		got := ok && tid.matches(object(tc.api, tc.kind, tc.ns, "web", nil, nil))
		if got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestManagerChecks covers the cases where a missing manager must not be
// reported because it could not be seen.
func TestManagerChecks(t *testing.T) {
	tracked := func(app string) *resource.Object {
		return object("v1", "ConfigMap", "shop", "web", nil, map[string]string{argoTrackingID: app + ":/ConfigMap:shop/web"})
	}
	codes := func(objs ...*resource.Object) []string {
		out, _, err := LabelClassifier{}.Process(context.Background(), objs)
		if err != nil {
			t.Fatal(err)
		}
		var c []string
		for _, f := range out[0].Findings {
			c = append(c, f.Code)
		}
		return c
	}
	// No Applications at all: Argo CD may run in another cluster.
	if c := codes(tracked("gone")); len(c) != 0 {
		t.Errorf("Argo CD not in this cluster: got %v, want no findings", c)
	}
	app := object("argoproj.io/v1alpha1", "Application", "argocd", "other", nil, nil)
	if c := codes(tracked("gone"), app); len(c) != 1 || c[0] != "MANAGER_NOT_FOUND" {
		t.Errorf("Argo CD here, app missing: got %v, want MANAGER_NOT_FOUND", c)
	}
	// Helm release with no readable Secrets in its namespace: unknown.
	helmObj := object("v1", "ConfigMap", "shop", "web", nil, map[string]string{helmReleaseName: "web"})
	if c := codes(helmObj); len(c) != 0 {
		t.Errorf("no Secrets visible: got %v, want no findings", c)
	}
}

func TestOperatorOwner(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		apiVersion string
		want       snapshotv1alpha1.Source
	}{
		{"cert-manager.io/v1", snapshotv1alpha1.Source_SOURCE_OPERATOR},
		{"batch/v1", snapshotv1alpha1.Source_SOURCE_UNMANAGED},
		{"v1", snapshotv1alpha1.Source_SOURCE_UNMANAGED},
		{"apiextensions.k8s.io/v1", snapshotv1alpha1.Source_SOURCE_UNMANAGED},
	} {
		o := object("v1", "Secret", "shop", "tls", nil, nil)
		o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: tc.apiVersion, Kind: "Owner", Name: "x", Controller: &yes}})
		if got := (LabelClassifier{}).Classify(o); got.Source != tc.want {
			t.Errorf("owner %s: got %s, want %s", tc.apiVersion, got.Source, tc.want)
		}
	}
}

// A CI/CD pipeline's annotations, or its field manager when asked for, make
// an object CI/CD's; GitOps markers stay stronger, with the annotations kept
// as evidence.
func TestCIPipeline(t *testing.T) {
	src := map[string]string{
		SourceRepoAnnotation:     "https://github.com/acme/shop",
		SourcePathAnnotation:     "/deploy/prod",
		SourceRevisionAnnotation: "3f2c1a9",
	}
	c := LabelClassifier{CIFieldManagers: []string{"github-actions"}}

	p := c.Classify(object("v1", "ConfigMap", "shop", "a", nil, src))
	if p.Source != snapshotv1alpha1.Source_SOURCE_CI_CD || p.Manager != "https://github.com/acme/shop//deploy/prod" ||
		p.Evidence[SourceRevisionAnnotation] != "3f2c1a9" {
		t.Errorf("annotated: %+v", p)
	}

	// helm template | kubectl apply leaves Helm's label only: the pipeline wins
	p = c.Classify(object("v1", "ConfigMap", "shop", "b", map[string]string{"app.kubernetes.io/managed-by": "Helm"}, src))
	if p.Source != snapshotv1alpha1.Source_SOURCE_CI_CD {
		t.Errorf("Helm label and annotations: %+v", p)
	}

	// Argo CD's tracking id is stronger; the annotations stay as evidence
	argo := map[string]string{"argocd.argoproj.io/tracking-id": "shop:/ConfigMap:shop/c"}
	for k, v := range src {
		argo[k] = v
	}
	p = c.Classify(object("v1", "ConfigMap", "shop", "c", nil, argo))
	if p.Source != snapshotv1alpha1.Source_SOURCE_ARGO_CD || p.Evidence[SourceRepoAnnotation] != "https://github.com/acme/shop" {
		t.Errorf("Argo CD and annotations: %+v", p)
	}

	// the field manager counts only when asked for
	withManager := func() *resource.Object {
		o := object("v1", "ConfigMap", "shop", "d", nil, nil)
		o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "github-actions", Operation: metav1.ManagedFieldsOperationApply}})
		return o
	}
	p = c.Classify(withManager())
	if p.Source != snapshotv1alpha1.Source_SOURCE_CI_CD || p.Evidence["fieldManager"] != "github-actions" || p.Evidence["confidence"] != "low" {
		t.Errorf("field manager: %+v", p)
	}
	if p = (LabelClassifier{}).Classify(withManager()); p.Source != snapshotv1alpha1.Source_SOURCE_UNMANAGED {
		t.Errorf("field manager not asked for: %+v", p)
	}
}

func TestCanonicalRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/foo/bar":                        "https://github.com/foo/bar",
		"http://github.com/foo/bar/":                        "https://github.com/foo/bar",
		"github.com/foo/bar":                                "https://github.com/foo/bar",
		"github/foo/bar":                                    "https://github.com/foo/bar",
		"GitHub.com/foo/bar":                                "https://github.com/foo/bar",
		"https://github.com/foo/bar.git":                    "https://github.com/foo/bar",
		"git@github.com:foo/bar.git":                        "https://github.com/foo/bar",
		"ssh://git@github.com/foo/bar":                      "https://github.com/foo/bar",
		"https://x-access-token:ghs_abc@github.com/foo/bar": "https://github.com/foo/bar",
		"gitlab/group/sub/project":                          "https://gitlab.com/group/sub/project",
		"https://git.example.com:8443/team/app":             "https://git.example.com:8443/team/app",
		"foo/bar":                                           "foo/bar",
		"internal-repo":                                     "internal-repo",
	} {
		if got := CanonicalRepo(in); got != want {
			t.Errorf("CanonicalRepo(%q) = %q, want %q", in, got, want)
		}
	}
	// the manager and the evidence carry the canonical form, without the token
	p := (LabelClassifier{}).Classify(object("v1", "ConfigMap", "shop", "a", nil,
		map[string]string{SourceRepoAnnotation: "https://ci:secret@github.com/foo/bar.git"}))
	if p.Manager != "https://github.com/foo/bar" || p.Evidence[SourceRepoAnnotation] != "https://github.com/foo/bar" {
		t.Errorf("classified: %+v", p)
	}
}
