// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/template"

	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

const externalSecretsDir = "external-secrets"

// ExternalSecrets configures the ExternalSecret written for each redacted secret
type ExternalSecrets struct {
	// Provider names the secret manager, for the README.
	Provider string
	// Service is the provider's spec.provider.aws.service in the example
	// ClusterSecretStore: SecretsManager or ParameterStore.
	Service string
	// StoreKind is ClusterSecretStore or SecretStore.
	StoreKind string
	// StoreName is the store the ExternalSecrets reference.
	StoreName string
	// KeyTemplate is a text/template for each Secret's remote key, with .Cluster, .Namespace and .Name.
	KeyTemplate string
	// Auto picks the provider from the cluster's platform at export (see resolved).
	Auto bool
	// ID is the provider as --external-secrets names it, e.g. aws-secrets-manager.
	ID string
}

// ExternalSecretProviders are the providers DefaultExternalSecrets knows.
// auto is AWS Secrets Manager on a cluster on AWS, else generic: any secret
// manager, with examples of stores for the common ones.
var ExternalSecretProviders = []string{"auto", "generic", "aws-secrets-manager", "aws-parameter-store"}

// DefaultStoreName is the store every ExternalSecret references unless
// --secret-store names another: provider-neutral, so the same manifests work
// with whichever secret manager the store is set up for.
const DefaultStoreName = "secret-store"

const defaultKeyTemplate = "{{.Cluster}}/{{.Namespace}}/{{.Name}}"

// DefaultExternalSecrets returns the defaults for a provider.
func DefaultExternalSecrets(provider string) (*ExternalSecrets, error) {
	x := &ExternalSecrets{StoreKind: "ClusterSecretStore", StoreName: DefaultStoreName, KeyTemplate: defaultKeyTemplate, ID: provider}
	switch provider {
	case "auto":
		x.Auto = true
		x.Provider = "your secret manager"
	case "generic":
		x.Provider = "your secret manager"
	case "aws-secrets-manager":
		x.Provider, x.Service = "AWS Secrets Manager", "SecretsManager"
	case "aws-parameter-store":
		// Parameter names are paths and start with a slash.
		x.Provider, x.Service, x.KeyTemplate = "AWS Systems Manager Parameter Store", "ParameterStore", "/"+defaultKeyTemplate
	default:
		return nil, fmt.Errorf("unknown external secrets provider %q (known: %s)", provider, strings.Join(ExternalSecretProviders, ", "))
	}
	return x, nil
}

// resolved is x with auto settled for the cluster's platform: AWS Secrets
// Manager on AWS, generic elsewhere. The store name and key template are
// kept, whether defaults or given with flags.
func (x *ExternalSecrets) resolved(cls *platform.Classification) *ExternalSecrets {
	r := *x
	if r.Auto {
		r.ID = "generic"
		if cls.On(platform.AWS) || cls.Is(platform.EKS) {
			r.Provider, r.Service, r.ID = "AWS Secrets Manager", "SecretsManager", "aws-secrets-manager"
		}
	}
	r.Auto = false
	return &r
}

// Validate checks the settings before anything is read or written, so a bad key template fails at once rather than partway through writing the export.
func (x *ExternalSecrets) Validate() error {
	_, err := x.keyTemplate()
	return err
}

// keyTemplate parses KeyTemplate and runs it once on sample values, which catches unknown fields (e.g. {{.Namespce}}) as well as syntax errors.
func (x *ExternalSecrets) keyTemplate() (*template.Template, error) {
	tmpl, err := template.New("key").Option("missingkey=error").Parse(x.KeyTemplate)
	if err == nil {
		err = tmpl.Execute(io.Discard, map[string]string{"Cluster": "c", "Namespace": "ns", "Name": "n"})
	}
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

// remoteSecret is one secret to create in the secret manager.
type remoteSecret struct {
	key        string
	properties []string
	secret     string // namespace/name of the Secret it replaces
}

// emit writes external-secrets/ for the redacted Secrets in resources, and returns the remote secrets they read.
func (x *ExternalSecrets) emit(s *snapshotv1alpha1.Snapshot, resources []*snapshotv1alpha1.Resource, root, platDir string) ([]remoteSecret, error) {
	tmpl, err := x.keyTemplate()
	if err != nil {
		return nil, fmt.Errorf("secret key template: %w", err)
	}
	var out []*snapshotv1alpha1.Resource
	var remotes []remoteSecret
	for _, r := range resources {
		if r.ApiVersion != "v1" || r.Kind != "Secret" || !ReferenceOnly(r) {
			continue
		}
		es, remote, err := x.externalSecret(s.GetCluster().GetName(), r, tmpl)
		if err != nil {
			return nil, fmt.Errorf("Secret %s/%s: %w", r.Namespace, r.Name, err)
		}
		if es != nil {
			out = append(out, es)
			remotes = append(remotes, remote)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	// A template without .Namespace (or .Name) can give two Secrets one remote secret: one set of values for both.
	byKey := map[string]string{}
	for _, r := range remotes {
		if other, ok := byKey[r.key]; ok {
			return nil, fmt.Errorf("secret key template gives the remote secret %q to both Secret %s and Secret %s; "+
				"include {{.Namespace}} and {{.Name}} in --secret-key-template", r.key, other, r.secret)
		}
		byKey[r.key] = r.secret
	}
	sortRemotes(remotes)

	paths, err := filePaths(out, externalSecretsDir)
	if err != nil {
		return nil, err
	}
	var files []string
	for i, r := range out {
		path := filepath.Join(paths[i].bucket, paths[i].rel)
		if _, err := writeManifest(filepath.Join(root, path), r); err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(externalSecretsDir, path)
		files = append(files, filepath.ToSlash(rel))
	}
	sort.Strings(files)
	if err := writeKustomization(filepath.Join(root, externalSecretsDir), files); err != nil {
		return nil, err
	}
	// The exact store for the source's cloud goes with the platform's files.
	storeFile := ""
	if x.Service != "" && strings.HasPrefix(platDir, "platform/aws/") {
		storeFile = filepath.ToSlash(filepath.Join(filepath.Dir(platDir), "secret-store.yaml"))
		if err := writeFile(filepath.Join(root, storeFile), []byte(x.awsStore(region(s), ""))); err != nil {
			return nil, err
		}
	}
	return remotes, writeFile(filepath.Join(root, externalSecretsDir, "README.md"), []byte(x.readme(s, remotes, storeFile)))
}

// externalSecret builds the ExternalSecret for one redacted Secret: one remote secret per Kubernetes Secret, holding each key as a property.
func (x *ExternalSecrets) externalSecret(cluster string, r *snapshotv1alpha1.Resource, tmpl *template.Template) (*snapshotv1alpha1.Resource, remoteSecret, error) {
	var secret struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Type       string         `json:"type"`
		Data       map[string]any `json:"data"`
		StringData map[string]any `json:"stringData"`
	}
	if err := json.Unmarshal([]byte(r.ManifestJson), &secret); err != nil {
		return nil, remoteSecret{}, err
	}
	keySet := map[string]bool{}
	for k := range secret.Data {
		keySet[k] = true
	}
	for k := range secret.StringData {
		keySet[k] = true
	}
	if len(keySet) == 0 {
		return nil, remoteSecret{}, nil
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	if err := tmpl.Execute(&b, map[string]string{"Cluster": cluster, "Namespace": r.Namespace, "Name": r.Name}); err != nil {
		return nil, remoteSecret{}, fmt.Errorf("secret key template: %w", err)
	}
	remoteKey := b.String()

	data := make([]any, 0, len(keys))
	for _, k := range keys {
		data = append(data, map[string]any{
			"secretKey": k,
			"remoteRef": map[string]any{"key": remoteKey, "property": k},
		})
	}
	target := map[string]any{"name": r.Name}
	if len(Generated(r)) > 0 {
		// A controller creates this Secret and writes some of its keys (Argo CD's argocd-secret)
		// Merge adds the other keys to it without owning it, so the two do not fight
		target["creationPolicy"] = "Merge"
	} else if t := secretTemplate(secret.Type, secret.Metadata.Labels, secret.Metadata.Annotations); len(t) > 0 {
		target["template"] = t
	}
	manifest := map[string]any{
		"apiVersion": "external-secrets.io/v1",
		"kind":       "ExternalSecret",
		"metadata":   map[string]any{"name": r.Name, "namespace": r.Namespace},
		"spec": map[string]any{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]any{"kind": x.StoreKind, "name": x.StoreName},
			"target":          target,
			"data":            data,
		},
	}
	js, err := json.Marshal(manifest)
	if err != nil {
		return nil, remoteSecret{}, err
	}
	return &snapshotv1alpha1.Resource{
			ApiVersion: "external-secrets.io/v1", Kind: "ExternalSecret", Namespace: r.Namespace, Name: r.Name,
			ManifestJson: string(js), Provenance: r.Provenance,
		}, remoteSecret{key: remoteKey, properties: keys, secret: r.Namespace + "/" + r.Name},
		nil
}

// sortRemotes sorts remote secrets by key, for the README and RESTORE.md
func sortRemotes(remotes []remoteSecret) {
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].key < remotes[j].key })
}

// secretTemplate keeps the Secret's type (e.g. kubernetes.io/dockerconfigjson), and its labels and annotations
func secretTemplate(typ string, labels, annotations map[string]string) map[string]any {
	t := map[string]any{}
	if typ != "" && typ != "Opaque" {
		t["type"] = typ
	}
	meta := map[string]any{}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	if len(meta) > 0 {
		t["metadata"] = meta
	}
	return t
}

// region returns the AWS region from the AWS inventory or an EKS cluster ARN, or a placeholder.
func region(s *snapshotv1alpha1.Snapshot) string {
	if r := s.GetInventory().GetAws().GetRegion(); r != "" {
		return r
	}
	c := s.GetCluster()
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) > 3 && parts[0] == "arn" && parts[3] != "" {
		return parts[3]
	}
	return "<region>"
}

// readme is external-secrets/README.md: how to set up the store and the remote secrets, then apply the directory
func (x *ExternalSecrets) readme(s *snapshotv1alpha1.Snapshot, remotes []remoteSecret, storeFile string) string {
	// A SecretStore is namespaced: each namespace with an ExternalSecret needs its own.
	where, storeNS := "", ""
	if x.StoreKind == "SecretStore" {
		var namespaces []string
		for _, r := range remotes {
			ns, _, _ := strings.Cut(r.secret, "/")
			if !slices.Contains(namespaces, ns) {
				namespaces = append(namespaces, ns)
			}
		}
		sort.Strings(namespaces)
		where = ", in each of these namespaces\n   (a SecretStore only serves its own): " + strings.Join(namespaces, ", ")
		storeNS = "\n  namespace: <namespace>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# External secrets\n\n"+
		"One ExternalSecret for each Secret under ../redacted/. External Secrets\n"+
		"Operator creates each Secret from %s, so the values never go\n"+
		"into Git. SetDrift did not read or copy any value.\n\n"+
		"These are NOT applied: ../kustomization.yaml does not list this directory.\n"+
		"External Secrets Operator overwrites a Secret's data with what it reads from\n"+
		"the secret manager, so apply them only once every remote secret below holds\n"+
		"the right values.\n\n", x.Provider)
	fmt.Fprintf(&b, "1. Install External Secrets Operator, with access to %s.\n", x.Provider)
	fmt.Fprintf(&b, "2. Create the store the ExternalSecrets use, a %s named `%s`%s.\n", x.StoreKind, x.StoreName, where)
	switch {
	case storeFile != "":
		fmt.Fprintf(&b, "   On AWS: ../%s (Secrets Manager in %s; give the operator access\n"+
			"   with EKS Pod Identity or IRSA). Elsewhere, one of the examples below.\n", storeFile, region(s))
	case x.Service != "":
		b.WriteString("   For " + x.Provider + ":\n\n" + indent("```yaml\n"+x.awsStore(region(s), storeNS)+"```\n", "   ") + "\n")
	default:
		b.WriteString("   One of the examples below, for your secret manager.\n")
	}
	b.WriteString(`3. Create each remote secret below, as JSON with one property per key,
   copying the values from the live Secrets. A binary value (e.g. a
   keystore) can't go into JSON as is: store it base64-encoded and add
   ` + "`decodingStrategy: Base64`" + ` to its remoteRef in the ExternalSecret.
4. Add ` + "`- external-secrets`" + ` to ../kustomization.yaml.

| Remote secret | Properties | Replaces Secret |
|---|---|---|
`)
	for _, r := range remotes {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.key, "`"+strings.Join(r.properties, "`, `")+"`", r.secret)
	}
	if x.Service == "" || storeFile != "" {
		b.WriteString("\n" + storeExamples(x.StoreKind, x.StoreName, storeNS))
	}
	return b.String()
}

// awsStore is the store for AWS Secrets Manager or Parameter Store, as YAML.
func (x *ExternalSecrets) awsStore(region, ns string) string {
	return fmt.Sprintf("apiVersion: external-secrets.io/v1\nkind: %s\nmetadata:\n  name: %s%s\nspec:\n  provider:\n    aws:\n"+
		"      service: %s\n      region: %s\n", x.StoreKind, x.StoreName, ns, x.Service, region)
}

// storeExamples shows the store for the common secret managers, with the
// remote key format each accepts.
func storeExamples(kind, name, ns string) string {
	head := fmt.Sprintf("apiVersion: external-secrets.io/v1\nkind: %s\nmetadata:\n  name: %s%s\nspec:\n  provider:\n", kind, name, ns)
	ex := func(title, note, provider string) string {
		return "**" + title + ".** " + note + "\n\n```yaml\n" + head + provider + "```\n\n"
	}
	return "## Stores for common secret managers\n\n" +
		"The ExternalSecrets read from `" + name + "`; create it for the secret manager\n" +
		"you use, and give the operator access to it.\n\n" +
		ex("AWS Secrets Manager", "Remote keys as above.",
			"    aws:\n      service: SecretsManager\n      region: <region>\n") +
		ex("Azure Key Vault", "Secret names allow only letters, digits and dashes: export again\n"+
			"with `--secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'`.",
			"    azurekv:\n      vaultUrl: https://<vault-name>.vault.azure.net\n      authType: WorkloadIdentity\n"+
				"      serviceAccountRef:\n        name: external-secrets\n        namespace: external-secrets\n") +
		ex("Google Cloud Secret Manager", "Secret names allow letters, digits, dashes and underscores: export\n"+
			"again with `--secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'`.",
			"    gcpsm:\n      projectID: <project-id>\n") +
		ex("HashiCorp Vault", "Remote keys as above, under the KV mount.",
			"    vault:\n      server: https://vault.example.com\n      path: secret\n      version: v2\n"+
				"      auth:\n        kubernetes:\n          mountPath: kubernetes\n          role: external-secrets\n")
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
