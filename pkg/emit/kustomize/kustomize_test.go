// tests for the kustomize emitter, what goes where in an export, what gets applied and what never does

package kustomize

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// resource builds a test resource with a minimal manifest, with a redacted secret value when redacted is set
func resource(kind, ns, name string, redacted bool) *snapshotv1alpha1.Resource {
	r := &snapshotv1alpha1.Resource{
		ApiVersion:   "v1",
		Kind:         kind,
		Namespace:    ns,
		Name:         name,
		ManifestJson: `{"apiVersion":"v1","kind":"` + kind + `","metadata":{"name":"` + name + `","namespace":"` + ns + `"}}`,
	}
	if redacted {
		r.Redactions = []*snapshotv1alpha1.Redaction{{
			Path:   "data.password",
			Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
		}}
	}
	return r
}

// check that a resource with redacted values is written for reference, but no kustomization.yaml can reach it
func TestRedactedResourcesNotApplied(t *testing.T) {
	dir := t.TempDir()
	s := &snapshotv1alpha1.Snapshot{
		Cluster: &snapshotv1alpha1.Cluster{Name: "c"},
		Resources: []*snapshotv1alpha1.Resource{
			resource("ConfigMap", "shop", "settings", false),
			resource("Secret", "shop", "db-creds", true),
			// a Secret with nothing redacted (no data) stays in the applied set
			resource("Secret", "shop", "plain", false),
			// a namespace holding only redacted resources gets no kustomization
			resource("Secret", "vault", "token", true),
		},
	}
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "clusters", "c")

	for _, f := range []string{
		"redacted/README.md",
		"redacted/namespaces/shop/secret/db-creds.yaml.redacted",
		"redacted/namespaces/vault/secret/token.yaml.redacted",
		"namespaces/shop/secret/plain.yaml",
	} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	for _, f := range []string{"redacted/kustomization.yaml", "namespaces/vault/kustomization.yaml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("%s should not exist", f)
		}
	}

	// no kustomization anywhere mentions a redacted resource or directory
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "kustomization.yaml" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, bad := range []string{"redacted", "db-creds", "vault"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s references %q:\n%s", path, bad, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// check that redacted/ and its README are only written when something was redacted
func TestReadmeOnlyWhenNeeded(t *testing.T) {
	dir := t.TempDir()
	s := &snapshotv1alpha1.Snapshot{Resources: []*snapshotv1alpha1.Resource{resource("ConfigMap", "shop", "settings", false)}}
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clusters", "cluster", redactedDir)); err == nil {
		t.Error("redacted/ written with no redacted resources")
	}
}

// check that UnmanagedOnly writes the unmanaged resources, the ones whose manager wasnt found or was only guessed, and leaves out the managed ones
func TestUnmanagedOnly(t *testing.T) {
	src := func(r *snapshotv1alpha1.Resource, s snapshotv1alpha1.Source, ev map[string]string, codes ...string) *snapshotv1alpha1.Resource {
		r.Provenance = &snapshotv1alpha1.Provenance{Source: s, Evidence: ev}
		for _, c := range codes {
			r.Findings = append(r.Findings, &snapshotv1alpha1.Finding{Code: c})
		}
		return r
	}
	s := &snapshotv1alpha1.Snapshot{
		Cluster: &snapshotv1alpha1.Cluster{Name: "c"},
		Resources: []*snapshotv1alpha1.Resource{
			src(resource("ConfigMap", "shop", "manual", false), snapshotv1alpha1.Source_SOURCE_UNMANAGED, nil),
			src(resource("ConfigMap", "shop", "argo", false), snapshotv1alpha1.Source_SOURCE_ARGO_CD, nil),
			src(resource("ConfigMap", "shop", "orphan", false), snapshotv1alpha1.Source_SOURCE_ARGO_CD, nil, "MANAGER_NOT_FOUND"),
			src(resource("ConfigMap", "shop", "templated", false), snapshotv1alpha1.Source_SOURCE_HELM, map[string]string{"confidence": "low"}),
			src(resource("ConfigMap", "shop", "operator", false), snapshotv1alpha1.Source_SOURCE_OPERATOR, nil),
		},
	}
	dir := t.TempDir()
	if err := (Emitter{UnmanagedOnly: true}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "clusters", "c", "namespaces", "shop", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"manual", "orphan", "templated"} {
		if !strings.Contains(string(b), "configmap/"+want+".yaml") {
			t.Errorf("%s missing from:\n%s", want, b)
		}
	}
	for _, unwanted := range []string{"argo", "operator"} {
		if strings.Contains(string(b), "configmap/"+unwanted+".yaml") {
			t.Errorf("%s should be left out:\n%s", unwanted, b)
		}
	}
}

// check that a custom key template shapes the ExternalSecret's remote key, the README store example gets the region from the arn, external-secrets/ isnt applied and an unknown provider is refused
func TestExternalSecretKeyTemplate(t *testing.T) {
	es, err := DefaultExternalSecrets("aws-parameter-store")
	if err != nil {
		t.Fatal(err)
	}
	es.KeyTemplate = "/{{.Cluster}}/apps/{{.Name}}"
	s := &snapshotv1alpha1.Snapshot{
		Cluster:   &snapshotv1alpha1.Cluster{Name: "prod", ProviderId: "arn:aws:eks:eu-west-1:111122223333:cluster/prod"},
		Resources: []*snapshotv1alpha1.Resource{resource("Secret", "shop", "db", true)},
	}
	s.Resources[0].ManifestJson = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"shop"},"data":{"password":"x"}}`
	dir := t.TempDir()
	if err := (Emitter{ExternalSecrets: es}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "clusters", "prod")
	b := mustRead(t, filepath.Join(root, "external-secrets", "namespaces", "shop", "externalsecret.external-secrets.io", "db.yaml"))
	for _, want := range []string{"key: /prod/apps/db", "name: secret-store", "property: password"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ExternalSecret lacks %q:\n%s", want, b)
		}
	}
	readme := mustRead(t, filepath.Join(root, "external-secrets", "README.md"))
	if !strings.Contains(string(readme), "region: eu-west-1") || !strings.Contains(string(readme), "service: ParameterStore") {
		t.Errorf("README store example:\n%s", readme)
	}
	rootKustomization := mustRead(t, filepath.Join(root, "kustomization.yaml"))
	if strings.Contains(string(rootKustomization), "external-secrets") {
		t.Error("external-secrets/ must not be applied by default")
	}
	if _, err := DefaultExternalSecrets("vault"); err == nil {
		t.Error("unknown provider accepted")
	}
}

// check that a key template that gives two Secrets one remote secret fails the export
func TestExternalSecretKeysUnique(t *testing.T) {
	es := mustExternalSecrets(t, "aws-secrets-manager")
	es.KeyTemplate = "{{.Name}}"
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"}}
	for _, ns := range []string{"shop", "billing"} {
		r := resource("Secret", ns, "db", true)
		r.ManifestJson = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"` + ns + `"},"data":{"password":"x"}}`
		s.Resources = append(s.Resources, r)
	}
	err := (Emitter{ExternalSecrets: es}).Emit(s, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `remote secret "db" to both Secret shop/db and Secret billing/db`) {
		t.Errorf("got %v", err)
	}
}

// check that a SecretStore is created in each namespace, and parameter store gets ssm commands in RESTORE.md
func TestExternalSecretStores(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod", Platform: "eks"}}
	for _, ns := range []string{"shop", "billing", "shop"} {
		name := "db-" + fmt.Sprint(len(s.Resources))
		r := resource("Secret", ns, name, true)
		r.ManifestJson = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + name + `","namespace":"` + ns + `"},"data":{"password":"x"}}`
		s.Resources = append(s.Resources, r)
	}
	es := mustExternalSecrets(t, "aws-parameter-store")
	es.StoreKind = "SecretStore"
	dir := t.TempDir()
	if err := (Emitter{ExternalSecrets: es}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "clusters", "prod")
	readme := mustRead(t, filepath.Join(root, "external-secrets", "README.md"))
	if !strings.Contains(string(readme), "in each of these namespaces\n   (a SecretStore only serves its own): billing, shop.") ||
		!strings.Contains(string(readme), "namespace: <namespace>") {
		t.Errorf("README:\n%s", readme)
	}
	restore := mustRead(t, filepath.Join(root, "platform", "aws", "RESTORE.md"))
	if !strings.Contains(string(restore), "aws ssm put-parameter --region \"$REGION\" --type SecureString --name /prod/billing/db-1") ||
		strings.Contains(string(restore), "aws secretsmanager create-secret") {
		t.Errorf("RESTORE.md remote secret commands:\n%s", restore)
	}
}

// check that Force replaces the cluster's directory, so files of resources no longer in the cluster go, and keeps everything else in the output directory
func TestForceReplacesClusterDir(t *testing.T) {
	dir := t.TempDir()
	snap := func(names ...string) *snapshotv1alpha1.Snapshot {
		s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "c"}}
		for _, n := range names {
			s.Resources = append(s.Resources, resource("ConfigMap", "shop", n, false))
		}
		return s
	}
	if err := (Emitter{}).Emit(snap("a"), dir); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "README.md")
	other := filepath.Join(dir, "clusters", "other", "kustomization.yaml")
	for _, f := range []string{mine, other} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Emitter{}).Emit(snap("b"), dir); err == nil {
		t.Fatal("wrote into a non-empty directory without Force")
	}
	if err := (Emitter{Force: true}).Emit(snap("b"), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clusters", "c", "namespaces", "shop", "configmap", "a.yaml")); !os.IsNotExist(err) {
		t.Error("stale a.yaml kept")
	}
	for _, f := range []string{mine, other, filepath.Join(dir, "clusters", "c", "namespaces", "shop", "configmap", "b.yaml")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// check that an export failing partway leaves the previous export as it was and no temporary directory behind, and that one killed earlier has its temporary directory removed
func TestFailedExportKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	good := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "c"},
		Resources: []*snapshotv1alpha1.Resource{resource("ConfigMap", "shop", "a", false)}}
	if err := (Emitter{}).Emit(good, dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "clusters", ".c.tmp-123")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "x.yaml"), []byte("left by a killed export"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := resource("ConfigMap", "shop", "b", false)
	bad.ManifestJson = "{not json"
	broken := &snapshotv1alpha1.Snapshot{Cluster: good.Cluster,
		Resources: []*snapshotv1alpha1.Resource{resource("ConfigMap", "shop", "a2", false), bad}}
	if err := (Emitter{Force: true}).Emit(broken, dir); err == nil {
		t.Fatal("no error for an unwritable manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, "clusters", "c", "namespaces", "shop", "configmap", "a.yaml")); err != nil {
		t.Errorf("previous export lost: %v", err)
	}
	entries := mustReadDir(t, filepath.Join(dir, "clusters"))
	if len(entries) != 1 || entries[0].Name() != "c" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("clusters/ holds %v, want only c", names)
	}
	if fi, err := os.Stat(filepath.Join(dir, "clusters", "c")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("cluster directory: %v %v", fi.Mode(), err)
	}
}

// check that ClusterDir always gives one directory right under clusters/, whatever the cluster is named
func TestClusterDirStaysInside(t *testing.T) {
	for _, name := range []string{"..", ".", "", "../x", "a/b"} {
		got := ClusterDir("/out", name)
		if rel, err := filepath.Rel("/out/clusters", got); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/") {
			t.Errorf("%q: %s is not one directory under /out/clusters", name, got)
		}
	}
}

// check that nothing to apply still gives a root kustomization.yaml kustomize accepts, and the README doesnt point at external-secrets/ without ExternalSecrets
func TestEmptyRootKustomization(t *testing.T) {
	dir := t.TempDir()
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "c"},
		Resources: []*snapshotv1alpha1.Resource{resource("Secret", "shop", "db", true)}}
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "clusters", "c", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "resources: []") {
		t.Errorf("root kustomization:\n%s", b)
	}
	readme := mustRead(t, filepath.Join(dir, "clusters", "c", "redacted", "README.md"))
	if strings.Contains(string(readme), "../external-secrets/") {
		t.Error("README points at external-secrets/ without ExternalSecrets")
	}
}

// check that the export records its source cluster, with the kube-system uid, in a file no manifest tool reads, and a missing file reads as nil without an error
func TestExportInfo(t *testing.T) {
	ks := resource("Namespace", "", "kube-system", false)
	ks.SourceUid = "uid-1"
	s := &snapshotv1alpha1.Snapshot{
		Cluster:   &snapshotv1alpha1.Cluster{Name: "prod", ProviderId: "arn:aws:eks:us-east-2:111122223333:cluster/prod"},
		Producer:  &snapshotv1alpha1.Producer{Version: "v0.1.0"},
		Resources: []*snapshotv1alpha1.Resource{ks},
	}
	dir := t.TempDir()
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	info, err := ReadExportInfo(ClusterDir(dir, "prod"))
	if err != nil || info == nil || info.Cluster != "prod" || info.KubeSystemUID != "uid-1" || info.SetDriftVersion != "v0.1.0" ||
		info.ProviderID != s.Cluster.ProviderId {
		t.Errorf("%+v %v", info, err)
	}
	if info, err := ReadExportInfo(t.TempDir()); info != nil || err != nil {
		t.Errorf("no file: %v %v", info, err)
	}
}

// check that a custom resource of an api the platform provides goes to the platform set, which builds with kustomize, and the generic set leaves it out
func TestPlatformSet(t *testing.T) {
	crd := &snapshotv1alpha1.Resource{ApiVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "nodeclasses.eks.amazonaws.com",
		ManifestJson: `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"nodeclasses.eks.amazonaws.com"},` +
			`"spec":{"group":"eks.amazonaws.com","names":{"kind":"NodeClass","plural":"nodeclasses"},"scope":"Cluster"}}`,
		Provenance: &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_PLATFORM, Manager: "eks"}}
	nodeClass := &snapshotv1alpha1.Resource{ApiVersion: "eks.amazonaws.com/v1", Kind: "NodeClass", Name: "custom",
		ManifestJson: `{"apiVersion":"eks.amazonaws.com/v1","kind":"NodeClass","metadata":{"name":"custom"},"spec":{"role":"r"}}`}
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod", Platform: "eks"},
		Resources: []*snapshotv1alpha1.Resource{crd, nodeClass, resource("ConfigMap", "shop", "settings", false)}}
	dir := t.TempDir()
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, "prod")
	if _, err := os.Stat(filepath.Join(root, "platform", "aws", "eks", "cluster-scoped", "nodeclass.eks.amazonaws.com", "custom.yaml")); err != nil {
		t.Errorf("NodeClass not in the platform set: %v", err)
	}
	build := func(path string) string {
		t.Helper()
		k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
		m, err := k.Run(filesys.MakeFsOnDisk(), path)
		if err != nil {
			t.Fatalf("kustomize build %s: %v", path, err)
		}
		y, _ := m.AsYaml()
		return string(y)
	}
	generic, plat := build(root), build(filepath.Join(root, "platform", "aws", "eks"))
	if strings.Contains(generic, "kind: NodeClass") || !strings.Contains(generic, "name: settings") {
		t.Errorf("generic set:\n%s", generic)
	}
	if !strings.Contains(plat, "kind: NodeClass") || !strings.Contains(plat, "name: settings") {
		t.Errorf("platform set:\n%s", plat)
	}
	md := mustRead(t, filepath.Join(root, "RESTORE.md"))
	if !strings.Contains(string(md), "kubectl --context \"$DST\" apply --server-side -k platform/aws/eks") {
		t.Errorf("RESTORE.md does not say to apply the platform set:\n%s", md)
	}
}

// check that on aws the store is written next to the platform's files, as yaml that applies
func TestAWSSecretStoreFile(t *testing.T) {
	es := mustExternalSecrets(t, "auto")
	r := resource("Secret", "shop", "db", true)
	r.ManifestJson = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"shop"},"data":{"password":"x"}}`
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod", Platform: "eks", ProviderId: "arn:aws:eks:us-east-2:1:cluster/prod"},
		Resources: []*snapshotv1alpha1.Resource{r}}
	dir := t.TempDir()
	if err := (Emitter{ExternalSecrets: es}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ClusterDir(dir, "prod"), "platform", "aws", "secret-store.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var store map[string]any
	if err := yaml.Unmarshal(b, &store); err != nil || store["kind"] != "ClusterSecretStore" {
		t.Errorf("not a store manifest: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "service: SecretsManager") || !strings.Contains(string(b), "region: us-east-2") {
		t.Errorf("store:\n%s", b)
	}
}

// check that a kind or group from the cluster or a manifest file cant make a path that leaves the export, KindDir keeps it one directory name and every write goes through an os.Root on the export's directory
func TestPathsStayInsideExport(t *testing.T) {
	for _, kind := range []string{"../../../../../../ESCAPED", "..", "a/b", `..\..\x`} {
		if d := KindDir("v1", kind); strings.ContainsAny(d, `/\`) || strings.Trim(d, ".") == "" {
			t.Errorf("KindDir(v1, %q) = %q, not one plain directory name", kind, d)
		}
	}
	if d := KindDir("../../x/v1", "ConfigMap"); strings.ContainsAny(d, `/\`) {
		t.Errorf("KindDir with a group of dots = %q", d)
	}

	parent := t.TempDir()
	dir := filepath.Join(parent, "out")
	r := resource("ConfigMap", "shop", "x", false)
	r.Kind = "../../../../../../ESCAPED"
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "c"}, Resources: []*snapshotv1alpha1.Resource{r}}
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); p != parent && strings.HasPrefix(rel, "..") {
			t.Errorf("written outside the export: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := openOutDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer out.close()
	if err := out.write(filepath.Join("..", "escaped.yaml"), []byte("x")); err == nil {
		t.Error("outDir wrote a path above its directory")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
