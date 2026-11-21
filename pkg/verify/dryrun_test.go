package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeCluster serves live objects by name and answers dry-run applies with
// result, or err.
type fakeCluster struct {
	live    map[string]*unstructured.Unstructured
	results map[string]*unstructured.Unstructured
	errs    map[string]error
}

func (f fakeCluster) Get(_ context.Context, _ schema.GroupVersionResource, _, name string) (*unstructured.Unstructured, error) {
	if o, ok := f.live[name]; ok {
		return o.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
}

func (f fakeCluster) DryRunApply(_ context.Context, _ schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := f.errs[obj.GetName()]; err != nil {
		return nil, err
	}
	if r, ok := f.results[obj.GetName()]; ok {
		return r, nil
	}
	return f.live[obj.GetName()].DeepCopy(), nil
}

func cm(name, value, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "shop", "resourceVersion": rv,
			"managedFields": []any{map[string]any{"manager": "kubectl"}}},
		"data": map[string]any{"k": value},
	}}
	return u
}

func mapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	return m
}

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func manifest(kind, name string) string {
	return "apiVersion: v1\nkind: " + kind + "\nmetadata: {name: " + name + ", namespace: shop}\ndata: {k: v}\n"
}

func TestDryRunVerifier(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"clusters/c/kustomization.yaml":                               kustomization("namespaces/shop"),
		"clusters/c/namespaces/shop/kustomization.yaml":               kustomization("configmap/clean.yaml", "configmap/drifted.yaml", "configmap/gone.yaml", "configmap/shell.yaml", "pod/debug.yaml"),
		"clusters/c/namespaces/shop/configmap/clean.yaml":             manifest("ConfigMap", "clean"),
		"clusters/c/namespaces/shop/configmap/drifted.yaml":           manifest("ConfigMap", "drifted"),
		"clusters/c/namespaces/shop/configmap/gone.yaml":              manifest("ConfigMap", "gone"),
		"clusters/c/namespaces/shop/configmap/shell.yaml":             "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: shell, namespace: shop}\n",
		"clusters/c/namespaces/shop/configmap/unlisted.yaml":          manifest("ConfigMap", "unlisted"),
		"clusters/c/namespaces/shop/pod/debug.yaml":                   "apiVersion: v1\nkind: Pod\nmetadata: {name: debug, namespace: shop}\nspec: {containers: [{name: a, image: b}]}\n",
		"clusters/c/redacted/namespaces/shop/secret/db.yaml.redacted": "apiVersion: v1\nkind: Secret\n",
		// Not applied, so not checked.
		"clusters/c/external-secrets/kustomization.yaml":                                         kustomization("namespaces/shop/externalsecret.external-secrets.io/db.yaml"),
		"clusters/c/external-secrets/namespaces/shop/externalsecret.external-secrets.io/db.yaml": "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata: {name: db, namespace: shop}\n",
		// Another cluster's redacted file is not counted.
		"clusters/d/redacted/namespaces/shop/secret/x.yaml.redacted": "apiVersion: v1\nkind: Secret\n",
	})
	drifted := cm("drifted", "v", "1")
	debug := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "debug", "namespace": "shop"}, "spec": map[string]any{"containers": []any{}}}}
	cluster := fakeCluster{
		live: map[string]*unstructured.Unstructured{
			"clean":   cm("clean", "v", "1"),
			"drifted": drifted,
			"shell":   cm("shell", "v", "1"),
			"debug":   debug,
		},
		results: map[string]*unstructured.Unstructured{
			// A manager and resource version change in every apply; data
			// changed here.
			"clean":   cm("clean", "v", "2"),
			"drifted": cm("drifted", "changed", "2"),
		},
		errs: map[string]error{"debug": podUpdateRejected()},
	}

	report, err := DryRunVerifier{Cluster: cluster, Mapper: mapper()}.Verify(context.Background(), filepath.Join(repo, "clusters", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Total != 5 || report.Clean != 1 {
		t.Errorf("total %d clean %d, want 5 and 1 (unlisted and external-secrets files are not checked)", report.Total, report.Clean)
	}
	got := map[string]Diff{}
	for _, d := range report.Diffs {
		got[d.Ref] = d
	}
	if d := got["ConfigMap shop/drifted"]; len(d.Paths) != 1 || d.Paths[0] != "data.k" || d.Explanation != "" || d.Kind != KindChanged ||
		len(d.Fields) != 1 || d.Fields[0].Cluster != "v" || d.Fields[0].Export != "changed" {
		t.Errorf("drifted: %+v", d)
	}
	if d := got["ConfigMap shop/gone"]; len(d.Paths) != 1 || !strings.Contains(d.Paths[0], "not in the cluster") {
		t.Errorf("gone: %+v", d)
	}
	if d := got["Pod shop/debug"]; !strings.Contains(d.Explanation, "Pods can't be updated") {
		t.Errorf("debug: %+v", d)
	}
	// The apply would change nothing, but the export lost the data.
	if d := got["ConfigMap shop/shell"]; len(d.Paths) != 1 || !strings.HasPrefix(d.Paths[0], ".data: in the cluster but missing") {
		t.Errorf("shell: %+v", d)
	}
	if len(report.Skipped) != 1 || report.Skipped["redacted (Secret values were replaced; not applied on purpose)"] != 1 {
		t.Errorf("skipped %v, want this cluster's one redacted Secret", report.Skipped)
	}
	if report.Passed() {
		t.Error("passed with unexplained diffs")
	}
}

func TestDryRunVerifierAbortsOnCredentials(t *testing.T) {
	files := map[string]string{}
	live := map[string]*unstructured.Unstructured{}
	errs := map[string]error{}
	var names []string
	for i := range 50 {
		name := fmt.Sprintf("cm%d", i)
		names = append(names, name+".yaml")
		files[name+".yaml"] = manifest("ConfigMap", name)
		live[name] = cm(name, "v", "1")
		errs[name] = apierrors.NewUnauthorized("expired")
	}
	files["kustomization.yaml"] = kustomization(names...)
	repo := writeRepo(t, files)
	_, err := (DryRunVerifier{Cluster: fakeCluster{live: live, errs: errs}, Mapper: mapper()}).Verify(context.Background(), repo)
	if err == nil {
		t.Fatal("no error on an expired credential")
	}
	// The first one stops the run: one error, not fifty.
	if n := strings.Count(err.Error(), "expired"); n != 1 {
		t.Errorf("%d errors reported:\n%v", n, err)
	}
}

func TestClusterRoot(t *testing.T) {
	one := writeRepo(t, map[string]string{"clusters/prod/kustomization.yaml": kustomization()})
	if got, err := ClusterRoot(one); err != nil || got != filepath.Join(one, "clusters", "prod") {
		t.Errorf("one cluster: %s %v", got, err)
	}
	if got, err := ClusterRoot(filepath.Join(one, "clusters", "prod")); err != nil || got != filepath.Join(one, "clusters", "prod") {
		t.Errorf("cluster dir: %s %v", got, err)
	}
	two := writeRepo(t, map[string]string{"clusters/a/kustomization.yaml": kustomization(), "clusters/b/kustomization.yaml": kustomization()})
	if _, err := ClusterRoot(two); err == nil || !strings.Contains(err.Error(), "2 clusters") {
		t.Errorf("two clusters: %v", err)
	}
	if _, err := ClusterRoot(t.TempDir()); err == nil {
		t.Error("empty directory accepted")
	}
}

func TestRefHasGroup(t *testing.T) {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("example.com/v1")
	o.SetKind("Widget")
	o.SetNamespace("shop")
	o.SetName("blue")
	if got := Ref(o); got != "example.com/Widget shop/blue" {
		t.Errorf("got %s", got)
	}
}

func kustomization(resources ...string) string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [" + strings.Join(resources, ", ") + "]\n"
}

// podUpdateRejected is the API server's answer to changing a Pod's spec.
func podUpdateRejected() error {
	err := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "debug", nil)
	err.ErrStatus.Message = `Pod "debug" is invalid: spec: Forbidden: pod updates may not change fields other than ...`
	return err
}

// Every golden export builds, and what verify checks never includes the
// ExternalSecrets, which are not applied.
func TestBuildCorpus(t *testing.T) {
	roots, err := filepath.Glob("../../testdata/corpus/*/expected/repo")
	if err != nil || len(roots) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	for _, repo := range roots {
		root, err := ClusterRoot(repo)
		if err != nil {
			t.Fatal(err)
		}
		objs, err := build(root)
		if err != nil {
			t.Errorf("%s: %v", repo, err)
			continue
		}
		for _, o := range objs {
			if o.GetKind() == "ExternalSecret" {
				t.Errorf("%s: %s would be verified", repo, Ref(o))
			}
		}
	}
}

// A CRD in the export that the cluster lacks is reported, and its custom
// resources say why their type is not served.
func TestMissingCRDs(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"kustomization.yaml":      kustomization("crds", "namespaces/shop"),
		"crds/kustomization.yaml": kustomization("widgets.example.com.yaml"),
		"crds/widgets.example.com.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: widgets.example.com}\n" +
			"spec: {group: example.com, names: {kind: Widget, plural: widgets}, scope: Namespaced}\n",
		"namespaces/shop/kustomization.yaml": kustomization("widget.yaml"),
		"namespaces/shop/widget.yaml":        "apiVersion: example.com/v1\nkind: Widget\nmetadata: {name: w, namespace: shop}\n",
	})
	m := mapper().(*meta.DefaultRESTMapper)
	m.Add(crdGVK, meta.RESTScopeRoot)
	report, err := DryRunVerifier{Cluster: fakeCluster{}, Mapper: m}.Verify(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingCRDs) != 1 || report.MissingCRDs[0] != "widgets.example.com" {
		t.Errorf("missing CRDs %v", report.MissingCRDs)
	}
	var widget *Diff
	for i, d := range report.Diffs {
		if d.Ref == "example.com/Widget shop/w" {
			widget = &report.Diffs[i]
		}
	}
	if widget == nil || !strings.Contains(widget.Paths[0], "its CustomResourceDefinition widgets.example.com is in crds/ but not in the cluster yet") {
		t.Errorf("widget diff %+v (all: %+v)", widget, report.Diffs)
	}
}

// Status a controller changes between the read and the dry-run (a Pod
// starting) is not a difference: an apply never changes it.
func TestStatusIgnored(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"kustomization.yaml": kustomization("cm.yaml"),
		"cm.yaml":            manifest("ConfigMap", "c"),
	})
	live, after := cm("c", "v", "1"), cm("c", "v", "2")
	live.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	after.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	report, err := DryRunVerifier{Cluster: fakeCluster{live: map[string]*unstructured.Unstructured{"c": live},
		results: map[string]*unstructured.Unstructured{"c": after}}, Mapper: mapper()}.Verify(context.Background(), repo)
	if err != nil || report.Clean != 1 {
		t.Errorf("%v %+v", err, report)
	}
}

// A broken connection is retried; an answer from the API server is not.
func TestRetry(t *testing.T) {
	calls := 0
	reset := func() (*unstructured.Unstructured, error) {
		calls++
		if calls < 3 {
			return nil, fmt.Errorf("read tcp: %w", syscall.ECONNRESET)
		}
		return &unstructured.Unstructured{}, nil
	}
	if _, err := retry(context.Background(), reset); err != nil || calls != 3 {
		t.Errorf("connection reset: %v after %d calls", err, calls)
	}
	calls = 0
	notFound := func() (*unstructured.Unstructured, error) {
		calls++
		return nil, apierrors.NewNotFound(schema.GroupResource{}, "x")
	}
	if _, err := retry(context.Background(), notFound); !apierrors.IsNotFound(err) || calls != 1 {
		t.Errorf("not found: %v after %d calls", err, calls)
	}
}

// A resource the credentials may not patch for the dry-run is not checked:
// out of the count, and the run fails for it; with -n, only that namespace
// is checked.
func TestNotCheckedAndNamespaces(t *testing.T) {
	other := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: elsewhere, namespace: prod}\ndata: {k: v}\n"
	repo := writeRepo(t, map[string]string{
		"kustomization.yaml": kustomization("a.yaml", "b.yaml", "elsewhere.yaml"),
		"a.yaml":             manifest("ConfigMap", "a"),
		"b.yaml":             manifest("ConfigMap", "b"),
		"elsewhere.yaml":     other,
	})
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "b", errors.New("cannot patch"))
	cluster := fakeCluster{
		live:    map[string]*unstructured.Unstructured{"a": cm("a", "k", "v"), "b": cm("b", "k", "v"), "elsewhere": cm("elsewhere", "k", "v")},
		results: map[string]*unstructured.Unstructured{"a": cm("a", "k", "v"), "elsewhere": cm("elsewhere", "k", "v")},
		errs:    map[string]error{"b": forbidden},
	}
	report, err := DryRunVerifier{Cluster: cluster, Mapper: mapper()}.Verify(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if report.Total != 2 || report.Clean != 2 || len(report.NotChecked) != 1 || report.NotChecked[0].Kind != KindNotChecked ||
		!strings.Contains(report.NotChecked[0].Paths[0], "may not patch") {
		t.Errorf("report = %+v", report)
	}
	if report.Passed() {
		t.Error("a run with a resource not checked passed")
	}
	report, err = DryRunVerifier{Cluster: cluster, Mapper: mapper(), Namespaces: []string{"prod"}}.Verify(context.Background(), repo)
	if err != nil || report.Total != 1 || report.Clean != 1 || len(report.NotChecked) != 0 || !report.Passed() {
		t.Errorf("-n prod: %v %+v", err, report)
	}
}

// An export that lists remote resources is refused before kustomize would
// clone them.
func TestLocalOnly(t *testing.T) {
	for ref, remote := range map[string]bool{
		"configmap/a.yaml": false, "../base": false,
		"https://github.com/acme/x//deploy?ref=v1": true, "github.com/acme/x/deploy": true,
		"git@github.com:acme/x.git": true, "ssh://git@host/x": true,
	} {
		root := t.TempDir()
		k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- " + ref + "\n"
		if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(k), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := localOnly(root); (err != nil) != remote {
			t.Errorf("%s: err = %v, want refused %v", ref, err, remote)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
