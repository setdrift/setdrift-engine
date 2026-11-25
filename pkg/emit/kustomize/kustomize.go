// Package kustomize writes a snapshot as a kustomize repository, laid out like this
//
//	clusters/<cluster>/kustomization.yaml
//	clusters/<cluster>/README.md
//	clusters/<cluster>/RESTORE.md
//	clusters/<cluster>/.setdrift-export (the source cluster, for setdrift verify, see ExportInfo)
//	clusters/<cluster>/cluster_profile/README.md (the cluster as infrastructure, see ProfileDir)
//	clusters/<cluster>/cluster_profile/profile.json
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
//	clusters/<cluster>/cloud-resources/README.md
//	clusters/<cluster>/cloud-resources/namespaces/<ns>/<kind>.<group>/<name>.yaml
//
// resources whose values were redacted (see ReferenceOnly) go under redacted/, no kustomization lists them and they must not be applied
// RESTORE.md says how to supply their values
// objects of operators that create cloud resources, like an ACK Bucket, go under cloud-resources/, no kustomization lists them either, RESTORE.md says how to move each one
package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/provenance"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// the directory names inside a cluster's directory, and the extension of redacted files
const (
	clusterScopedDir = "cluster-scoped"
	crdsDir          = "crds"
	redactedDir      = "redacted"
	// RedactedExt is not .yaml, .yml or .json, so tools that apply every manifest under a directory skip these files too
	RedactedExt = ".yaml.redacted"
)

// redactedReadme is written to redacted/README.md when any resource is reference-only
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

// the Secrets line of redactedReadme, one with ExternalSecrets and one without
//
// #nosec G101 -- README text, not credentials
const (
	readmeWithExternalSecrets = `- Secrets: ../external-secrets/ has an ExternalSecret for each, which reads
  the values from a secret manager instead; its README says how to set it up.`
	readmeWithoutExternalSecrets = `- Secrets: keep the values out of Git with the secret tooling you use
  (External Secrets Operator, Sealed Secrets, the Secrets Store CSI driver),
  using these files to see which Secrets and keys you need. Export again with
  --external-secrets to have an ExternalSecret written for each.`
)

// rootReadme and rootReadmeApply make README.md at the cluster root, the overview of what the export found goes between them
const rootReadme = `# %s

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.
`

// rootReadmeApply is the apply section of README.md, the apply steps go in its %s
const rootReadmeApply = `## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

%s`

// applyWithCRDs are the apply steps when the export has crds, kubectl needs them applied and established first
const applyWithCRDs = `Argo CD and Flux apply CustomResourceDefinitions before the resources that use
them, so point them at this directory. With kubectl, apply the CRDs first:
kubectl rejects a custom resource whose definition does not exist yet, even
when the definition is in the same apply.

    kubectl apply --server-side -k crds/
    kubectl wait --for condition=established --timeout=60s -k crds/
    kubectl apply --server-side -k .
`

// applyWithoutCRDs are the apply steps when the export has no crds
const applyWithoutCRDs = `Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .
`

// Emitter holds the export settings and writes a snapshot as a kustomize repository
type Emitter struct {
	// allows writing into a non-empty directory
	Force bool
	// UnmanagedOnly writes only resources nothing manages, the ones to adopt into git
	UnmanagedOnly bool
	// ExternalSecrets, if set, writes an ExternalSecret for every redacted Secret
	ExternalSecrets *ExternalSecrets
	// HelmRepos, if set, names the chart repository of helm releases for RESTORE.md (export --helm-repos)
	HelmRepos *helmsource.Repos
	// SourceContext is the kubeconfig context the snapshot was read with, for the commands in RESTORE.md, empty writes a placeholder
	SourceContext string
	// Region is the aws region for RESTORE.md when the snapshot doesnt show it (no --aws, and a context that isnt the cluster arn)
	Region string
}

// Unmanaged tells if no gitops tool or helm manages r, counting a low-confidence or missing manager as none
// those are the resources to adopt into git
func Unmanaged(r *snapshotv1alpha1.Resource) bool {
	p := r.GetProvenance()
	// checking who the provenance says manages r, unmanaged and unknown count as none, an operator, the platform or ci/cd count as managed
	switch p.GetSource() {
	case snapshotv1alpha1.Source_SOURCE_UNMANAGED, snapshotv1alpha1.Source_SOURCE_UNSPECIFIED:
		return true
	case snapshotv1alpha1.Source_SOURCE_OPERATOR, snapshotv1alpha1.Source_SOURCE_PLATFORM, snapshotv1alpha1.Source_SOURCE_CI_CD:
		// a ci/cd pipeline deploys it from git, even when only its field manager says so
		return false
	}
	// checking if the manager was only guessed with low confidence, if so it counts as none
	// only snapshots from before v0.2.0 have such guesses, now an object with only a chart's labels is unmanaged
	if p.GetEvidence()["confidence"] == "low" {
		return true
	}
	// going through the findings to see if the manager its markers name wasnt found
	for _, f := range r.GetFindings() {
		// checking if this is the provenance.ManagerNotFound finding, if so the manager counts as none
		if f.GetCode() == provenance.ManagerNotFound {
			return true
		}
	}
	return false
}

// kustomization is a kustomization.yaml that lists its resources
type kustomization struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Resources  []string `json:"resources"`
}

// ReferenceOnly tells if r had values taken out, then it must not be applied because it would overwrite the real values or settings
func ReferenceOnly(r *snapshotv1alpha1.Resource) bool {
	// going through r's redactions to look for a secret or generated value
	for _, red := range r.GetRedactions() {
		// checking why the value was taken out, a secret or a generated value makes r reference-only
		switch red.GetReason() {
		case snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
			snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE:
			return true
		}
	}
	return false
}

// NeedsValues tells if someone must supply values for r, it has redacted values a person or tool wrote, not only generated ones
func NeedsValues(r *snapshotv1alpha1.Resource) bool {
	// going through r's redactions to look for a secret value
	for _, red := range r.GetRedactions() {
		// checking if this one was taken out as a secret value
		if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
			return true
		}
	}
	return false
}

// Generated gives back r's values a controller wrote, the export leaves them out because the controller writes them again in a new cluster
func Generated(r *snapshotv1alpha1.Resource) []*snapshotv1alpha1.Redaction {
	var out []*snapshotv1alpha1.Redaction
	// going through r's redactions to keep the generated ones
	for _, red := range r.GetRedactions() {
		// checking if this one was taken out as a generated value
		if red.GetReason() == snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE {
			out = append(out, red)
		}
	}
	return out
}

// Emit writes s under dir, as ClusterDir(dir, cluster)
// everything is written to a temporary directory first, so an export that fails before completing leaves the previous one as it was
func (e Emitter) Emit(s *snapshotv1alpha1.Snapshot, dir string) error {
	// invoking ClusterDir to pick the cluster's directory under dir, its defined below
	root := ClusterDir(dir, s.GetCluster().GetName())
	// reading dir to see if it already holds anything
	entries, err := os.ReadDir(dir)
	// checking if dir couldnt be read for a reason other than not existing yet, thats a terminal error
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// checking if dir already holds something and Force isnt set, if so nothing gets written
	// with Force the cluster's directory is replaced, files of resources no longer in the cluster would otherwise stay, the rest of dir is kept
	if len(entries) > 0 && !e.Force {
		return fmt.Errorf("output directory %s is not empty (use --force to replace %s)", dir, root)
	}
	parent, name := filepath.Dir(root), filepath.Base(root)
	// creating the parent directory the temporary directory goes in
	if err := os.MkdirAll(parent, 0o755); err != nil { //#nosec G301 -- an export is a Git repository, with no secret values
		return err
	}
	// looking for temporary directories left by an export that was killed before it could clean up
	// the pattern is fixed, so Glob cant fail
	stale, _ := filepath.Glob(filepath.Join(parent, "."+name+".tmp-*"))
	// removing each one if possible, one left behind changes nothing
	for _, d := range stale {
		_ = os.RemoveAll(d)
	}
	// creating the temporary directory the export is written to, next to root
	tmp, err := os.MkdirTemp(parent, "."+name+".tmp-")
	if err != nil {
		return err
	}
	// removing the temporary directory on the way out, theres nothing left to remove once it has replaced root
	defer os.RemoveAll(tmp)
	// making the temporary directory readable like any checkout, MkdirTemp creates it private
	if err := os.Chmod(tmp, 0o755); err != nil { //#nosec G302 -- an export directory, readable like any checkout
		return err
	}
	// invoking openOutDir to open the temporary directory so no write can leave it, its defined below
	out, err := openOutDir(tmp)
	if err != nil {
		return err
	}
	// invoking write to write the export into the temporary directory, its defined below
	err = e.write(s, out, name)
	// invoking close to release the directory, its defined below
	// checking if writing went fine, if so a close error is the one to give back
	if cerr := out.close(); err == nil {
		err = cerr
	}
	// checking if writing or closing failed, if so root is left as it was
	if err != nil {
		return err
	}
	// invoking replaceDir to put the finished export in place of the previous one, its defined below
	return replaceDir(tmp, root)
}

// replaceDir moves the finished export tmp to root, putting the previous export back if that fails
func replaceDir(tmp, root string) error {
	old := ""
	// checking if theres a previous export at root, if so its moved aside to tmp-previous first
	// any error other than root not existing is a terminal error
	if _, err := os.Lstat(root); err == nil {
		old = tmp + "-previous"
		// moving the previous export aside
		if err := os.Rename(root, old); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// moving the finished export into place, checking if that failed
	if err := os.Rename(tmp, root); err != nil {
		// checking if a previous export was moved aside, if so its put back
		if old != "" {
			// best effort, the error that matters is err
			_ = os.Rename(old, root)
		}
		return err
	}
	// checking if a previous export was moved aside, if so its removed now that the new one is in place
	if old != "" {
		// checking if removing it failed, if so the export is done but the error names what was left behind
		if err := os.RemoveAll(old); err != nil {
			return fmt.Errorf("exported, but removing the previous export %s failed: %w", old, err)
		}
	}
	return nil
}

// write writes the export into out, name is the cluster directory's final name, for README.md
func (e Emitter) write(s *snapshotv1alpha1.Snapshot, out *outDir, name string) error {
	// invoking exportPlatform to find the source's platform and its platform set directory, its defined in platformset.go
	cls, platDir := exportPlatform(s)
	// invoking capability.Load for the cloud-resource operators, whose objects are held back, its defined in pkg/capability/capability.go
	m, err := capability.Load()
	if err != nil {
		return err
	}
	// invoking partition to pick the resources to write, with the platform set's own ones at the end, and the ones held back, its defined below
	resources, bound, held := e.partition(s, platDir, m)
	// invoking filePaths to give each generic resource its file, its defined below
	paths, err := filePaths(resources[:len(resources)-bound], "")
	if err != nil {
		return err
	}
	// invoking filePaths again for the platform set's resources, under the platform set's directory
	boundPaths, err := filePaths(resources[len(resources)-bound:], platDir)
	if err != nil {
		return err
	}
	// invoking writeManifests to write every resource and collect what each kustomization lists, its defined below
	written, buckets, err := writeManifests(out, resources, append(paths, boundPaths...))
	if err != nil {
		return err
	}
	// checking if any written file is reference-only, if so redacted/ needs its README
	referenceOnly := slices.ContainsFunc(written, func(w writtenFile) bool { return w.referenceOnly })
	var settings *ExternalSecrets
	// checking if ExternalSecrets were asked for, if so invoking resolved to settle auto for the cluster's platform, its defined in externalsecrets.go
	if e.ExternalSecrets != nil {
		settings = e.ExternalSecrets.resolved(cls)
	}

	var remotes []remoteSecret
	// checking if anything was redacted, if so redacted/ gets its README and the ExternalSecrets get written
	if referenceOnly {
		esText := readmeWithoutExternalSecrets
		// checking if ExternalSecrets are written, if so the README points at them
		if settings != nil {
			esText = readmeWithExternalSecrets
		}
		// invoking out.write to write redacted/README.md with the matching Secrets line, its defined below
		if err := out.write(filepath.Join(redactedDir, "README.md"), []byte(fmt.Sprintf(redactedReadme, esText))); err != nil {
			return err
		}
		// checking if ExternalSecrets were asked for, if so invoking emit to write external-secrets/, its defined in externalsecrets.go
		if settings != nil {
			// invoking emit to write external-secrets/ and get back the remote secrets RESTORE.md lists, its defined in externalsecrets.go
			if remotes, err = settings.emit(s, resources, out, platDir); err != nil {
				return err
			}
		}
	}
	// invoking writeHeld to write cloud-resources/, its defined below
	heldFiles, err := writeHeld(out, held, m)
	if err != nil {
		return err
	}
	// invoking writeRestore to write RESTORE.md and restore/volumes/, its defined in restore.go
	if err := e.writeRestore(out, s, written, remotes, bound, heldFiles); err != nil {
		return err
	}
	// invoking writeExportInfo to record the source cluster for setdrift verify, its defined in exportinfo.go
	if err := writeExportInfo(out, s); err != nil {
		return err
	}
	// invoking writeProfile to write cluster_profile/, the cluster described as infrastructure, its defined in profile.go
	if err := writeProfile(out, s); err != nil {
		return err
	}
	// checking if any crds were written, if so the README says to apply them first
	_, hasCRDs := buckets[crdsDir]
	// invoking readme to build README.md and out.write to write it, both defined below
	if err := out.write("README.md", []byte(e.readme(s, name, hasCRDs, cls, platDir, len(held) > 0))); err != nil {
		return err
	}
	// invoking writeBuckets to write the kustomizations and the platform set, its defined below
	return writeBuckets(out, buckets, cls, platDir, bound)
}

// partition gives back the resources to write, the generic ones first, and how many at the end are the platform set's own objects
// those are custom resources of apis the platform provides (their crd is the platform's and left out), which no other cluster serves
// held are the objects of operators that create cloud resources, written to cloud-resources/ and never applied with the rest
func (e Emitter) partition(s *snapshotv1alpha1.Snapshot, platDir string, m *capability.Map) ([]*snapshotv1alpha1.Resource, int, []*snapshotv1alpha1.Resource) {
	// invoking platformCRDKinds to find the kinds only the platform serves, its defined in platformset.go
	platformKinds := platformCRDKinds(s)
	var generic, bound, held []*snapshotv1alpha1.Resource
	// going through every resource to sort it into the generic or the platform set
	for _, r := range s.Resources {
		// invoking NotWritten (defined below) and Unmanaged (defined above) to skip what the export leaves out, and with UnmanagedOnly what something manages
		if NotWritten(r) || (e.UnmanagedOnly && !Unmanaged(r)) {
			continue
		}
		// checking if theres a platform set and r is a kind only the platform serves, if so it goes to the platform set
		// invoking resourceGroupKind for r's group and kind, its defined in platformset.go, and ReferenceOnly, its defined above, so reference-only ones stay generic
		// invoking resourceGroupKind and CloudResourceOperator to check if a cloud-resource operator serves r, if so its held back, reference-only ones stay in redacted/
		switch op, _ := m.CloudResourceOperator(resourceGroupKind(r).Group); {
		case op != nil && !ReferenceOnly(r):
			held = append(held, r)
		case platDir != "" && platformKinds[resourceGroupKind(r)] && !ReferenceOnly(r):
			bound = append(bound, r)
		default:
			generic = append(generic, r)
		}
	}
	return append(generic, bound...), len(bound), held
}

// writeHeld writes the held-back cloud-resource objects under cloud-resources/ with its README, in no kustomization, and gives back what it wrote with each one's operator
func writeHeld(out *outDir, held []*snapshotv1alpha1.Resource, m *capability.Map) ([]heldResource, error) {
	if len(held) == 0 {
		return nil, nil
	}
	// invoking filePaths to give each one its file under cloud-resources/, its defined below
	paths, err := filePaths(held, cloudResourcesDir)
	if err != nil {
		return nil, err
	}
	var files []heldResource
	for i, r := range held {
		rel := filepath.Join(paths[i].bucket, paths[i].rel)
		// invoking writeManifest to write r as yaml, its defined below
		b, err := out.writeManifest(rel, r)
		if err != nil {
			return nil, err
		}
		op, _ := m.CloudResourceOperator(resourceGroupKind(r).Group)
		files = append(files, heldResource{file: writtenFile{resource: r, path: filepath.ToSlash(rel), yaml: b}, op: op})
	}
	// invoking out.write to write the README that says why theyre held back
	return files, out.write(filepath.Join(cloudResourcesDir, "README.md"), []byte(cloudResourcesReadme))
}

// writeManifests writes each resource at its path, and gives back what it wrote and, by bucket directory, the files each kustomization lists
// reference-only files are in no kustomization
func writeManifests(out *outDir, resources []*snapshotv1alpha1.Resource, paths []filePath) ([]writtenFile, map[string][]string, error) {
	buckets := map[string][]string{}
	written := make([]writtenFile, len(resources))
	// going through the resources to write each one at the path filePaths gave it
	for i, r := range resources {
		bucket, rel := paths[i].bucket, paths[i].rel
		// invoking writeManifest to write r as yaml, its defined below
		b, err := out.writeManifest(filepath.Join(bucket, rel), r)
		if err != nil {
			return nil, nil, err
		}
		written[i] = writtenFile{resource: r, path: filepath.ToSlash(filepath.Join(bucket, rel)), yaml: b, referenceOnly: paths[i].referenceOnly}
		// checking if the file can be applied, if so its bucket's kustomization lists it
		if !paths[i].referenceOnly {
			buckets[bucket] = append(buckets[bucket], filepath.ToSlash(rel))
		}
	}
	return written, buckets, nil
}

// readme builds README.md at the cluster root, what the export holds, what it found, how to apply it, and the platform set
func (e Emitter) readme(s *snapshotv1alpha1.Snapshot, name string, hasCRDs bool, cls *platform.Classification, platDir string, held bool) string {
	apply := applyWithoutCRDs
	// checking if the export has crds, if so the apply steps put them first
	if hasCRDs {
		apply = applyWithCRDs
	}
	readme := fmt.Sprintf(rootReadme, name)
	// checking if cloud-resource objects were held back, if so the README says theyre not applied either
	if held {
		readme += cloudResourcesDir + "/ is not included either: its objects manage real cloud resources, and\nRESTORE.md says how to move each one.\n"
	}
	// invoking NewOverview and markdown for what the export found, both defined in overview.go, it goes before how to apply it
	// checking if the overview has anything to say, if not only a blank line goes in
	if ov := NewOverview(s, e.UnmanagedOnly).markdown(); ov != "" {
		readme += ov
	} else {
		readme += "\n"
	}
	readme += fmt.Sprintf(rootReadmeApply, apply)
	// checking if the snapshot has a profile, if so the README points at it
	if s.GetInventory().GetProfile() != nil {
		readme += "\n## Cluster profile\n\n" +
			ProfileDir + "/README.md describes the cluster as infrastructure: its cloud, networks\n" +
			"(pod, service and node ranges, VPC, subnets), DNS, storage and system\n" +
			"components, each with where it was found. Start there to recreate the\n" +
			"cluster on another platform or cloud.\n"
	}
	// checking if the export has a platform set, if so the README explains it
	// invoking cls.Summary to name the source's platform, its defined in pkg/platform/platform.go
	if platDir != "" {
		readme += fmt.Sprintf("\n## The %s set\n\n"+
			"This directory is generic: it applies to any Kubernetes cluster. The source\n"+
			"ran on %s. For a cluster on the same platform,\n"+
			"%s/ is the same export plus what only that platform serves (see\n"+
			"its README), and\n"+
			"RESTORE.md says which of its steps are for that platform.\n", platDir, cls.Summary(), platDir)
	}
	return readme
}

// writeBuckets writes each bucket's kustomization.yaml, the platform set, and the root kustomization.yaml listing the generic buckets
// the root lists crds first, then cluster-scoped objects (Namespaces), then namespaces
func writeBuckets(out *outDir, buckets map[string][]string, cls *platform.Classification, platDir string, bound int) error {
	var bucketNames, platformBuckets []string
	// going through the buckets to write each one's kustomization and sort it into generic or platform set
	for b, files := range buckets {
		// invoking writeBucket to write the bucket's kustomization.yaml, its defined below
		if err := out.writeBucket(b, files); err != nil {
			return err
		}
		// checking if the bucket is inside the platform set, if so the platform set lists it, relative to its own directory
		if platDir != "" && strings.HasPrefix(filepath.ToSlash(b), platDir+"/") {
			platformBuckets = append(platformBuckets, strings.TrimPrefix(filepath.ToSlash(b), platDir+"/"))
			continue
		}
		bucketNames = append(bucketNames, filepath.ToSlash(b))
	}
	// rank orders the buckets, crds first, then cluster-scoped, then the namespaces
	rank := func(b string) int {
		// checking which bucket b is
		switch b {
		case crdsDir:
			return 0
		case clusterScopedDir:
			return 1
		}
		return 2
	}
	// sorting the generic buckets by rank, then by name
	sort.Slice(bucketNames, func(i, j int) bool {
		// checking if the two have different ranks, if so the rank decides
		if ri, rj := rank(bucketNames[i]), rank(bucketNames[j]); ri != rj {
			return ri < rj
		}
		return bucketNames[i] < bucketNames[j]
	})
	// checking if the export has a platform set, if so invoking writePlatformSet to write it, its defined in platformset.go
	if platDir != "" {
		if err := writePlatformSet(out, platDir, cls, bucketNames, platformBuckets, bound); err != nil {
			return err
		}
	}
	// invoking writeKustomization to write the root kustomization.yaml, its defined below
	return out.writeKustomization(".", bucketNames)
}

// writeBucket writes the kustomization.yaml of one bucket, listing its files sorted
func (out *outDir) writeBucket(dir string, files []string) error {
	sort.Strings(files)
	// invoking writeKustomization to write it, its defined below
	return out.writeKustomization(dir, files)
}

// writeManifest writes r as yaml at path and gives back what it wrote
func (out *outDir) writeManifest(path string, r *snapshotv1alpha1.Resource) ([]byte, error) {
	// turning r's json manifest into yaml
	b, err := yaml.JSONToYAML([]byte(r.ManifestJson))
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s: %w", r.Kind, r.Namespace, r.Name, err)
	}
	// invoking out.write to write it, its defined below
	return b, out.write(path, b)
}

// writtenFile is a resource as Emit wrote it, for RESTORE.md
type writtenFile struct {
	resource *snapshotv1alpha1.Resource
	// path relative to the cluster directory, with forward slashes
	path string
	// the yaml that was written
	yaml []byte
	// whether the file went to redacted/ and is not applied
	referenceOnly bool
}

// PlatformName gives the display name of a platform manager in a resource's provenance (eks, kubeadm, kind), e.g. "EKS"
func PlatformName(manager string) string {
	// invoking Name on the manager's platform.Distribution, its defined in pkg/platform/platform.go
	// checking if it has a display name, if so thats the one
	if n := platform.Distribution(manager).Name(); n != "" {
		return n
	}
	// checking if theres no manager at all, if so its named generically
	if manager == "" {
		return "the platform"
	}
	return manager
}

// RanOnce tells if r is a finished Job (lifecycle.RanOnce), the snapshot keeps it as a record of what ran
// the export leaves it out since applying it would run it again, and RESTORE.md lists it
func RanOnce(r *snapshotv1alpha1.Resource) bool {
	// going through r's findings to look for the lifecycle.RanOnce one
	for _, f := range r.GetFindings() {
		// checking if this is that finding
		if f.GetCode() == lifecycle.RanOnce {
			return true
		}
	}
	return false
}

// NotWritten tells if the export leaves r out though the snapshot has it, thats the platform's own objects and finished Jobs
// invoking Platform (defined below) and RanOnce (defined above) to tell
func NotWritten(r *snapshotv1alpha1.Resource) bool { return Platform(r) || RanOnce(r) }

// Platform tells if the platform manages r (e.g. eks), then a new cluster gets its own copy and the export leaves it out
func Platform(r *snapshotv1alpha1.Resource) bool {
	return r.GetProvenance().GetSource() == snapshotv1alpha1.Source_SOURCE_PLATFORM
}

// filePath is where a resource is written, rel within bucket
type filePath struct {
	bucket, rel string
	// whether the resource goes to redacted/
	referenceOnly bool
}

// filePaths gives every resource its own file, unique even on case-insensitive file systems, prefix is put in front of every bucket
func filePaths(resources []*snapshotv1alpha1.Resource, prefix string) ([]filePath, error) {
	paths := make([]filePath, len(resources))
	// the lower-cased paths already assigned
	used := map[string]bool{}
	// assign picks the bucket and file of resources[i], with a hash added to the name when the safe name alone is taken
	assign := func(i int) error {
		r := resources[i]
		bucket := clusterScopedDir
		// checking where r goes, a namespaced resource under its namespace, a crd under crds, the rest stays cluster-scoped
		// invoking SafeName for the namespace's directory name and isCRD to spot a crd, both defined below
		switch {
		case r.Namespace != "":
			bucket = filepath.Join("namespaces", SafeName(r.Namespace))
		case isCRD(r):
			bucket = crdsDir
		}
		// invoking ReferenceOnly to tell if r goes to redacted/, its defined above
		ext, refOnly := ".yaml", ReferenceOnly(r)
		// checking if r is reference-only, if so it goes under redacted/ with the extension no tool applies
		if refOnly {
			bucket, ext = filepath.Join(redactedDir, bucket), RedactedExt
		}
		bucket = filepath.Join(prefix, bucket)
		// invoking KindDir for the kind's directory name, its defined below
		dir := KindDir(r.ApiVersion, r.Kind)
		// checking if the bucket is crds, it holds one kind so it gets no kind directory
		if bucket == crdsDir {
			dir = ""
		}
		ref := fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name)
		// trying the safe name first, then the safe name with shortHash added, both defined below
		for _, name := range []string{SafeName(r.Name), SafeName(r.Name) + "-" + shortHash(r.Name)} {
			rel := filepath.Join(dir, name+ext)
			key := strings.ToLower(filepath.Join(bucket, rel))
			// checking if this path is still free, compared lower-cased, if so r takes it
			if !used[key] {
				used[key] = true
				paths[i] = filePath{bucket, rel, refOnly}
				return nil
			}
		}
		// only reachable with a hash collision or duplicate resources, which snapshot.Build removes
		return fmt.Errorf("%s: no unique file name in %s", ref, filepath.Join(bucket, dir))
	}
	// assigning in two passes, names SafeName leaves as they are first, so they keep their name as the file name
	for _, plain := range []bool{true, false} {
		// going through the resources for the ones of this pass
		for i, r := range resources {
			// checking if r belongs to the other pass, if so its skipped for now
			if (SafeName(r.Name) == r.Name) != plain {
				continue
			}
			// invoking assign to give r its file, its defined above
			if err := assign(i); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

// ClusterDir gives the directory Emit writes a cluster's repository to, <dir>/clusters/<cluster>, and the one it replaces with Force
func ClusterDir(dir, cluster string) string {
	// invoking SafeName to make the cluster name safe as a directory name, its defined below
	name := SafeName(cluster)
	// checking if the name is empty or would name dir itself or its parent, if so it gets replaced
	switch name {
	case "":
		name = "cluster"
	case ".", "..":
		// "." would name dir itself and ".." its parent
		name = strings.Repeat("_", len(name))
	}
	return filepath.Join(dir, "clusters", name)
}

// isCRD tells if r is a CustomResourceDefinition
func isCRD(r *snapshotv1alpha1.Resource) bool {
	g, _, _ := strings.Cut(r.ApiVersion, "/")
	return g == "apiextensions.k8s.io" && r.Kind == "CustomResourceDefinition"
}

// shortHash gives 8 hex digits of s's sha-256, to tell apart names SafeName made equal
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// KindDir gives the directory for a kind, e.g. configmap or deployment.apps
// the kind and group come from the cluster or a manifest file, so they pass through SafeName, no "/" or other separator, and never "." or ".."
func KindDir(apiVersion, kind string) string {
	// invoking SafeName on the lowercased kind, its defined below
	k := SafeName(strings.ToLower(kind))
	// checking if the api version has a group, if so its added after a dot
	if g, _, ok := strings.Cut(apiVersion, "/"); ok {
		k += "." + SafeName(g)
	}
	// checking if k is "", "." or "..", no kind or nothing but dots, if so it becomes underscores
	if strings.Trim(k, ".") == "" {
		return strings.Repeat("_", max(len(k), 1))
	}
	return k
}

// SafeName makes a name safe for file paths on every os, kubernetes names may contain a colon (e.g. rbac), which windows forbids
func SafeName(name string) string {
	// mapping every rune that isnt a letter, digit, dash, dot or underscore to an underscore
	return strings.Map(func(r rune) rune {
		// checking if r is one of the safe runes
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}

// writeKustomization writes dir/kustomization.yaml listing resources
func (out *outDir) writeKustomization(dir string, resources []string) error {
	// checking if theres nothing to list, a null resources list makes kustomize fail with kustomization.yaml is empty, so it gets an empty one
	if resources == nil {
		resources = []string{}
	}
	b, err := yaml.Marshal(kustomization{
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		Kind:       "Kustomization",
		Resources:  resources,
	})
	if err != nil {
		return err
	}
	// invoking out.write to write it, its defined below
	return out.write(filepath.Join(dir, "kustomization.yaml"), b)
}

// outDir is a directory an export or its ExternalSecrets are written to
// every write goes through an os.Root opened on it, so no path, whatever a cluster or a manifest put in it, can leave the directory
// a path that tries ("../x", or through a symlink) fails instead
type outDir struct {
	root *os.Root
}

// openOutDir creates dir if needed and opens it for writing
func openOutDir(dir string) (*outDir, error) {
	// creating dir and its parents
	if err := os.MkdirAll(dir, 0o755); err != nil { //#nosec G301 -- an export is a Git repository, with no secret values
		return nil, err
	}
	// opening the os.Root every write goes through
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &outDir{root: root}, nil
}

// close releases the directory
func (out *outDir) close() error { return out.root.Close() }

// write writes path, relative to the directory, creating its directories
// the export is a repository to commit, with placeholders for every secret value, so its files are readable like any checkout's
func (out *outDir) write(path string, data []byte) error {
	path = filepath.Clean(path)
	// creating the file's directories inside the root
	if err := out.root.MkdirAll(filepath.Dir(path), 0o755); err != nil { //#nosec G301 -- see write's doc
		return err
	}
	return out.root.WriteFile(path, data, 0o644) //#nosec G306 -- see write's doc
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
