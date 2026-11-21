// builds the export with kustomize and server-side applies every resource with dryRun=All, comparing the result with the live object, nothing in the cluster changes
// it also finds the cluster's directory in an export, refuses remote kustomize references, and works out who owns or changed each differing field

package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/internal/kube"
	sdiff "setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/redact"
)

// FieldManager is the field manager of the dry-run applies, so verify's own entries can be told apart in managedFields
const FieldManager = "setdrift-verify"

// maxPaths caps the differing fields a Diff lists in Paths
const maxPaths = 5

// Cluster is what DryRunVerifier needs from a cluster, so tests can give it a fake one
type Cluster interface {
	// Get gives back the live object
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	// DryRunApply server-side applies obj with dryRun=All and gives back the result
	DryRunApply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// DynamicCluster is the real Cluster, reaching the API server through a dynamic client
type DynamicCluster struct{ Client dynamic.Interface }

// Get reads the live object from the cluster, for Cluster
func (c DynamicCluster) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	// invoking retry so a broken connection doesnt end the run, its defined below
	return retry(ctx, func() (*unstructured.Unstructured, error) {
		return c.Client.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	})
}

// retry runs call again, twice at most, when the connection to the API server broke, reset or closed mid-response
// both calls only read, so theyre safe to repeat, and one broken connection shouldnt end a run over hundreds of resources
func retry(ctx context.Context, call func() (*unstructured.Unstructured, error)) (*unstructured.Unstructured, error) {
	// trying up to 3 times, waiting a second longer after each attempt
	for attempt := 1; ; attempt++ {
		u, err := call()
		// checking if the call worked, was the last attempt, or failed for a reason other than a broken connection, if so its result is final
		if err == nil || attempt == 3 || !(utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err)) {
			return u, err
		}
		// waiting before the next attempt, unless the run is cancelled first
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
}

// DryRunApply server-side applies obj with dryRun=All and force, as FieldManager, and gives back what the object would become, for Cluster
func (c DynamicCluster) DryRunApply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}
	force := true
	// invoking retry so a broken connection doesnt end the run, its defined above
	return retry(ctx, func() (*unstructured.Unstructured, error) {
		return c.Client.Resource(gvr).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
			metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldManager, Force: &force})
	})
}

// DryRunVerifier runs the equivalent of kubectl apply --server-side --dry-run=server for every exported resource and compares the result with the live object, a clean resource means applying the export would change nothing
// force is set, the question is what the export would do, not whether another field manager owns a field
// only what the cluster's root kustomization.yaml builds is checked, as kubectl kustomize would build it, resources written for reference only under redacted/ are counted in Report.Skipped, and external-secrets/, which isnt applied, is left out
// a server-side apply only changes the fields it sends, so a field missing from the export doesnt show up as a difference, lost fields are caught only when a resource lost all of its spec or data, see lostFields
type DryRunVerifier struct {
	Cluster Cluster
	// Mapper maps each kind in the export to the API resource the cluster serves it as
	Mapper meta.RESTMapper
	// Concurrency is how many resources are checked at once, zero means 8
	Concurrency int
	// Set is the directory under the cluster's directory to build and check, like platform/aws/eks for the platform set, empty for the generic set
	Set string
	// ExportedAt is when the export was taken, from .setdrift-export, writes to a differing live object after it are reported as its cause
	// zero when unknown
	ExportedAt time.Time
	// Threshold is the share of resources, in percent, that must not change, zero means AcceptancePercent
	Threshold int
	// Namespaces limits the check to resources in these namespaces and their Namespace objects, as export -n reads them, empty checks everything
	Namespaces []string
}

// Verify checks the export at repoDir, a cluster's directory like clusters/<cluster> with a kustomization.yaml, or an export holding one cluster, and gives back the report
// errors about one resource go in the report, only errors that concern the entire run, like credentials or cancellation, are returned
func (v DryRunVerifier) Verify(ctx context.Context, repoDir string) (*Report, error) {
	// invoking ClusterRoot to find the cluster's directory, its defined below
	root, err := ClusterRoot(repoDir)
	if err != nil {
		return nil, err
	}
	// invoking localOnly to refuse an export that pulls in anything remote, its defined below
	if err := localOnly(root); err != nil {
		return nil, err
	}
	// invoking build to get what the chosen set's kustomization builds, its defined below
	objs, err := build(filepath.Join(root, filepath.FromSlash(v.Set)))
	if err != nil {
		return nil, err
	}
	// checking if namespaces were asked for, if so only their resources and their Namespace objects are kept, cluster-scoped resources go
	if len(v.Namespaces) > 0 {
		objs = slices.DeleteFunc(objs, func(o *unstructured.Unstructured) bool {
			// checking if the object is namespaced, if so it stays when its namespace was asked for
			if o.GetNamespace() != "" {
				return !slices.Contains(v.Namespaces, o.GetNamespace())
			}
			gvk := o.GroupVersionKind()
			return !(gvk.Group == "" && gvk.Kind == "Namespace" && slices.Contains(v.Namespaces, o.GetName()))
		})
	}
	report := &Report{Total: len(objs), Skipped: map[string]int{}, Threshold: v.Threshold, Namespaces: v.Namespaces}
	// invoking countRedacted to count the redacted files that are skipped on purpose, and checking if there are any, its defined below
	if n, err := countRedacted(root); err != nil {
		return nil, err
	} else if n > 0 {
		report.Skipped["redacted (Secret values were replaced; not applied on purpose)"] = n
	}

	// invoking missingCRDs to find the export's CRDs the cluster doesnt have yet, its defined below
	missing, err := v.missingCRDs(ctx, objs)
	if err != nil {
		return nil, err
	}
	report.MissingCRDs = slices.Sorted(maps.Values(missing))

	n := v.Concurrency
	// checking if no concurrency was set, if so 8 resources are checked at once
	if n <= 0 {
		n = 8
	}
	diffs := make([]*Diff, len(objs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(n)
	// checking each object in its own goroutine, each writing its own slot of diffs
	for i, o := range objs {
		g.Go(func() error {
			var err error
			// invoking check to dry-run the object and compare it with the live one, its defined below
			diffs[i], err = v.check(gctx, o, missing)
			return err
		})
	}
	// waiting for all the checks, the first error that isnt about one resource, like credentials or cancellation, stops the rest and only it is returned
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// sorting each result into the report
	for i, d := range diffs {
		gvk := objs[i].GroupVersionKind()
		// checking what the check found, clean, not checked, or a diff
		switch {
		case d == nil:
			report.Clean++
		case d.Kind == KindNotChecked && len(v.Namespaces) > 0 && gvk.Group == "" && gvk.Kind == "Namespace":
			// namespace-scoped access cant get or patch the cluster-scoped Namespace object, export -n leaves it out the same way
			report.Skipped["Namespace objects these credentials may not get or patch (cluster-scoped)"]++
			report.Total--
		case d.Kind == KindNotChecked:
			// it says nothing about the export, so its out of the count
			report.NotChecked = append(report.NotChecked, *d)
			report.Total--
		default:
			report.Diffs = append(report.Diffs, *d)
		}
	}
	// sorting the diffs and the resources not checked by ref, so the report reads the same every run
	sort.Slice(report.Diffs, func(i, j int) bool { return report.Diffs[i].Ref < report.Diffs[j].Ref })
	sort.Slice(report.NotChecked, func(i, j int) bool { return report.NotChecked[i].Ref < report.NotChecked[j].Ref })
	return report, nil
}

// ClusterRoot takes a directory and gives back the one to verify, dir itself if it has a kustomization.yaml, else the one cluster under dir/clusters/
// its an error when theres no cluster there, or more than one
func ClusterRoot(dir string) (string, error) {
	// checking if dir has its own kustomization.yaml, if so its the cluster's directory
	if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err == nil {
		return dir, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "clusters", "*", "kustomization.yaml"))
	if err != nil {
		return "", err
	}
	// checking how many clusters were found, none is not an export and one is the answer
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s has no kustomization.yaml and no clusters/<cluster>/kustomization.yaml: not an export", dir)
	case 1:
		return filepath.Dir(matches[0]), nil
	}
	var dirs []string
	// collecting each cluster's directory for the error, so the user can pick one
	for _, m := range matches {
		dirs = append(dirs, filepath.Dir(m))
	}
	return "", fmt.Errorf("%s holds %d clusters; verify one of them:\n  %s", dir, len(dirs), strings.Join(dirs, "\n  "))
}

// localOnly refuses an export whose kustomizations pull in anything remote
// kustomize would clone it, running git, and build it, so verifying a directory someone changed could fetch and apply-check code from anywhere
// an export only ever lists its own files
func localOnly(root string) error {
	// walking every file under root, looking for kustomizations
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// checking if the file is a kustomization, anything else is skipped
		switch d.Name() {
		case "kustomization.yaml", "kustomization.yml", "Kustomization":
		default:
			return nil
		}
		// checking if the kustomization is a symlink, an export's kustomizations are real files so its refused
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: an export's kustomizations are files", path)
		}
		b, err := os.ReadFile(path) //#nosec G304,G122 -- a file in the directory the user asked to verify
		if err != nil {
			return err
		}
		// only the lists that can name other files or directories are read
		var k struct {
			Resources  []string `json:"resources"`
			Components []string `json:"components"`
			Bases      []string `json:"bases"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// invoking remoteRef on each resource, component and base, and refusing the export if any is remote, its defined below
		for _, ref := range append(append(k.Resources, k.Components...), k.Bases...) {
			// checking if this entry is remote, if so the export is refused
			if remoteRef(ref) {
				return fmt.Errorf("%s lists %s, which is not a file of the export: verify builds only local files", path, ref)
			}
		}
		return nil
	})
}

// remoteRef tells if a kustomization entry names something remote, a url, a git repository or a hosted repository path
func remoteRef(ref string) bool {
	// checking the entry for each marker of a remote reference
	for _, marker := range []string{"://", "?ref=", "git@", "github.com/", "gitlab.com/", "bitbucket.org/"} {
		// checking if the entry has this marker, if so its remote
		if strings.Contains(ref, marker) {
			return true
		}
	}
	return false
}

// build gives back the objects the kustomization in root builds, as kubectl kustomize does, so what applying root would apply
func build(root string) ([]*unstructured.Unstructured, error) {
	// running kustomize with its default options on the files on disk
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), root)
	if err != nil {
		return nil, fmt.Errorf("building %s: %w", root, err)
	}
	var out []*unstructured.Unstructured
	// turning each built resource into an unstructured object
	for _, r := range resMap.Resources() {
		m, err := r.Map()
		if err != nil {
			return nil, fmt.Errorf("building %s: %w", root, err)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// Ref names an object as the snapshot does, [group/]Kind [namespace/]name
func Ref(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	gk := gvk.Kind
	// checking if the kind has a group, the core group has none
	if gvk.Group != "" {
		gk = gvk.Group + "/" + gvk.Kind
	}
	// checking if the object is namespaced, if so the namespace goes before the name
	if ns := obj.GetNamespace(); ns != "" {
		return gk + " " + ns + "/" + obj.GetName()
	}
	return gk + " " + obj.GetName()
}

// check takes one exported object and gives back nil if applying it changes nothing, otherwise a Diff saying what differs or went wrong
// errors that concern only this resource become a Diff, others like cancellation or credentials are returned
func (v DryRunVerifier) check(ctx context.Context, obj *unstructured.Unstructured, missingCRDs map[schema.GroupKind]string) (*Diff, error) {
	gvk := obj.GroupVersionKind()
	// invoking Ref to name the object in the diff, its defined above
	ref := Ref(obj)
	// diff builds a Diff of this resource with the given kind, explanation and paths
	diff := func(kind Kind, explanation string, paths ...string) (*Diff, error) {
		return &Diff{Ref: ref, Kind: kind, Paths: paths, Explanation: explanation}, nil
	}
	// notChecked builds a Diff for a resource the credentials were refused, saying what was refused and why
	notChecked := func(refused, why string) (*Diff, error) {
		return &Diff{Ref: ref, Kind: KindNotChecked, Refused: refused, Paths: []string{why}}, nil
	}

	mapping, err := v.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	// checking if the cluster doesnt serve the type
	if err != nil {
		// checking if its CRD is in the export but not the cluster yet, if so the diff says so
		if crd, ok := missingCRDs[gvk.GroupKind()]; ok {
			return diff(KindNotServed, "", "type not served by the cluster: its CustomResourceDefinition "+crd+" is in crds/ but not in the cluster yet")
		}
		return diff(KindNotServed, "", "type not served by the cluster: "+err.Error())
	}
	live, err := v.Cluster.Get(ctx, mapping.Resource, obj.GetNamespace(), obj.GetName())
	// checking why the live object couldnt be read, if it couldnt
	// a refused get or a missing object or one error about this object is a diff, invoking kube.IsObjectError to tell those apart, its defined in internal/kube/kube.go
	// any other error ends the run
	switch {
	case apierrors.IsForbidden(err):
		return notChecked(RefusedGet, "these credentials may not get it: "+err.Error())
	case apierrors.IsNotFound(err):
		return diff(KindNotInCluster, "", "not in the cluster: applying would create it")
	case err != nil && kube.IsObjectError(err):
		return diff(KindRejected, "", err.Error())
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	// invoking lostFields and checking if the export lost the live object's entire spec or data, which the apply wouldnt notice, its defined below
	if lost := lostFields(obj, live); len(lost) > 0 {
		d, _ := diff(KindLost, "", lost...)
		// invoking writesAfterExport to say who changed it since, its defined below
		d.ChangedAfterExport = v.writesAfterExport(live)
		return d, nil
	}
	result, err := v.Cluster.DryRunApply(ctx, mapping.Resource, obj)
	// checking why the dry-run failed, if it did
	// a refused patch or a rejection of this object is a diff, with explainApplyError naming a known cause, its defined below
	// any other error ends the run
	switch {
	case apierrors.IsForbidden(err):
		return notChecked(RefusedPatch, "these credentials may not patch it, which the dry-run needs (it changes nothing): "+err.Error())
	case err != nil && kube.IsObjectError(err):
		return diff(KindRejected, explainApplyError(gvk, err), err.Error())
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	// invoking applyComparable on both sides and sdiff.Fields to list the fields that differ, theyre defined below and in pkg/diff/diff.go
	fields := sdiff.Fields(applyComparable(live.Object), applyComparable(result.Object))
	// checking if nothing differs, if so the resource is clean
	if len(fields) == 0 {
		return nil, nil
	}
	// checking if more than one field differs, if so generation goes, it moves with any spec change and is noise beside the fields that moved it
	if len(fields) > 1 {
		kept := fields[:0]
		// keeping every field but generation
		for _, f := range fields {
			// checking if its some field other than generation, if so its kept
			if f.Path != "metadata.generation" {
				kept = append(kept, f)
			}
		}
		fields = kept
	}
	// invoking ownership to read who owns which live field, its defined below
	owned := ownership(live)
	d, _ := diff(KindChanged, "")
	// going through each differing field, listing the first maxPaths paths and keeping the first maxFields fields with their values
	for i, f := range fields {
		// checking if theres still room in the paths
		if i < maxPaths {
			d.Paths = append(d.Paths, f.Path)
		}
		// checking if theres still room in the fields
		if i < maxFields {
			// the live value never went through the redactor, so redact.FieldValue replaces a secret in it before it reaches the terminal or the report, its defined in pkg/redact/embedded.go
			// owners says who set the live value, its defined below
			d.Fields = append(d.Fields, Field{Path: f.Path, Cluster: redact.FieldValue(f.Path, f.A), Export: redact.FieldValue(f.Path, f.B),
				SetBy: owned.owners(f.Keys)})
		}
	}
	// checking if there were more fields than paths listed, if so the paths say how many more
	if len(fields) > maxPaths {
		d.Paths = append(d.Paths, fmt.Sprintf("and %d more", len(fields)-maxPaths))
	}
	// invoking writesAfterExport to say who changed the object since the export, its defined below
	d.ChangedAfterExport = v.writesAfterExport(live)
	return d, nil
}

// fieldOwners is a live object's managedFields, decoded
type fieldOwners []fieldOwner

// fieldOwner is one field manager and the fields it owns, as fieldsV1
type fieldOwner struct {
	owner Owner
	// fields is the manager's fieldsV1 decoded, keyed like f:spec
	fields map[string]any
}

// ownership decodes a live object's managedFields, leaving out verify's own dry-runs and entries that cant be decoded
func ownership(live *unstructured.Unstructured) fieldOwners {
	var out fieldOwners
	// going through each managedFields entry
	for _, m := range live.GetManagedFields() {
		// checking if its verify's own entry, or has no fields
		if m.Manager == FieldManager || m.FieldsV1 == nil {
			continue
		}
		var f map[string]any
		// checking if the fields cant be decoded, if so the entry is left out
		if json.Unmarshal(m.FieldsV1.GetRawBytes(), &f) != nil {
			continue
		}
		out = append(out, fieldOwner{Owner{Manager: m.Manager, Operation: string(m.Operation), Subresource: m.Subresource}, f})
	}
	return out
}

// owners gives back who owns the field at keys, the steps diff.Field.Keys gives in pkg/diff/diff.go
func (fo fieldOwners) owners(keys []string) []Owner {
	var out []Owner
	// invoking owns on each manager's fields and keeping the ones that own it, its defined below
	for _, o := range fo {
		// checking if this manager owns any of the fields, if so its kept
		if owns(o.fields, keys) {
			out = append(out, o.owner)
		}
	}
	return out
}

// owns tells if a manager's fieldsV1 includes the field at keys, the field itself or an atomic value holding it, which shows as a leaf before keys end
// it follows f-prefixed map keys and n-prefixed named list items, anything else like an i-prefixed index isnt matched
func owns(node map[string]any, keys []string) bool {
	// walking down one key at a time
	for _, k := range keys {
		// checking if node is a leaf, if so the manager owns the entire value, like an atomic list
		if len(node) == 0 {
			return true
		}
		var next any
		// checking what kind of step the key is
		switch {
		case strings.HasPrefix(k, "f:"):
			next = node[k]
		case strings.HasPrefix(k, "n:"):
			// a list item keyed by name, which managedFields writes as a k-prefixed key holding the json of the name
			name := strings.TrimPrefix(k, "n:")
			// looking through the node's keyed items for the one with that name
			for key, v := range node {
				var item map[string]any
				// checking if the key is a k-prefixed item whose json has the name
				if strings.HasPrefix(key, "k:") && json.Unmarshal([]byte(strings.TrimPrefix(key, "k:")), &item) == nil && item["name"] == name {
					next = v
					break
				}
			}
		default:
			return false
		}
		m, ok := next.(map[string]any)
		// checking if the step wasnt found, if so the manager doesnt own the field
		if !ok {
			return false
		}
		node = m
	}
	return true
}

// maxFields caps the fields with values a Diff keeps
const maxFields = 50

// writesAfterExport lists who wrote the live object after the export was taken, from its managedFields, oldest first
// writes to its status, where controllers report progress, and verify's own dry-runs are left out
func (v DryRunVerifier) writesAfterExport(live *unstructured.Unstructured) []Write {
	// checking if the export time is unknown, if so nothing can be said
	if v.ExportedAt.IsZero() {
		return nil
	}
	var out []Write
	// going through each managedFields entry
	for _, m := range live.GetManagedFields() {
		// checking if the entry has no time, is from before the export, is a status write or is verify's own
		if m.Time == nil || !m.Time.After(v.ExportedAt) || m.Subresource == "status" || m.Manager == FieldManager {
			continue
		}
		out = append(out, Write{Manager: m.Manager, Operation: string(m.Operation), Time: m.Time.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// crdGVK is the kind of a CustomResourceDefinition, missingCRDs looks the export's CRDs up through it
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// missingCRDs gives back the export's CustomResourceDefinitions that the cluster lacks, by the group and kind they define
// errors about one CRD are ignored, its own check reports them, others are returned
func (v DryRunVerifier) missingCRDs(ctx context.Context, objs []*unstructured.Unstructured) (map[schema.GroupKind]string, error) {
	missing := map[schema.GroupKind]string{}
	mapping, err := v.Mapper.RESTMapping(crdGVK.GroupKind(), crdGVK.Version)
	// checking if CRDs arent served at all, if so every CRD's own check says so
	if err != nil {
		return missing, nil
	}
	// going through each object, looking for CRDs
	for _, o := range objs {
		// checking if its not a CRD
		if o.GroupVersionKind().GroupKind() != crdGVK.GroupKind() {
			continue
		}
		_, err := v.Cluster.Get(ctx, mapping.Resource, "", o.GetName())
		// checking if the cluster doesnt have it, if so the group and kind it defines are missing
		// an error that isnt about this CRD ends the run, kube.IsObjectError is defined in internal/kube/kube.go
		switch {
		case apierrors.IsNotFound(err):
			group, _, _ := unstructured.NestedString(o.Object, "spec", "group")
			kind, _, _ := unstructured.NestedString(o.Object, "spec", "names", "kind")
			missing[schema.GroupKind{Group: group, Kind: kind}] = o.GetName()
		case err != nil && !kube.IsObjectError(err):
			// invoking Ref to name the CRD in the error, its defined above
			return nil, fmt.Errorf("%s: %w", Ref(o), err)
		}
	}
	return missing, nil
}

// lostFields gives back the spec or data the live object has and the export lost entirely, the apply wouldnt notice but a new cluster would get an object without them
// Namespaces, whose only spec is the default finalizer, and service account token Secrets, whose data the token controller writes, lose them on purpose
func lostFields(exported, live *unstructured.Unstructured) []string {
	gk := live.GroupVersionKind().GroupKind()
	// checking if its a Namespace, if so its spec is lost on purpose
	if gk.Group == "" && gk.Kind == "Namespace" {
		return nil
	}
	var out []string
	// checking spec and data each
	for _, f := range []string{"spec", "data"} {
		// checking if its a Secret's data, if so a service account token's data is lost on purpose
		if f == "data" && gk.Group == "" && gk.Kind == "Secret" {
			// checking if its a service account token Secret, if so losing its data is expected and not flagged
			if t, _, _ := unstructured.NestedString(live.Object, "type"); t == "kubernetes.io/service-account-token" {
				continue
			}
		}
		// invoking emptyField to check if the live object has it and the export doesnt, its defined below
		if !emptyField(live.Object[f]) && emptyField(exported.Object[f]) {
			out = append(out, "."+f+": in the cluster but missing from the export, so a new cluster would get the object without it")
		}
	}
	return out
}

// emptyField tells if a field is missing, nil, or an empty map or list
func emptyField(v any) bool {
	// checking what the value is
	switch n := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(n) == 0
	case []any:
		return len(n) == 0
	}
	return false
}

// explainApplyError names known reasons an apply of an unchanged export is rejected, empty when its not a known one
// it matches the API server's message, the Invalid error carries no structured cause for it
func explainApplyError(gvk schema.GroupVersionKind, err error) string {
	// checking if a Pod was rejected because pods cant be updated in place
	if gvk.Group == "" && gvk.Kind == "Pod" && apierrors.IsInvalid(err) && strings.Contains(err.Error(), "pod updates may not change") {
		return "Pods can't be updated in place; a bare Pod in Git is recreated, not applied"
	}
	return ""
}

// applyComparable gives back a copy of obj without what an apply always changes, field ownership and the resource version
// generation is kept, it only moves when the spec does
func applyComparable(obj map[string]any) map[string]any {
	c := (&unstructured.Unstructured{Object: obj}).DeepCopy().Object
	unstructured.RemoveNestedField(c, "metadata", "managedFields")
	unstructured.RemoveNestedField(c, "metadata", "resourceVersion")
	// an apply never changes status, which controllers keep changing between the read and the dry-run, like a Pod starting
	delete(c, "status")
	return c
}

// countRedacted counts the reference-only redacted files under dir, the ones named with kustomize.RedactedExt from pkg/emit/kustomize/kustomize.go
func countRedacted(dir string) (int, error) {
	n := 0
	// walking every file under dir and counting the redacted ones
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// checking if its a readable file with the redacted extension, if so its counted
		if err == nil && !d.IsDir() && strings.HasSuffix(path, kustomize.RedactedExt) {
			n++
		}
		return err
	})
	return n, err
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
