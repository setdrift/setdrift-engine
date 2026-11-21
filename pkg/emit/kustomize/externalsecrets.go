// writes an ExternalSecret for each redacted Secret, and the README that says how to set up the secret store and the remote secrets
// the values themselves are never read or copied, they go from the secret manager straight to the cluster

package kustomize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// externalSecretsDir is the directory the ExternalSecrets go in, inside the cluster's directory
const externalSecretsDir = "external-secrets"

// the aws services ExternalSecrets read from (spec.provider.aws.service)
const (
	serviceSecretsManager = "SecretsManager"
	serviceParameterStore = "ParameterStore"
)

// regionPlaceholder stands in RESTORE.md's commands for an aws region the snapshot doesnt show
const regionPlaceholder = "<region>"

// ExternalSecrets holds the settings for the ExternalSecret written for each redacted Secret
type ExternalSecrets struct {
	// Provider names the secret manager, for the README
	Provider string
	// Service is the provider's spec.provider.aws.service in the example ClusterSecretStore, SecretsManager or ParameterStore
	Service string
	// StoreKind is ClusterSecretStore or SecretStore
	StoreKind string
	// StoreName is the store the ExternalSecrets reference
	StoreName string
	// KeyTemplate is a text/template for each Secret's remote key, with .Cluster, .Namespace and .Name
	KeyTemplate string
	// Auto picks the provider from the cluster's platform at export (see resolved)
	Auto bool
	// ID is the provider as --external-secrets names it, e.g. aws-secrets-manager
	ID string
}

// ExternalSecretProviders are the providers DefaultExternalSecrets knows
// auto is aws secrets manager on a cluster on aws, else generic, which is any secret manager with examples of stores for the common ones
var ExternalSecretProviders = []string{"auto", "generic", "aws-secrets-manager", "aws-parameter-store"}

// DefaultStoreName is the store every ExternalSecret references unless --secret-store names another
// its provider-neutral, so the same manifests work with whichever secret manager the store is set up for
const DefaultStoreName = "secret-store"

// defaultKeyTemplate is the remote key of each Secret unless --secret-key-template gives another
const defaultKeyTemplate = "{{.Cluster}}/{{.Namespace}}/{{.Name}}"

// DefaultExternalSecrets gives back the default settings for a provider, an unknown provider is a terminal error
func DefaultExternalSecrets(provider string) (*ExternalSecrets, error) {
	x := &ExternalSecrets{StoreKind: "ClusterSecretStore", StoreName: DefaultStoreName, KeyTemplate: defaultKeyTemplate, ID: provider}
	// checking which provider was asked for, each one fills in its name and aws service
	switch provider {
	case "auto":
		x.Auto = true
		x.Provider = "your secret manager"
	case "generic":
		x.Provider = "your secret manager"
	case "aws-secrets-manager":
		x.Provider, x.Service = "AWS Secrets Manager", serviceSecretsManager
	case "aws-parameter-store":
		// parameter names are paths and start with a slash
		x.Provider, x.Service, x.KeyTemplate = "AWS Systems Manager Parameter Store", serviceParameterStore, "/"+defaultKeyTemplate
	default:
		return nil, fmt.Errorf("unknown external secrets provider %q (known: %s)", provider, strings.Join(ExternalSecretProviders, ", "))
	}
	return x, nil
}

// resolved gives back a copy of x with auto settled for the cluster's platform, aws secrets manager on aws, generic elsewhere
// the store name and key template are kept, whether defaults or given with flags
func (x *ExternalSecrets) resolved(cls *platform.Classification) *ExternalSecrets {
	r := *x
	// checking if the provider is auto, if so it gets settled here
	if r.Auto {
		r.ID = "generic"
		// invoking cls.On and cls.Is to tell if the cluster is on aws or is eks, theyre defined in pkg/platform/platform.go
		if cls.On(platform.AWS) || cls.Is(platform.EKS) {
			r.Provider, r.Service, r.ID = "AWS Secrets Manager", serviceSecretsManager, "aws-secrets-manager"
		}
	}
	r.Auto = false
	return &r
}

// Validate checks the settings before anything is read or written, so a bad key template or store fails at once rather than partway through writing the export
func (x *ExternalSecrets) Validate() error {
	// invoking CheckStore to check the store's kind and name, its defined below
	if err := CheckStore(x.StoreKind, x.StoreName); err != nil {
		return err
	}
	// invoking keyTemplate to check the key template parses, its defined below
	_, err := x.keyTemplate()
	return err
}

// keyTemplate parses KeyTemplate, refusing syntax errors and unknown fields (e.g. {{.Namespce}})
func (x *ExternalSecrets) keyTemplate() (*text.KeyTemplate, error) {
	// invoking text.ParseKeyTemplate to parse and check it, its defined in internal/text/keytemplate.go
	return text.ParseKeyTemplate(x.KeyTemplate)
}

// SecretSource is one redacted Secret as its ExternalSecret needs it, the remote secret its values are read from and how to rebuild it
type SecretSource struct {
	// the Secret's own namespace, name and type
	Namespace, Name, Type string
	// the Secret's labels and annotations, kept on the rebuilt Secret
	Labels, Annotations map[string]string
	// Keys are the Secret keys, each a property of RemoteKey
	Keys []string
	// RemoteKey is the remote secret in the secret manager
	RemoteKey string
	// Merge means a controller writes the Secret's other keys, the ExternalSecret adds these without owning it
	Merge bool
}

// remoteSecret is one secret to create in the secret manager, for the README and RESTORE.md
type remoteSecret struct {
	// the remote key the ExternalSecret reads
	key string
	// the properties it holds, one per Secret key
	properties []string
	// namespace/name of the Secret it replaces
	secret string
}

// emit writes external-secrets/ for the redacted Secrets in resources, and gives back the remote secrets they read
// on aws it also writes the store next to the platform's files
func (x *ExternalSecrets) emit(s *snapshotv1alpha1.Snapshot, resources []*snapshotv1alpha1.Resource, out *outDir, platDir string) ([]remoteSecret, error) {
	// invoking keyTemplate to parse the key template, its defined above
	tmpl, err := x.keyTemplate()
	if err != nil {
		return nil, fmt.Errorf("secret key template: %w", err)
	}
	var ess []*snapshotv1alpha1.Resource
	var remotes []remoteSecret
	// going through the resources to build an ExternalSecret for each redacted Secret
	for _, r := range resources {
		// checking if r isnt a Secret or wasnt redacted, invoking ReferenceOnly which is defined in kustomize.go, if so its skipped
		if r.ApiVersion != "v1" || r.Kind != "Secret" || !ReferenceOnly(r) {
			continue
		}
		// invoking externalSecret to build the ExternalSecret and its remote secret, its defined below
		es, remote, err := x.externalSecret(s.GetCluster().GetName(), r, tmpl)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s: %w", r.Namespace, r.Name, err)
		}
		// checking if the Secret had keys, a Secret without any gets no ExternalSecret
		if es != nil {
			ess = append(ess, es)
			remotes = append(remotes, remote)
		}
	}
	// checking if there was no ExternalSecret to write, if so external-secrets/ isnt written at all
	if len(ess) == 0 {
		return nil, nil
	}
	// a template without .Namespace (or .Name) can give two Secrets one remote secret, one set of values for both, thats a terminal error
	byKey := map[string]string{}
	// going through the remote secrets to find a key given twice
	for _, r := range remotes {
		// checking if another Secret already has this key
		if other, ok := byKey[r.key]; ok {
			return nil, fmt.Errorf("secret key template gives the remote secret %q to both Secret %s and Secret %s; "+
				"include {{.Namespace}} and {{.Name}} in --secret-key-template", r.key, other, r.secret)
		}
		byKey[r.key] = r.secret
	}
	// invoking sortRemotes to order them by key, its defined below
	sortRemotes(remotes)

	// invoking filePaths to give each ExternalSecret its file under external-secrets/, its defined in kustomize.go
	paths, err := filePaths(ess, externalSecretsDir)
	if err != nil {
		return nil, err
	}
	var files []string
	// going through the ExternalSecrets to write each one
	for i, r := range ess {
		path := filepath.Join(paths[i].bucket, paths[i].rel)
		// invoking writeManifest to write it as yaml, its defined in kustomize.go
		if _, err := out.writeManifest(path, r); err != nil {
			return nil, err
		}
		// the path relative to external-secrets/ for its kustomization, path is under externalSecretsDir so this cant fail
		rel, _ := filepath.Rel(externalSecretsDir, path)
		files = append(files, filepath.ToSlash(rel))
	}
	sort.Strings(files)
	// invoking writeKustomization to write external-secrets/kustomization.yaml, its defined in kustomize.go
	if err := out.writeKustomization(externalSecretsDir, files); err != nil {
		return nil, err
	}
	// the exact store for the source's cloud goes with the platform's files
	storeFile := ""
	// checking if the provider is an aws service and the platform set is under platform/aws/, if so the store is written there
	if x.Service != "" && strings.HasPrefix(platDir, awsDir) {
		storeFile = filepath.ToSlash(filepath.Join(filepath.Dir(platDir), "secret-store.yaml"))
		// invoking awsStore and region, both defined below, for the store's yaml, and out.write to write it, its defined in kustomize.go
		if err := out.write(filepath.FromSlash(storeFile), []byte(x.awsStore(region(s), ""))); err != nil {
			return nil, err
		}
	}
	// invoking readme to build external-secrets/README.md, its defined below, and out.write to write it
	return remotes, out.write(filepath.Join(externalSecretsDir, "README.md"), []byte(x.readme(s, remotes, storeFile)))
}

// externalSecret builds the ExternalSecret for one redacted Secret, one remote secret per kubernetes Secret, holding each key as a property
// a Secret with no keys gives back no ExternalSecret
func (x *ExternalSecrets) externalSecret(cluster string, r *snapshotv1alpha1.Resource, tmpl *text.KeyTemplate) (*snapshotv1alpha1.Resource, remoteSecret, error) {
	// secret holds the parts of the Secret's manifest the ExternalSecret needs
	var secret struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Type       string         `json:"type"`
		Data       map[string]any `json:"data"`
		StringData map[string]any `json:"stringData"`
	}
	// decoding the Secret's manifest
	if err := json.Unmarshal([]byte(r.ManifestJson), &secret); err != nil {
		return nil, remoteSecret{}, err
	}
	keySet := map[string]bool{}
	// collecting the keys of data
	for k := range secret.Data {
		keySet[k] = true
	}
	// collecting the keys of stringData too
	for k := range secret.StringData {
		keySet[k] = true
	}
	// checking if the Secret has no keys, if so theres nothing for an ExternalSecret to read
	if len(keySet) == 0 {
		return nil, remoteSecret{}, nil
	}
	keys := make([]string, 0, len(keySet))
	// turning the key set into a sorted list
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// invoking tmpl.Execute to build the remote key for this Secret, its defined in internal/text/keytemplate.go
	remoteKey := tmpl.Execute(cluster, r.Namespace, r.Name)

	// invoking manifest to build the ExternalSecret, its defined below
	manifest := x.manifest(SecretSource{Namespace: r.Namespace, Name: r.Name, Type: secret.Type,
		Labels: secret.Metadata.Labels, Annotations: secret.Metadata.Annotations, Keys: keys, RemoteKey: remoteKey,
		// a controller creates this Secret and writes some of its keys (argo cd's argocd-secret), Merge adds the other keys to it without owning it, so the two dont fight
		// invoking Generated to tell if the controller wrote any, its defined in kustomize.go
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

// manifest builds the ExternalSecret that rebuilds src from its remote secret, one property per key
// with Merge it adds its keys to a Secret a controller creates, otherwise it creates the Secret with its type, labels and annotations
func (x *ExternalSecrets) manifest(src SecretSource) map[string]any {
	data := make([]any, 0, len(src.Keys))
	// going through the keys to give each its remoteRef
	for _, k := range src.Keys {
		ref := map[string]any{"key": src.RemoteKey, "property": k}
		data = append(data, map[string]any{"secretKey": k, "remoteRef": ref})
	}
	target := map[string]any{"name": src.Name}
	// checking if a controller owns the Secret, if so the ExternalSecret merges into it
	// otherwise invoking secretTemplate to keep the Secret's type, labels and annotations, its defined below, when theres any
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

// secretTemplate builds the ExternalSecret's target template, keeping the Secret's type (e.g. kubernetes.io/dockerconfigjson), and its labels and annotations
func secretTemplate(typ string, labels, annotations map[string]string) map[string]any {
	t := map[string]any{}
	// checking if the type is anything but the default Opaque, if so its kept
	if typ != "" && typ != "Opaque" {
		t["type"] = typ
	}
	meta := map[string]any{}
	// checking if the Secret has labels, if so theyre kept
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	// checking if the Secret has annotations, if so theyre kept
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	// checking if any metadata is kept, if so the template gets it
	if len(meta) > 0 {
		t["metadata"] = meta
	}
	return t
}

// regionOf gives back the aws region from the aws inventory or an eks cluster arn, and whether the snapshot shows one
func regionOf(s *snapshotv1alpha1.Snapshot) (string, bool) {
	// checking if the aws inventory (--aws) recorded the region
	if r := s.GetInventory().GetAws().GetRegion(); r != "" {
		return r, true
	}
	c := s.GetCluster()
	// checking if the provider id is an eks cluster arn, if so its fourth colon-separated part is the region
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) > 3 && parts[0] == "arn" && parts[3] != "" {
		return parts[3], true
	}
	return "", false
}

// region gives back the snapshot's aws region, or regionPlaceholder, for a command shown to the user
func region(s *snapshotv1alpha1.Snapshot) string {
	// invoking regionOf and checking if the snapshot shows the region, its defined above
	if r, ok := regionOf(s); ok {
		return r
	}
	return regionPlaceholder
}

// readme builds external-secrets/README.md, how to set up the store and the remote secrets, then apply the directory
func (x *ExternalSecrets) readme(s *snapshotv1alpha1.Snapshot, remotes []remoteSecret, storeFile string) string {
	where, storeNS := "", ""
	// checking if the store is a SecretStore, its namespaced so each namespace with an ExternalSecret needs its own
	if x.StoreKind == "SecretStore" {
		var namespaces []string
		// collecting the namespaces of the Secrets being replaced
		for _, r := range remotes {
			ns, _, _ := strings.Cut(r.secret, "/")
			// checking if this namespace isnt listed yet
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
	// checking where the store comes from, the file emit wrote next to the platform's, an inline example for the aws service, or the examples below
	// invoking region (defined above), indent and awsStore (both defined below) for the inline example
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
	// adding a table row for each remote secret
	for _, r := range remotes {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.key, "`"+strings.Join(r.properties, "`, `")+"`", r.secret)
	}
	// checking if theres no aws service or the store was written to a file, if so invoking storeExamples to add the examples for other secret managers, its defined below
	if x.Service == "" || storeFile != "" {
		b.WriteString("\n" + storeExamples(x.StoreKind, x.StoreName, storeNS))
	}
	return b.String()
}

// awsStore builds the store for aws secrets manager or parameter store, as yaml
// namespace is the SecretStore's namespace, "" for a ClusterSecretStore
func (x *ExternalSecrets) awsStore(region, namespace string) string {
	var spec storeSpec
	spec.Spec.Provider.AWS = awsProvider{Service: x.Service, Region: region}
	// invoking storeHead and marshalYAML for the head and the spec, both defined below
	return storeHead(x.StoreKind, x.StoreName, namespace) + marshalYAML(spec)
}

// storeMeta is a store's apiVersion, kind and metadata, in the order yaml writes them
type storeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace,omitempty"`
	} `yaml:"metadata"`
}

// storeSpec is an aws store's spec, in the shape yaml writes it
type storeSpec struct {
	Spec struct {
		Provider struct {
			AWS awsProvider `yaml:"aws"`
		} `yaml:"provider"`
	} `yaml:"spec"`
}

// awsProvider is a store's spec.provider.aws
type awsProvider struct {
	Service string `yaml:"service"`
	Region  string `yaml:"region"`
}

// storeHead builds a store's apiVersion, kind and metadata, as yaml
func storeHead(kind, name, namespace string) string {
	m := storeMeta{APIVersion: "external-secrets.io/v1", Kind: kind}
	m.Metadata.Name, m.Metadata.Namespace = name, namespace
	// invoking marshalYAML to write it, its defined below
	return marshalYAML(m)
}

// marshalYAML writes v as yaml in its fields' order, two-space indented, so any value is quoted as yaml needs
func marshalYAML(v any) string {
	var b bytes.Buffer
	enc := yamlv3.NewEncoder(&b)
	enc.SetIndent(2)
	// checking if v failed to encode
	if err := enc.Encode(v); err != nil {
		// v is one of this file's structs of strings so it always encodes, a failure is a bug
		panic(fmt.Sprintf("kustomize: marshalYAML: %v", err))
	}
	return b.String()
}

// storeNameRE matches a kubernetes object name, a dns subdomain
var storeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// CheckStore checks an external secrets store's kind and name, the kind must be ClusterSecretStore or SecretStore and the name a kubernetes name
func CheckStore(kind, name string) error {
	// checking if the kind is one of the two store kinds
	if kind != "ClusterSecretStore" && kind != "SecretStore" {
		return fmt.Errorf("store kind %q: use ClusterSecretStore or SecretStore", kind)
	}
	// checking if the name is a valid kubernetes name, at most 253 characters
	if !storeNameRE.MatchString(name) || len(name) > 253 {
		return fmt.Errorf("store name %q: use a Kubernetes name: lowercase letters, digits, dashes and dots", name)
	}
	return nil
}

// storeExamples builds the README section with the store for the common secret managers, and the remote key format each accepts
func storeExamples(kind, name, namespace string) string {
	// invoking storeHead for the head every example shares, its defined above
	head := storeHead(kind, name, namespace) + "spec:\n  provider:\n"
	// ex builds one example, its title, a note on remote keys and the provider's yaml
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

// indent puts prefix in front of every non-empty line of s, to nest a code block in a numbered step of the README
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	// going through the lines to indent each one
	for i, l := range lines {
		// checking if the line has text, empty lines stay empty
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
