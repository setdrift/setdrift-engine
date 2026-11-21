// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package kustomize writes a snapshot as a Kustomize repository:
//	clusters/<cluster>/kustomization.yaml
//	clusters/<cluster>/README.md
//	clusters/<cluster>/RESTORE.md
//	clusters/<cluster>/.setdrift-export (the source cluster, for setdrift verify; see ExportInfo)
//	clusters/<cluster>/restore/volumes/<ns>/<claim>.yaml
//	clusters/<cluster>/crds/kustomization.yaml
//	clusters/<cluster>/crds/<name>.yaml
//	clusters/<cluster>/cluster-scoped/kustomization.yaml
//	clusters/<cluster>/cluster-scoped/<kind>[.<group>]/<name>.yaml
//	clusters/<cluster>/namespaces/<ns>/kustomization.yaml
//	clusters/<cluster>/namespaces/<ns>/<kind>[.<group>]/<name>.yaml
//	clusters/<cluster>/redacted/README.md
//	clusters/<cluster>/redacted/namespaces/<ns>/<kind>[.<group>]/<name>.yaml.redacted
//	clusters/<cluster>/external-secrets/README.md
//	clusters/<cluster>/external-secrets/kustomization.yaml
//	clusters/<cluster>/external-secrets/namespaces/<ns>/externalsecret.external-secrets.io/<name>.yaml
//
// Resources whose values were redacted (see ReferenceOnly) go under redacted/ and are not referenced and should not be applied, see RESTORE.md for instructions

package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

const (
	clusterScopedDir = "cluster-scoped"
	crdsDir          = "crds"
	redactedDir      = "redacted"
	// RedactedExt is not .yaml, .yml or .json, so tools that apply every manifest under a directory skip these files too.
	RedactedExt = ".yaml.redacted"
)

// redactedReadme is written to redacted/README.md when any resource is reference-only.
const redactedReadme = `# Redacted resources

These resources are exported for reference only. Secret values in them were
replaced with SETDRIFT_REDACTED before they left the cluster: Secrets, and
other resources holding what looks like a secret (a password in an env var or
a config file; see their SECRET_IN_MANIFEST finding in the snapshot). So they
are not listed in any kustomization.yaml, and use the .yaml.redacted
extension, which kubectl, Argo CD and Flux ignore when reading a directory.

Do not apply them: applying a resource from this directory would overwrite
the real values in the cluster with the placeholder.

%s
- Other resources: move the value into a Secret, reference it (env
  valueFrom.secretKeyRef, or a Secret volume), and export again.
`

// redactedReadme's Secrets line, with and without ExternalSecrets
const (
	readmeWithExternalSecrets = `- Secrets: ../external-secrets/ has an ExternalSecret for each, which reads
  the values from a secret manager instead; its README says how to set it up.`
	readmeWithoutExternalSecrets = `- Secrets: keep the values out of Git with the secret tooling you use
  (External Secrets Operator, Sealed Secrets, the Secrets Store CSI driver),
  using these files to see which Secrets and keys you need. Export again with
  --external-secrets to have an ExternalSecret written for each.`
)

// rootReadme is written to README.md at the cluster root.
const rootReadme = `# %s

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

%s`

const applyWithCRDs = `Argo CD and Flux apply CustomResourceDefinitions before the resources that use
them, so point them at this directory. With kubectl, apply the CRDs first:
kubectl rejects a custom resource whose definition does not exist yet, even
when the definition is in the same apply.

    kubectl apply --server-side -k crds/
    kubectl wait --for condition=established --timeout=60s -k crds/
    kubectl apply --server-side -k .
`

const applyWithoutCRDs = `Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .
`

// Emitter writes a snapshot as a Kustomize repository
type Emitter struct {
	// allows writing into a non-empty directory
	Force bool
	// UnmanagedOnly writes only resources nothing manages, the ones to adopt into Git
	UnmanagedOnly bool
	// ExternalSecrets, if set, writes an ExternalSecret for every redacted Secret.
	ExternalSecrets *ExternalSecrets
	// HelmRepos, if set, names the chart repository of Helm releases for RESTORE.md (export --helm-repos).
	HelmRepos *helmsource.Repos
	// SourceContext is the kubeconfig context the snapshot was read with, for the commands in RESTORE.md. Empty writes a placeholder.
	SourceContext string
	// Region is the AWS region for RESTORE.md when the snapshot does not
	// show it (no --aws, and a context that is not the cluster ARN).
	Region string
}

// Unmanaged reports whether no GitOps tool or Helm manages r, counting a low-confidence or missing manager as none: the resources to adopt into Git.
func Unmanaged(r *snapshotv1alpha1.Resource) bool {
	p := r.GetProvenance()
	switch p.GetSource() {
	case snapshotv1alpha1.Source_SOURCE_UNMANAGED, snapshotv1alpha1.Source_SOURCE_UNSPECIFIED:
		return true
	case snapshotv1alpha1.Source_SOURCE_OPERATOR, snapshotv1alpha1.Source_SOURCE_PLATFORM, snapshotv1alpha1.Source_SOURCE_CI_CD:
		// a CI/CD pipeline deploys it from Git, even when only its field manager says so
		return false
	}
	if p.GetEvidence()["confidence"] == "low" {
		return true
	}
	for _, f := range r.GetFindings() {
		if f.GetCode() == "MANAGER_NOT_FOUND" {
			return true
		}
	}
	return false
}

// kustomization is a kustomization.yaml that lists its resources.
type kustomization struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Resources  []string `json:"resources"`
}

// ReferenceOnly reports whether r had values taken out, dont apply because it would overwrite legitimate values or settings
func ReferenceOnly(r *snapshotv1alpha1.Resource) bool {
	for _, red := range r.GetRedactions() {
		switch red.GetReason() {
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
			snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE:
			return true
		}
	}
	return false
}

// NeedsValues reports whether someone must supply values for r: it has redacted values a person or tool wrote, not only generated ones.
func NeedsValues(r *snapshotv1alpha1.Resource) bool {
	for _, red := range r.GetRedactions() {
		if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
			return true
		}
	}
	return false
}

// Generated returns r's values a controller wrote, which the export leaves out because the controller writes them again in a new cluster.
func Generated(r *snapshotv1alpha1.Resource) []*snapshotv1alpha1.Redaction {
	var out []*snapshotv1alpha1.Redaction
	for _, red := range r.GetRedactions() {
		if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE {
			out = append(out, red)
		}
	}
	return out
}

// Emit writes s under dir, as ClusterDir(dir, cluster). Everything is written to a temporary directory first in case it fails before completing
func (e Emitter) Emit(s *snapshotv1alpha1.Snapshot, dir string) error {
	root := ClusterDir(dir, s.GetCluster().GetName())
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// With Force, the cluster's directory is replaced: files of resource no longer in the cluster would otherwise stay. The rest of dir is kept.
	if len(entries) > 0 && !e.Force {
		return fmt.Errorf("output directory %s is not empty (use --force to replace %s)", dir, root)
	}
	parent, name := filepath.Dir(root), filepath.Base(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	// Left by an export that was killed before it could clean up.
	stale, _ := filepath.Glob(filepath.Join(parent, "."+name+".tmp-*"))
	for _, d := range stale {
		_ = os.RemoveAll(d)
	}
	tmp, err := os.MkdirTemp(parent, "."+name+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // nothing left to remove once it has replaced root
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := e.write(s, tmp, name); err != nil {
		return err
	}
	return replaceDir(tmp, root)
}

// replaceDir moves the finished export tmp to root, putting the previous export back if that fails.
func replaceDir(tmp, root string) error {
	old := ""
	if _, err := os.Lstat(root); err == nil {
		old = tmp + "-previous"
		if err := os.Rename(root, old); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, root); err != nil {
		if old != "" {
			_ = os.Rename(old, root)
		}
		return err
	}
	if old != "" {
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("exported, but removing the previous export %s failed: %w", old, err)
		}
	}
	return nil
}

// write writes the export into root; name is the cluster directory's final name, for README.md.
func (e Emitter) write(s *snapshotv1alpha1.Snapshot, root, name string) error {
	var resources []*snapshotv1alpha1.Resource
	for _, r := range s.Resources {
		if NotWritten(r) || (e.UnmanagedOnly && !Unmanaged(r)) {
			continue
		}
		resources = append(resources, r)
	}
	// The platform set's own objects: custom resources of APIs the platform
	// provides (their CRD is the platform's, left out), which no other
	// cluster serves. Everything else is generic.
	cls, platDir := exportPlatform(s)
	platformKinds := platformCRDKinds(s)
	var generic, bound []*snapshotv1alpha1.Resource
	for _, r := range resources {
		if platDir != "" && platformKinds[resourceGroupKind(r)] && !ReferenceOnly(r) {
			bound = append(bound, r)
		} else {
			generic = append(generic, r)
		}
	}
	resources = append(generic, bound...)
	paths, err := filePaths(generic, "")
	if err != nil {
		return err
	}
	boundPaths, err := filePaths(bound, platDir)
	if err != nil {
		return err
	}
	paths = append(paths, boundPaths...)

	buckets := map[string][]string{} // bucket dir > files relative to it
	referenceOnly := false
	written := make([]writtenFile, len(resources))
	for i, r := range resources {
		bucket, rel := paths[i].bucket, paths[i].rel
		out, err := writeManifest(filepath.Join(root, bucket, rel), r)
		if err != nil {
			return err
		}
		written[i] = writtenFile{resource: r, path: filepath.ToSlash(filepath.Join(bucket, rel)), yaml: out, referenceOnly: paths[i].referenceOnly}
		if paths[i].referenceOnly {
			referenceOnly = true
			continue
		}
		buckets[bucket] = append(buckets[bucket], filepath.ToSlash(rel))
	}

	var remotes []remoteSecret
	if referenceOnly {
		esText := readmeWithoutExternalSecrets
		if e.ExternalSecrets != nil {
			esText = readmeWithExternalSecrets
		}
		if err := writeFile(filepath.Join(root, redactedDir, "README.md"), []byte(fmt.Sprintf(redactedReadme, esText))); err != nil {
			return err
		}
		if e.ExternalSecrets != nil {
			if remotes, err = e.ExternalSecrets.resolved(cls).emit(s, resources, root, platDir); err != nil {
				return err
			}
		}
	}
	if err := e.writeRestore(root, s, written, remotes, len(bound)); err != nil {
		return err
	}
	secretManager := ""
	if e.ExternalSecrets != nil && referenceOnly {
		secretManager = e.ExternalSecrets.resolved(cls).ID
	}
	if err := writeExportInfo(root, s, secretManager); err != nil {
		return err
	}
	apply := applyWithoutCRDs
	if _, ok := buckets[crdsDir]; ok {
		apply = applyWithCRDs
	}
	readme := fmt.Sprintf(rootReadme, name, apply)
	// What the export found, before how to apply it.
	if ov := NewOverview(s, e.UnmanagedOnly).markdown(); ov != "" {
		readme = strings.Replace(readme, "\n## Applying to a new cluster", ov+"## Applying to a new cluster", 1)
	}
	if platDir != "" {
		readme += fmt.Sprintf("\n## The %s set\n\n"+
			"This directory is generic: it applies to any Kubernetes cluster. The source\n"+
			"ran on %s. For a cluster on the same platform,\n"+
			"%s/ is the same export plus what only that platform serves (see\n"+
			"its README), and\n"+
			"RESTORE.md says which of its steps are for that platform.\n", platDir, cls.Summary(), platDir)
	}
	if err := writeFile(filepath.Join(root, "README.md"), []byte(readme)); err != nil {
		return err
	}

	var bucketNames, platformBuckets []string
	for b, files := range buckets {
		if err := writeBucket(filepath.Join(root, b), files); err != nil {
			return err
		}
		if platDir != "" && strings.HasPrefix(filepath.ToSlash(b), platDir+"/") {
			platformBuckets = append(platformBuckets, strings.TrimPrefix(filepath.ToSlash(b), platDir+"/"))
			continue
		}
		bucketNames = append(bucketNames, filepath.ToSlash(b))
	}

	// CRDs first, then cluster-scoped objects (Namespaces), then namespaces.
	rank := func(b string) int {
		switch b {
		case crdsDir:
			return 0
		case clusterScopedDir:
			return 1
		}
		return 2
	}
	sort.Slice(bucketNames, func(i, j int) bool {
		if ri, rj := rank(bucketNames[i]), rank(bucketNames[j]); ri != rj {
			return ri < rj
		}
		return bucketNames[i] < bucketNames[j]
	})
	if platDir != "" {
		if err := writePlatformSet(root, platDir, cls, bucketNames, platformBuckets, len(bound)); err != nil {
			return err
		}
	}
	return writeKustomization(root, bucketNames)
}

// writeBucket writes the kustomization.yaml of one bucket, listing its files.
func writeBucket(dir string, files []string) error {
	sort.Strings(files)
	return writeKustomization(dir, files)
}

// writeManifest writes r as YAML and returns what it wrote.
func writeManifest(path string, r *snapshotv1alpha1.Resource) ([]byte, error) {
	out, err := yaml.JSONToYAML([]byte(r.ManifestJson))
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s: %w", r.Kind, r.Namespace, r.Name, err)
	}
	return out, writeFile(path, out)
}

// writtenFile is a resource as Emit wrote it.
type writtenFile struct {
	resource *snapshotv1alpha1.Resource
	// path relative to the cluster directory, with forward slashes
	path          string
	yaml          []byte
	referenceOnly bool
}

// PlatformName is the display name of a platform manager in a resource's
// provenance (eks, kubeadm, kind), e.g. "EKS".
func PlatformName(manager string) string {
	if n := platform.Distribution(manager).Name(); n != "" {
		return n
	}
	if manager == "" {
		return "the platform"
	}
	return manager
}

// RanOnce reports whether r is a finished Job (lifecycle.RanOnce): the snapshot keeps it as a record of what ran, the
// export leaves it out, as applying it would run it again, and RESTORE.md lists it.
func RanOnce(r *snapshotv1alpha1.Resource) bool {
	for _, f := range r.GetFindings() {
		if f.GetCode() == lifecycle.RanOnce {
			return true
		}
	}
	return false
}

// NotWritten reports whether the export leaves r out though the snapshot has it: the platform's own objects, and
// finished Jobs.
func NotWritten(r *snapshotv1alpha1.Resource) bool { return Platform(r) || RanOnce(r) }

// Platform reports whether the platform manages r (e.g. EKS), so a new cluster gets its own copy and the export leaves it out.
func Platform(r *snapshotv1alpha1.Resource) bool {
	return r.GetProvenance().GetSource() == snapshotv1alpha1.Source_SOURCE_PLATFORM
}

// filePath is where a resource is written: rel within bucket.
type filePath struct {
	bucket, rel   string
	referenceOnly bool
}

// filePaths gives every resource its own file, unique even on case-insensitive file systems
func filePaths(resources []*snapshotv1alpha1.Resource, prefix string) ([]filePath, error) {
	paths := make([]filePath, len(resources))
	used := map[string]bool{} // lower-cased paths already assigned
	assign := func(i int) error {
		r := resources[i]
		bucket := clusterScopedDir
		switch {
		case r.Namespace != "":
			bucket = filepath.Join("namespaces", SafeName(r.Namespace))
		case isCRD(r):
			bucket = crdsDir
		}
		ext, refOnly := ".yaml", ReferenceOnly(r)
		if refOnly {
			bucket, ext = filepath.Join(redactedDir, bucket), RedactedExt
		}
		bucket = filepath.Join(prefix, bucket)
		dir := KindDir(r.ApiVersion, r.Kind)
		if bucket == crdsDir {
			// One kind, so no kind directory.
			dir = ""
		}
		ref := fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name)
		for _, name := range []string{SafeName(r.Name), SafeName(r.Name) + "-" + shortHash(r.Name)} {
			rel := filepath.Join(dir, name+ext)
			key := strings.ToLower(filepath.Join(bucket, rel))
			if !used[key] {
				used[key] = true
				paths[i] = filePath{bucket, rel, refOnly}
				return nil
			}
		}
		// Only reachable with a hash collision or duplicate resources, which snapshot.Build removes.
		return fmt.Errorf("%s: no unique file name in %s", ref, filepath.Join(bucket, dir))
	}
	for _, plain := range []bool{true, false} {
		for i, r := range resources {
			if (SafeName(r.Name) == r.Name) != plain {
				continue
			}
			if err := assign(i); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

// ClusterDir is the directory Emit writes a cluster's repository to, <dir>/clusters/<cluster>, and the one it replaces with Force.
func ClusterDir(dir, cluster string) string {
	name := SafeName(cluster)
	switch name {
	case "":
		name = "cluster"
	case ".", "..":
		// Would name dir itself, or its parent.
		name = strings.Repeat("_", len(name))
	}
	return filepath.Join(dir, "clusters", name)
}

// isCRD reports whether r is a CustomResourceDefinition
func isCRD(r *snapshotv1alpha1.Resource) bool {
	g, _, _ := strings.Cut(r.ApiVersion, "/")
	return g == "apiextensions.k8s.io" && r.Kind == "CustomResourceDefinition"
}

// shortHash is 8 hex digits of s's SHA-256, to tell apart names SafeName made equal
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// KindDir returns the directory for a kind, e.g. configmap or deployment.apps
func KindDir(apiVersion, kind string) string {
	k := strings.ToLower(kind)
	if g, _, ok := strings.Cut(apiVersion, "/"); ok {
		return k + "." + g
	}
	return k
}

// SafeName makes a name safe for file paths on every OS. Kubernetes names may contain ':' (e.g. rbac), which Windows forbids.
func SafeName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}

// writeKustomization writes dir/kustomization.yaml listing resources
func writeKustomization(dir string, resources []string) error {
	if resources == nil {
		// "resources: null" makes Kustomize fail with "kustomization.yaml is empty"
		resources = []string{}
	}
	out, err := yaml.Marshal(kustomization{
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		Kind:       "Kustomization",
		Resources:  resources,
	})
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "kustomization.yaml"), out)
}

// writeFile writes path, creating its directories
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
