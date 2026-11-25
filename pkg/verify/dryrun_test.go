// tests the dry-run verifier against a fake cluster, what gets built and checked, how each kind of difference is reported, retries, credentials and the remote reference guard

package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeCluster is a Cluster for tests, it serves live objects by name and answers dry-run applies with the result or error set for that name, or the live object unchanged
type fakeCluster struct {
	live    map[string]*unstructured.Unstructured
	results map[string]*unstructured.Unstructured
	errs    map[string]error
}

// Get gives back a copy of the live object by name, or not found
func (f fakeCluster) Get(_ context.Context, _ schema.GroupVersionResource, _, name string) (*unstructured.Unstructured, error) {
	if o, ok := f.live[name]; ok {
		return o.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
}

// DryRunApply gives back the error or result set for the object's name, otherwise a copy of the live object, meaning nothing would change
func (f fakeCluster) DryRunApply(_ context.Context, _ schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := f.errs[obj.GetName()]; err != nil {
		return nil, err
	}
	if r, ok := f.results[obj.GetName()]; ok {
		return r, nil
	}
	return f.live[obj.GetName()].DeepCopy(), nil
}

// cm builds a live ConfigMap in shop with one data value and a resource version, owned by kubectl
func cm(name, value, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "shop", "resourceVersion": rv,
			"managedFields": []any{map[string]any{"manager": "kubectl"}}},
		"data": map[string]any{"k": value},
	}}
	return u
}

// mapper builds a RESTMapper serving ConfigMaps and Pods
func mapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	return m
}

// writeRepo writes the files into a temporary directory and gives back its path
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

// manifest gives back the yaml of a core object of that kind and name in shop, with one data value
func manifest(kind, name string) string {
	return "apiVersion: v1\nkind: " + kind + "\nmetadata: {name: " + name + ", namespace: shop}\ndata: {k: v}\n"
}

// check that a clean, a drifted, a missing, an emptied and a rejected resource are each reported right, and that unlisted, external-secrets and other clusters' files are left out
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
		// not applied, so not checked
		"clusters/c/external-secrets/kustomization.yaml":                                         kustomization("namespaces/shop/externalsecret.external-secrets.io/db.yaml"),
		"clusters/c/external-secrets/namespaces/shop/externalsecret.external-secrets.io/db.yaml": "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata: {name: db, namespace: shop}\n",
		// another cluster's redacted file isnt counted
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
			// a manager and resource version change in every apply, the data changed here
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
	// the apply would change nothing, but the export lost the data
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

// check that an expired credential stops the run with one error, not one per resource
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
	// the first one stops the run, one error and not fifty
	if n := strings.Count(err.Error(), "expired"); n != 1 {
		t.Errorf("%d errors reported:\n%v", n, err)
	}
}

// check that ClusterRoot finds the one cluster in an export or takes a cluster's directory as is, and refuses two clusters or none
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

// check that Ref puts the group before the kind of a non-core object
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

// kustomization gives back the yaml of a kustomization listing these resources
func kustomization(resources ...string) string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [" + strings.Join(resources, ", ") + "]\n"
}

// podUpdateRejected builds the API server's answer to changing a Pod's spec
func podUpdateRejected() error {
	err := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "debug", nil)
	err.ErrStatus.Message = `Pod "debug" is invalid: spec: Forbidden: pod updates may not change fields other than ...`
	return err
}

// check that every golden export builds, and what verify checks never includes the ExternalSecrets, which arent applied
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
		objs, err := buildSealed(root, root)
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

// check that a CRD in the export that the cluster lacks is reported, and its custom resources say why their type isnt served
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

// check that status a controller changes between the read and the dry-run, like a Pod starting, isnt a difference, an apply never changes it
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

// check that a broken connection is retried and an answer from the API server isnt
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

// check that a resource the credentials may not patch is not checked, out of the count and failing the run, and that with -n only that namespace is checked
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

// check that an export listing remote resources is refused before kustomize would clone them
func TestLocalOnly(t *testing.T) {
	// each kustomization body, and whether verify must refuse it
	for body, remote := range map[string]bool{
		"resources:\n- configmap/a.yaml\n- ../base\n- deployment.apps/web.yaml\n":  false,
		"resources:\n- https://github.com/acme/x//deploy?ref=v1\n":                 true,
		"resources:\n- github.com/acme/x/deploy\n":                                 true,
		"resources:\n- git@github.com:acme/x.git\n":                                true,
		"resources:\n- ssh://git@host/x\n":                                         true,
		"resources:\n- git::https://example.com/x.git\n":                           true,
		"resources:\n- deploy@example.com:org/repo\n":                              true,
		"resources:\n- HTTPS://example.com/x.yaml\n":                               true,
		"components:\n- https://example.com/c\n":                                   true,
		"patches:\n- path: https://example.com/p.yaml\n":                           true,
		"configMapGenerator:\n- name: c\n  files:\n  - https://example.com/f\n":    true,
		"transformers:\n- github.com/acme/t\n":                                     true,
		"openapi:\n  path: http://example.com/schema.json\n":                       true,
		"crds:\n- file:///etc/crd.yaml\n":                                          true,
		"commonAnnotations:\n  source: https://github.com/acme/x\n":                false,
		"patches:\n- patch: |-\n    - op: add\n      value: https://example.com\n": false,
	} {
		root := t.TempDir()
		k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" + body
		if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(k), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := localOnly(root); (err != nil) != remote {
			t.Errorf("%q: err = %v, want refused %v", body, err, remote)
		}
	}
}

// check that a symlink anywhere in the export is refused, a file or a directory, since it could lead kustomize out of the export
func TestLocalOnlyRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	outside := t.TempDir()
	for _, link := range []string{"linked-dir", "linked.yaml"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte("resources: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
		if err := localOnly(root); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: err = %v, want a symlink refusal", link, err)
		}
	}
}

// check that kustomize builds from a sealed copy, so a kustomization reaching outside the export finds nothing there
func TestBuildIsSealed(t *testing.T) {
	parent := t.TempDir()
	root, outside := filepath.Join(parent, "export"), filepath.Join(parent, "outside")
	for dir, k := range map[string]string{root: "resources:\n- ../outside\n", outside: "resources:\n- cm.yaml\n"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(k), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stolen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := buildSealed(root, root)
	if err == nil {
		t.Fatalf("built %d objects from outside the export", len(objs))
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the error does not name the real directory: %v", err)
	}
}

// accessCluster is a fakeCluster that also answers whether patch is allowed, per namespace
type accessCluster struct {
	fakeCluster
	allowed map[string]bool
	asked   *int
}

// CanPatch answers from allowed, for AccessChecker
func (a accessCluster) CanPatch(_ context.Context, _ schema.GroupVersionResource, namespace string) (bool, error) {
	*a.asked++
	return a.allowed[namespace], nil
}

// check that verify stops at once with errNoPatch when the credentials may patch nothing in the export, goes on when they may patch something, and asks about each type and namespace once
func TestPatchPreflight(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"kustomization.yaml": kustomization("a.yaml", "b.yaml"),
		"a.yaml":             manifest("ConfigMap", "a"),
		"b.yaml":             manifest("ConfigMap", "b"),
	})
	live := map[string]*unstructured.Unstructured{"a": cm("a", "k", "v"), "b": cm("b", "k", "v")}
	asked := 0
	none := accessCluster{fakeCluster: fakeCluster{live: live, results: live}, asked: &asked}
	if _, err := (DryRunVerifier{Cluster: none, Mapper: mapper()}).Verify(context.Background(), repo); !errors.Is(err, errNoPatch) {
		t.Errorf("no patch anywhere: err %v, want errNoPatch", err)
	}
	if asked != 1 {
		t.Errorf("asked %d times about one type in one namespace, want 1", asked)
	}
	some := accessCluster{fakeCluster: fakeCluster{live: live, results: live}, allowed: map[string]bool{"shop": true}, asked: &asked}
	if report, err := (DryRunVerifier{Cluster: some, Mapper: mapper()}).Verify(context.Background(), repo); err != nil || report.Clean != 2 {
		t.Errorf("patch allowed: %v %+v", err, report)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
