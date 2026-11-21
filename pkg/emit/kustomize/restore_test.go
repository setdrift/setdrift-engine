package kustomize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/internal/text"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func TestLineOf(t *testing.T) {
	doc := []byte(`apiVersion: v1
data:
  .dockerconfigjson: abc
  tls.crt: def
kind: Secret
spec:
  template:
    spec:
      containers:
      - env:
        - name: OTHER
          value: x
        - name: TOKEN
          value: SETDRIFT_REDACTED
        name: api
`)
	for path, want := range map[string]int{
		"data..dockerconfigjson": 3,
		"data.tls.crt":           4,
		"spec.template.spec.containers[api].env[TOKEN].value": 14,
		".spec.template.spec.containers[0].env[1].value":      14,
		"data.missing": 0,
	} {
		if got := lineOf(doc, path); got != want {
			t.Errorf("%s: line %d, want %d", path, got, want)
		}
	}
}

func TestChartOf(t *testing.T) {
	for label, want := range map[string][2]string{
		"external-secrets-2.15.0": {"external-secrets", "2.15.0"},
		"chart-0.1.0":             {"chart", "0.1.0"},
		"nginx":                   {"nginx", ""},
	} {
		if c, v := chartOf(label); c != want[0] || v != want[1] {
			t.Errorf("%s: %s %s", label, c, v)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"web": "web", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "arn:aws:iam::1:role/x": "arn:aws:iam::1:role/x"} {
		if got := text.ShellQuote(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestRestorePlaybook(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{
		Cluster:  &snapshotv1alpha1.Cluster{Name: "prod", Platform: "eks", ProviderId: "arn:aws:eks:us-east-2:111122223333:cluster/prod"},
		Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Resources: []*snapshotv1alpha1.Resource{
			{ApiVersion: "v1", Kind: "ServiceAccount", Namespace: "shop", Name: "web",
				ManifestJson: `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"web","namespace":"shop","annotations":{"eks.amazonaws.com/role-arn":"arn:aws:iam::111122223333:role/web"}}}`},
			{ApiVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "eks:addon-manager",
				ManifestJson: `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"eks:addon-manager"}}`,
				Provenance:   &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_PLATFORM, Manager: "eks"}},
			{ApiVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: "gp3",
				ManifestJson: `{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass","metadata":{"name":"gp3","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"ebs.csi.eks.amazonaws.com"}`},
			{ApiVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: "gp2",
				ManifestJson: `{"apiVersion":"storage.k8s.io/v1","kind":"StorageClass","metadata":{"name":"gp2"},"provisioner":"kubernetes.io/aws-ebs"}`,
				Provenance:   &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_PLATFORM, Manager: "eks"}},
			{ApiVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "old",
				ManifestJson: `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"old","namespace":"shop"},"spec":{"storageClassName":"gp2"}}`},
			{ApiVersion: "v1", Kind: "ConfigMap", Namespace: "web", Name: "web-cfg", ManifestJson: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"web-cfg","namespace":"web","labels":{"helm.sh/chart":"web-1.0.0"}}}`,
				Provenance: &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_HELM, Manager: "web/web"}},
			{ApiVersion: "v1", Kind: "ConfigMap", Namespace: "api", Name: "api-cfg", ManifestJson: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"api-cfg","namespace":"api"}}`,
				Provenance: &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_HELM, Manager: "api/api"}},
			{ApiVersion: "v1", Kind: "ConfigMap", Namespace: "shop", Name: "cfg",
				ManifestJson: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"shop"},"data":{"bucket":"arn:aws:s3:::my-bucket","sg":"sg-0123456789abcdef0"}}`},
		},
		Inventory: &snapshotv1alpha1.Inventory{
			Volumes: []*snapshotv1alpha1.Volume{{Namespace: "shop", Claim: "data-db-0", PersistentVolume: "pvc-1", StorageClass: "gp3", Capacity: "10Gi",
				AccessModes: []string{"ReadWriteOnce"}, Driver: autoModeEBSDriver, VolumeHandle: "vol-0123456789abcdef0", Zone: "us-east-2a",
				ReclaimPolicy: "Delete", StatefulSet: "db"}},
			HelmReleases: []*snapshotv1alpha1.HelmRelease{
				{Namespace: "web", Name: "web", Chart: "web", ChartVersion: "1.0.0", Repository: "https://charts.example.com"},
				{Namespace: "api", Name: "api", Chart: "api", ChartVersion: "2.0.0", Repository: "oci://registry.example.com/charts"},
			},
			LoadBalancers: []*snapshotv1alpha1.LoadBalancer{{Kind: "Service", Namespace: "shop", Name: "web", Hostnames: []string{"old.elb.amazonaws.com"}}},
		},
	}
	dir := t.TempDir()
	if err := (Emitter{SourceContext: "prod-ctx"}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, "prod")
	generic := readFile(t, root, "RESTORE.md")
	// The AWS steps are in the platform's playbooks; RESTORE.md points to them.
	md := generic + readFile(t, root, "platform/aws/RESTORE.md") + readFile(t, root, "platform/aws/eks/RESTORE.md")
	for _, aws := range []string{"aws eks ", "aws ec2 ", "aws iam ", "$REGION", "$DST_CLUSTER"} {
		if strings.Contains(generic, aws) {
			t.Errorf("generic RESTORE.md has %q", aws)
		}
	}
	if !strings.Contains(generic, "platform/aws/eks/RESTORE.md") || !strings.Contains(generic, "**If the new cluster is EKS too**") {
		t.Error("generic RESTORE.md does not point to the EKS steps")
	}
	for _, want := range []string{
		"SRC=prod-ctx", "REGION=us-east-2", "2 objects that EKS creates", // platform left out
		"aws eks list-pod-identity-associations", // no --aws
		"arn:aws:iam::111122223333:role/web", "update-assume-role-policy --role-name web",
		"`arn:aws:s3:::my-bucket`", "`sg-0123456789abcdef0`",
		`patch pv pvc-1 -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'`,
		"--volume-id vol-0123456789abcdef0", "> restore/volumes/shop/data-db-0.snapshot", "SNAP=$(cat restore/volumes/shop/data-db-0.snapshot)", "Key=eks:eks-cluster-name", "restore/volumes/shop/data-db-0.yaml",
		"old.elb.amazonaws.com",
		"install web web --repo https://charts.example.com -n web", "install api oci://registry.example.com/charts/api -n api",
		"get storageclass gp2 >/dev/null 2>&1 || kubectl --context \"$DST\" apply -f restore/storageclasses/gp2.yaml",
		"get storageclass gp3", "--field-manager=setdrift-restore", "create namespace shop",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("RESTORE.md lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "cluster-scoped", "clusterrole.rbac.authorization.k8s.io", "eks_addon-manager.yaml")); err == nil {
		t.Error("platform object written")
	}
	sc, err := os.ReadFile(filepath.Join(root, "restore", "storageclasses", "gp3.yaml"))
	if err != nil || strings.Contains(string(sc), "is-default-class") {
		t.Errorf("storage class file: %v\n%s", err, sc)
	}
	pv, err := os.ReadFile(filepath.Join(root, "restore", "volumes", "shop", "data-db-0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claimRef:", "name: data-db-0", "volumeHandle: vol-0123456789abcdef0", "persistentVolumeReclaimPolicy: Retain", "us-east-2a", "storageClassName: gp3",
		"kind: PersistentVolumeClaim", "volumeName: pvc-1"} {
		if !strings.Contains(string(pv), want) {
			t.Errorf("PersistentVolume lacks %q:\n%s", want, pv)
		}
	}
}

func TestGeneratedSecrets(t *testing.T) {
	gen := func(path string) *snapshotv1alpha1.Redaction {
		return &snapshotv1alpha1.Redaction{Path: path, Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE, Controller: "argocd-server"}
	}
	val := func(path string) *snapshotv1alpha1.Redaction {
		return &snapshotv1alpha1.Redaction{Path: path, Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE}
	}
	s := &snapshotv1alpha1.Snapshot{
		Cluster:  &snapshotv1alpha1.Cluster{Name: "prod"},
		Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Resources: []*snapshotv1alpha1.Resource{
			{ApiVersion: "v1", Kind: "Secret", Namespace: "argocd", Name: "argocd-redis",
				ManifestJson: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"argocd-redis","namespace":"argocd"},"type":"Opaque"}`,
				Redactions:   []*snapshotv1alpha1.Redaction{gen("data.auth")}},
			{ApiVersion: "v1", Kind: "Secret", Namespace: "argocd", Name: "argocd-secret",
				ManifestJson: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"argocd-secret","namespace":"argocd"},"type":"Opaque","data":{"webhook.github.secret":"U0VURFJJRlRfUkVEQUNURUQ="}}`,
				Redactions:   []*snapshotv1alpha1.Redaction{gen("data.server.secretkey"), val("data.webhook.github.secret")}},
		},
	}
	es := mustExternalSecrets(t, "aws-secrets-manager")
	dir := t.TempDir()
	if err := (Emitter{ExternalSecrets: es}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, "prod")
	md := mustRead(t, filepath.Join(root, "RESTORE.md"))
	for _, want := range []string{
		"- Secret argocd/argocd-redis, by argocd-server",
		"(Secret argocd/argocd-secret, merge: argocd-server writes the other keys)",
		`data: {"webhook.github.secret": .data["webhook.github.secret"]}`,
		"--field-manager=setdrift-restore -f redacted/namespaces/argocd/secret/argocd-secret.yaml.redacted",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("RESTORE.md lacks %q", want)
		}
	}
	if strings.Contains(string(md), "get secret argocd-redis") {
		t.Error("RESTORE.md restores a generated Secret")
	}
	if _, err := os.Stat(filepath.Join(root, "external-secrets", "namespaces", "argocd", "externalsecret.external-secrets.io", "argocd-redis.yaml")); err == nil {
		t.Error("ExternalSecret written for a generated Secret")
	}
	merged, err := os.ReadFile(filepath.Join(root, "external-secrets", "namespaces", "argocd", "externalsecret.external-secrets.io", "argocd-secret.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "creationPolicy: Merge") || strings.Contains(string(merged), "server.secretkey") || strings.Contains(string(merged), "template") {
		t.Errorf("merge ExternalSecret:\n%s", merged)
	}
}

// Pull Secrets are told apart by their type, the one thing the redacted
// manifest keeps about them.
func TestPullSecret(t *testing.T) {
	for manifest, want := range map[string]bool{
		`{"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"x"}}`: true,
		`{"type":"kubernetes.io/dockercfg"}`:                                         true,
		`{"type":"Opaque","data":{"password":"x"}}`:                                  false,
		`not json`: false,
	} {
		if got := pullSecret(&snapshotv1alpha1.Resource{ApiVersion: "v1", Kind: "Secret", ManifestJson: manifest}); got != want {
			t.Errorf("%s: %v, want %v", manifest, got, want)
		}
	}
}

// Redacted add-on configuration is named under the add-on commands, with the
// command that reads the real values from the source.
// Add-on configuration values never go into RESTORE.md, redacted or not:
// each command reads them from the source add-on as it runs.
func TestAddonConfigNotWritten(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{
		Cluster:  &snapshotv1alpha1.Cluster{Name: "prod", Platform: "eks", ProviderId: "arn:aws:eks:us-east-2:111122223333:cluster/prod"},
		Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Inventory: &snapshotv1alpha1.Inventory{Aws: &snapshotv1alpha1.AwsInventory{ClusterName: "prod", Region: "us-east-2", Addons: []*snapshotv1alpha1.Addon{
			{Name: "agent", Version: "v1", HasConfiguration: true},
			{Name: "plain", Version: "v1", HasConfiguration: true},
			{Name: "none", Version: "v1"},
		}}},
	}
	dir := t.TempDir()
	if err := (Emitter{SourceContext: "prod-ctx"}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, "prod")
	md := readFile(t, root, "RESTORE.md") + readFile(t, root, "platform/aws/eks/RESTORE.md")
	for _, leaked := range []string{"site", "replicas", "missed-by-the-patterns", "SETDRIFT_REDACTED"} {
		if strings.Contains(md, leaked) {
			t.Errorf("RESTORE.md holds the configuration value %q", leaked)
		}
	}
	for _, name := range []string{"agent", "plain"} {
		lookup := `CONFIG=$(aws eks describe-addon --region "$REGION" --cluster-name prod --addon-name ` + name + ` --query addon.configurationValues --output text)`
		create := `--addon-name ` + name + ` --addon-version v1 --configuration-values "$CONFIG"`
		if !strings.Contains(md, lookup) || !strings.Contains(md, create) {
			t.Errorf("RESTORE.md lacks the lookup for %s:\n%s", name, md)
		}
	}
	if strings.Contains(md, "--addon-name none --query") {
		t.Error("a configuration lookup for an add-on with none")
	}
}

// A chart installed by hand gets the repository Argo CD or Flux installs the
// same chart from; a chart from two repositories, or a release with no
// recorded chart, gets none, with the command that shows the chart.
func TestHelmRepoFromGitOps(t *testing.T) {
	obj := func(apiVersion, kind, ns, name, spec string) *snapshotv1alpha1.Resource {
		return &snapshotv1alpha1.Resource{ApiVersion: apiVersion, Kind: kind, Namespace: ns, Name: name,
			ManifestJson: `{"apiVersion":"` + apiVersion + `","kind":"` + kind + `","metadata":{"name":"` + name + `","namespace":"` + ns + `"},"spec":` + spec + `}`,
			Provenance:   &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_UNMANAGED}}
	}
	helmObj := func(ns, release, chart string) *snapshotv1alpha1.Resource {
		r := obj("v1", "ConfigMap", ns, release+"-cm", "{}")
		r.ManifestJson = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + release + `-cm","namespace":"` + ns + `"}}`
		if chart != "" {
			r.ManifestJson = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + release + `-cm","namespace":"` + ns + `","labels":{"helm.sh/chart":"` + chart + `"}}}`
		}
		r.Provenance = &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_HELM, Manager: ns + "/" + release}
		return r
	}
	s := &snapshotv1alpha1.Snapshot{
		Cluster:  &snapshotv1alpha1.Cluster{Name: "prod"},
		Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Resources: []*snapshotv1alpha1.Resource{
			obj("argoproj.io/v1alpha1", "Application", "argocd", "monitoring",
				`{"source":{"repoURL":"https://prometheus-community.github.io/helm-charts","chart":"kube-prometheus-stack"}}`),
			obj("source.toolkit.fluxcd.io/v1", "HelmRepository", "flux-system", "ingress", `{"url":"https://kubernetes.github.io/ingress-nginx"}`),
			obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "edge", "ingress",
				`{"chart":{"spec":{"chart":"ingress-nginx","sourceRef":{"kind":"HelmRepository","name":"ingress","namespace":"flux-system"}}}}`),
			obj("source.toolkit.fluxcd.io/v1", "OCIRepository", "apps", "podinfo", `{"url":"oci://ghcr.io/stefanprodan/charts/podinfo"}`),
			obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "apps", "podinfo", `{"chartRef":{"kind":"OCIRepository","name":"podinfo"}}`),
			// two repositories for one chart: no guess
			obj("argoproj.io/v1alpha1", "Application", "argocd", "a", `{"sources":[{"repoURL":"https://one.example.com","chart":"web"}]}`),
			obj("argoproj.io/v1alpha1", "Application", "argocd", "b", `{"source":{"repoURL":"https://two.example.com","chart":"web"}}`),
			helmObj("monitoring", "prom", "kube-prometheus-stack-60.0.0"),
			helmObj("ingress", "nginx", "ingress-nginx-4.11.0"),
			helmObj("apps", "podinfo-manual", "podinfo-6.7.0"),
			helmObj("shop", "web", "web-1.0.0"),
			helmObj("shop", "mystery", ""),
		},
	}
	dir := t.TempDir()
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	b := mustRead(t, filepath.Join(ClusterDir(dir, "prod"), "RESTORE.md"))
	md := string(b)
	for _, want := range []string{
		"install prom kube-prometheus-stack --repo https://prometheus-community.github.io/helm-charts",
		"install nginx ingress-nginx --repo https://kubernetes.github.io/ingress-nginx",
		"install podinfo-manual oci://ghcr.io/stefanprodan/charts/podinfo",
		"install web <repo>/web",
		"- prom: https://prometheus-community.github.io/helm-charts, as Argo CD Application argocd/monitoring installs it",
		"- nginx: https://kubernetes.github.io/ingress-nginx, as Flux HelmRelease edge/ingress installs it",
		`helm --kube-context "$SRC" list -n shop --filter '^mystery$'`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("RESTORE.md lacks %q", want)
		}
	}
}

// A Secret its controller only fills in is created empty, then its namespace
// restarted; one the controller creates whole stays "nothing to do".
func TestSecretsFilledByController(t *testing.T) {
	secret := func(name string, finding bool) *snapshotv1alpha1.Resource {
		r := &snapshotv1alpha1.Resource{ApiVersion: "v1", Kind: "Secret", Namespace: "argocd", Name: name,
			ManifestJson: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + name + `","namespace":"argocd"},"data":{}}`,
			Redactions:   []*snapshotv1alpha1.Redaction{{Path: "data.k", Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE, Controller: "argocd-server"}}}
		if finding {
			r.Findings = []*snapshotv1alpha1.Finding{{Code: "SECRET_FILLED_BY_CONTROLLER"}}
		}
		return r
	}
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"}, Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Resources: []*snapshotv1alpha1.Resource{secret("argocd-secret", true), secret("argocd-initial-admin-secret", false)}}
	dir := t.TempDir()
	if err := (Emitter{}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	b := mustRead(t, filepath.Join(ClusterDir(dir, "prod"), "RESTORE.md"))
	md := string(b)
	for _, want := range []string{
		"**Create these empty.**",
		`apply --server-side --field-manager=setdrift-restore -f redacted/namespaces/argocd/secret/argocd-secret.yaml.redacted   # filled in by argocd-server`,
		`kubectl --context "$DST" -n argocd rollout restart deployment,statefulset`,
		"- Secret argocd/argocd-initial-admin-secret, by argocd-server",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("RESTORE.md lacks %q", want)
		}
	}
	if strings.Contains(md, "argocd-initial-admin-secret.yaml.redacted   # filled") {
		t.Error("a Secret the controller creates whole is created empty")
	}
}

// readFile reads a file under root, or "" if there is none.
func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mustReadDir returns the entries of dir, failing the test if it can't be read.
func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// mustExternalSecrets is DefaultExternalSecrets, failing the test on an error.
func mustExternalSecrets(t *testing.T, provider string) *ExternalSecrets {
	t.Helper()
	x, err := DefaultExternalSecrets(provider)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
