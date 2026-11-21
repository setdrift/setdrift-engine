// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"

	yamlv3 "go.yaml.in/yaml/v3"

	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

const externalSecretsDir = "external-secrets"

// The AWS services ExternalSecrets read from (spec.provider.aws.service).
const (
	serviceSecretsManager = "SecretsManager"
	serviceParameterStore = "ParameterStore"
)

// regionPlaceholder stands in RESTORE.md's commands for an AWS region the
// snapshot does not show.
const regionPlaceholder = "<region>"

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
		x.Provider, x.Service = "AWS Secrets Manager", serviceSecretsManager
	case "aws-parameter-store":
		// Parameter names are paths and start with a slash.
		x.Provider, x.Service, x.KeyTemplate = "AWS Systems Manager Parameter Store", serviceParameterStore, "/"+defaultKeyTemplate
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
			r.Provider, r.Service, r.ID = "AWS Secrets Manager", serviceSecretsManager, "aws-secrets-manager"
		}
	}
	r.Auto = false
	return &r
}

// Validate checks the settings before anything is read or written, so a
// bad key template or store fails at once rather than partway through
// writing the export.
func (x *ExternalSecrets) Validate() error {
	if err := CheckStore(x.StoreKind, x.StoreName); err != nil {
		return err
	}
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
			return nil, fmt.Errorf("secret %s/%s: %w", r.Namespace, r.Name, err)
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
	if x.Service != "" && strings.HasPrefix(platDir, awsDir) {
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

	manifest := x.manifest(SecretSource{Namespace: r.Namespace, Name: r.Name, Type: secret.Type,
		Labels: secret.Metadata.Labels, Annotations: secret.Metadata.Annotations, Keys: keys, RemoteKey: remoteKey,
		// A controller creates this Secret and writes some of its keys (Argo
		// CD's argocd-secret): Merge adds the other keys to it without
		// owning it, so the two do not fight.
		Merge: len(Generated(r)) > 0})
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

// manifest is the ExternalSecret that rebuilds src from its remote secret,
// one property per key: with Merge, adding its keys to a Secret a controller
// creates; otherwise creating the Secret with its type, labels and
// annotations.
func (x *ExternalSecrets) manifest(src SecretSource) map[string]any {
	data := make([]any, 0, len(src.Keys))
	for _, k := range src.Keys {
		ref := map[string]any{"key": src.RemoteKey, "property": k}
		if slices.Contains(src.Binary, k) {
			ref["decodingStrategy"] = "Base64"
		}
		data = append(data, map[string]any{"secretKey": k, "remoteRef": ref})
	}
	target := map[string]any{"name": src.Name}
	if src.Merge {
		target["creationPolicy"] = "Merge"
	} else if t := secretTemplate(src.Type, src.Labels, src.Annotations); len(t) > 0 {
		target["template"] = t
	}
	return map[string]any{
		"apiVersion": "external-secrets.io/v1",
		"kind":       "ExternalSecret",
		"metadata":   map[string]any{"name": src.Name, "namespace": src.Namespace},
		"spec": map[string]any{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]any{"kind": x.StoreKind, "name": x.StoreName},
			"target":          target,
			"data":            data,
		},
	}
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

// regionOf returns the AWS region from the AWS inventory or an EKS cluster
// ARN, and whether the snapshot shows one.
func regionOf(s *snapshotv1alpha1.Snapshot) (string, bool) {
	if r := s.GetInventory().GetAws().GetRegion(); r != "" {
		return r, true
	}
	c := s.GetCluster()
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) > 3 && parts[0] == "arn" && parts[3] != "" {
		return parts[3], true
	}
	return "", false
}

// region returns the snapshot's AWS region, or regionPlaceholder, for a
// command shown to the user.
func region(s *snapshotv1alpha1.Snapshot) string {
	if r, ok := regionOf(s); ok {
		return r
	}
	return regionPlaceholder
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
		storeNS = "<namespace>"
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
// namespace is the SecretStore's namespace, "" for a ClusterSecretStore.
func (x *ExternalSecrets) awsStore(region, namespace string) string {
	var spec storeSpec
	spec.Spec.Provider.AWS = awsProvider{Service: x.Service, Region: region}
	return storeHead(x.StoreKind, x.StoreName, namespace) + marshalYAML(spec)
}

// storeMeta is a store's apiVersion, kind and metadata, as YAML writes them.
type storeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace,omitempty"`
	} `yaml:"metadata"`
}

// storeSpec is an AWS store's spec, as YAML writes it.
type storeSpec struct {
	Spec struct {
		Provider struct {
			AWS awsProvider `yaml:"aws"`
		} `yaml:"provider"`
	} `yaml:"spec"`
}

// awsProvider is a store's spec.provider.aws.
type awsProvider struct {
	Service string `yaml:"service"`
	Region  string `yaml:"region"`
}

// storeHead is a store's apiVersion, kind and metadata, as YAML.
func storeHead(kind, name, namespace string) string {
	m := storeMeta{APIVersion: "external-secrets.io/v1", Kind: kind}
	m.Metadata.Name, m.Metadata.Namespace = name, namespace
	return marshalYAML(m)
}

// marshalYAML writes v as YAML in its fields' order, two-space indented, so
// any value is quoted as YAML needs.
func marshalYAML(v any) string {
	var b bytes.Buffer
	enc := yamlv3.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		// v is one of this file's structs of strings: it always encodes
		panic(fmt.Sprintf("kustomize: marshalYAML: %v", err))
	}
	return b.String()
}

// storeNameRE matches a Kubernetes object name: a DNS subdomain.
var storeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// CheckStore checks an External Secrets store's kind and name: the kind is
// ClusterSecretStore or SecretStore, and the name a Kubernetes name.
func CheckStore(kind, name string) error {
	if kind != "ClusterSecretStore" && kind != "SecretStore" {
		return fmt.Errorf("store kind %q: use ClusterSecretStore or SecretStore", kind)
	}
	if !storeNameRE.MatchString(name) || len(name) > 253 {
		return fmt.Errorf("store name %q: use a Kubernetes name: lowercase letters, digits, dashes and dots", name)
	}
	return nil
}

// storeExamples shows the store for the common secret managers, with the
// remote key format each accepts.
func storeExamples(kind, name, namespace string) string {
	head := storeHead(kind, name, namespace) + "spec:\n  provider:\n"
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
