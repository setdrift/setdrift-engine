// builds the export with kustomize and server-side applies every resource with dryRun=All, comparing the result with the live object, nothing in the cluster changes
// it also finds the cluster's directory in an export, refuses remote kustomize references, and works out who owns or changed each differing field

package verify

import (
	"context"
	"encoding/json"
	"errors"
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
	"setdrift.io/setdrift-engine/pkg/capability"
	sdiff "setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
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

// AccessChecker is what a Cluster can also do, ask the API server whether these credentials may patch a type in a namespace, so verify can stop at once when they may patch nothing
type AccessChecker interface {
	// CanPatch tells if the credentials may patch the resource type in the namespace, "" for cluster-scoped
	CanPatch(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (bool, error)
}

// errNoPatch is the error verify gives back when the credentials may patch nothing in the export
var errNoPatch = errors.New("these credentials may not patch anything in the export, and verify needs patch for its dry-run apply (it changes nothing); " +
	"bind them to the setdrift-verify role in deploy/permissions/kubernetes/setdrift-verify.yaml, or setdrift-verify-namespace.yaml for some namespaces")

// CanPatch asks the API server with a SelfSubjectAccessReview whether these credentials may patch gvr in namespace, for AccessChecker
func (c DynamicCluster) CanPatch(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (bool, error) {
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": "patch", "group": gvr.Group, "resource": gvr.Resource, "namespace": namespace,
		}},
	}}
	ssar := schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews"}
	out, err := c.Client.Resource(ssar).Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	allowed, _, _ := unstructured.NestedBool(out.Object, "status", "allowed")
	return allowed, nil
}

// canPatchAny takes the export's objects and tells if the credentials may patch at least one of their types, in its namespace
// each type and namespace is asked about once, and it stops at the first one allowed, so a cluster that allows patch is asked once or a few times
// a cluster that cant answer, or a type the cluster doesnt serve, counts as allowed, so verify goes on and reports each resource as it does without the check
func (v DryRunVerifier) canPatchAny(ctx context.Context, objs []*unstructured.Unstructured) (bool, error) {
	ac, ok := v.Cluster.(AccessChecker)
	// checking if the cluster can be asked at all, a fake one in tests may not
	if !ok || len(objs) == 0 {
		return true, nil
	}
	asked := map[string]bool{}
	// going through the objects, asking about each type and namespace once
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		mapping, err := v.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		// checking if the cluster doesnt serve the type, if so its not asked about, check reports it
		if err != nil {
			continue
		}
		key := mapping.Resource.String() + "/" + o.GetNamespace()
		if asked[key] {
			continue
		}
		asked[key] = true
		allowed, err := ac.CanPatch(ctx, mapping.Resource, o.GetNamespace())
		// checking if the API server couldnt answer, if so verify goes on without the check
		if err != nil {
			return true, nil
		}
		if allowed {
			return true, nil
		}
	}
	// checking if anything was asked, if not theres nothing to say
	return len(asked) == 0, nil
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
	// Target is this cluster's platform, as classified with confidence, for the capability check, Unknown skips it
	Target platform.Distribution
	// Capability is the capability map the check reads, nil skips it
	Capability *capability.Map
	// Explain gives back why an object that wont work here is that way on purpose, like a decision in a translation's translate-decisions.yaml, empty when it isnt
	Explain func(*unstructured.Unstructured) string
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
	objs, err := build(root, filepath.Join(root, filepath.FromSlash(v.Set)))
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
	// invoking canPatchAny to stop at once when the credentials may patch nothing, rather than dry-run every resource to be refused, its defined above
	if ok, err := v.canPatchAny(ctx, objs); err != nil {
		return nil, err
	} else if !ok {
		return nil, errNoPatch
	}

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
	// invoking misfits to add the resources that would apply but name what this platform doesnt have, its defined below
	v.misfits(objs, diffs, report)
	// sorting the diffs and the resources not checked by ref, so the report reads the same every run
	sort.Slice(report.Diffs, func(i, j int) bool { return report.Diffs[i].Ref < report.Diffs[j].Ref })
	sort.Slice(report.NotChecked, func(i, j int) bool { return report.NotChecked[i].Ref < report.NotChecked[j].Ref })
	return report, nil
}

// misfits checks the objects against the capability map for the target platform
// a resource that was clean becomes a KindWontWork diff, one that already differs gets the reasons added, since both mean it wont work here as exported
func (v DryRunVerifier) misfits(objs []*unstructured.Unstructured, diffs []*Diff, report *Report) {
	// checking if theres a map and a known target, if not theres nothing to check against
	if v.Capability == nil || v.Target == platform.Unknown {
		return
	}
	in := make([]capability.Object, len(objs))
	index := map[string]int{}
	for i, o := range objs {
		gvk := o.GroupVersionKind()
		// invoking Ref to name the object as the report does, its defined below
		in[i] = capability.Object{Ref: Ref(o), Group: gvk.Group, Kind: gvk.Kind, Content: o.Object}
		index[in[i].Ref] = i
	}
	// invoking Misfits to find the objects naming another platform's markers, its defined in pkg/capability/capability.go
	for _, mf := range v.Capability.Misfits(in, v.Target) {
		i := index[mf.Ref]
		// checking how the dry-run left the object, clean, not checked, or already a diff
		switch d := diffs[i]; {
		case d == nil:
			report.Clean--
			nd := Diff{Ref: mf.Ref, Kind: KindWontWork, Paths: mf.Reasons}
			// checking if theres a reason its this way on purpose, if so it counts as explained
			if v.Explain != nil {
				nd.Explanation = v.Explain(objs[i])
			}
			report.Diffs = append(report.Diffs, nd)
		case d.Kind == KindNotChecked:
			// it wasnt counted, so the reasons go with it where its listed
			for j := range report.NotChecked {
				if report.NotChecked[j].Ref == mf.Ref {
					report.NotChecked[j].Paths = append(report.NotChecked[j].Paths, mf.Reasons...)
				}
			}
		default:
			for j := range report.Diffs {
				if report.Diffs[j].Ref == mf.Ref {
					report.Diffs[j].Paths = append(report.Diffs[j].Paths, mf.Reasons...)
				}
			}
		}
	}
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

// notPaths are the kustomization fields whose values are never paths kustomize loads, like labels or an inline patch, so a url in them is fine
var notPaths = map[string]bool{
	"apiVersion": true, "kind": true, "metadata": true, "namespace": true, "namePrefix": true, "nameSuffix": true,
	"commonLabels": true, "commonAnnotations": true, "labels": true, "images": true, "replicas": true,
	"literals": true, "patch": true, "buildMetadata": true, "sortOptions": true, "vars": true, "options": true,
}

// localOnly takes the cluster's directory and refuses an export that would make kustomize fetch anything remote or follow a symlink
// kustomize clones a git repository or downloads a url named in any field that names a file or directory, so a changed export could pull in code from anywhere
// a symlink could lead kustomize out of the export, and an export only ever holds its own regular files
func localOnly(root string) error {
	// walking every file and directory under root, looking for symlinks and kustomizations
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// checking if its a symlink, file or directory, if so the export is refused
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: verify builds only an export's own files", path)
		}
		// checking if the file is a kustomization, anything else is skipped
		switch d.Name() {
		case "kustomization.yaml", "kustomization.yml", "Kustomization":
		default:
			return nil
		}
		b, err := os.ReadFile(path) //#nosec G304,G122 -- a file in the directory the user asked to verify
		if err != nil {
			return err
		}
		var k map[string]any
		if err := yaml.Unmarshal(b, &k); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// invoking remoteValue to find the first value in any path field kustomize would fetch remotely, its defined below
		if ref, ok := remoteValue(k); ok {
			return fmt.Errorf("%s names %s, which is not a file of the export: verify builds only local files", path, ref)
		}
		return nil
	})
}

// remoteValue takes a decoded kustomization, or a part of one, and gives back the first string kustomize would fetch remotely
// it looks at every value except those under notPaths, so a new kustomize field that names files is covered too
func remoteValue(v any) (string, bool) {
	// checking what kind of value it is, a map and a list are searched, a string is matched with kube.KustomizeRemote, its defined in internal/kube/kube.go
	switch v := v.(type) {
	case string:
		return v, kube.KustomizeRemote(v)
	case map[string]any:
		// going through each field that can hold paths
		for key, child := range v {
			// checking if the field never holds paths, if so its skipped
			if notPaths[key] {
				continue
			}
			// checking if anything under the field is remote, if so thats the answer
			if ref, ok := remoteValue(child); ok {
				return ref, true
			}
		}
	case []any:
		// going through each item of the list
		for _, child := range v {
			// checking if the item is remote, if so thats the answer
			if ref, ok := remoteValue(child); ok {
				return ref, true
			}
		}
	}
	return "", false
}

// sealedRoot is where build puts its copy of the export in memory, a fixed path so a windows drive letter never gets in the way
const sealedRoot = "/export"

// build takes the cluster's directory and the directory inside it to build, and gives back the objects that kustomization builds, as kubectl kustomize does
// kustomize runs on an in-memory copy of the export's regular files, so it cant read anything outside the export even if localOnly missed something
// Build gives back what the kustomization in dir builds, dir being root, a cluster's directory, or a set under it, after refusing anything remote, as verify does
// translate builds the generic set with it
func Build(root, dir string) ([]*unstructured.Unstructured, error) {
	// invoking localOnly to refuse an export that pulls in anything remote, its defined below
	if err := localOnly(root); err != nil {
		return nil, err
	}
	return build(root, dir)
}

func build(root, dir string) ([]*unstructured.Unstructured, error) {
	// invoking sealedCopy to copy the export into memory, its defined below
	fSys, err := sealedCopy(root)
	if err != nil {
		return nil, fmt.Errorf("building %s: %w", dir, err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, fmt.Errorf("building %s: %w", dir, err)
	}
	// running kustomize with its default options on the copy, plugins and helm stay off
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fSys, filepath.Join(sealedRoot, rel))
	if err != nil {
		// putting the real directory back in kustomize's message in place of the copy's path
		return nil, fmt.Errorf("building %s: %s", dir, strings.ReplaceAll(err.Error(), sealedRoot, root))
	}
	var out []*unstructured.Unstructured
	// turning each built resource into an unstructured object
	for _, r := range resMap.Resources() {
		m, err := r.Map()
		if err != nil {
			return nil, fmt.Errorf("building %s: %w", dir, err)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// sealedCopy takes the cluster's directory and copies its regular files into an in-memory filesystem under sealedRoot, refusing a symlink or any other special file
func sealedCopy(root string) (filesys.FileSystem, error) {
	fSys := filesys.MakeFsInMemory()
	// walking every file and directory under root to copy it
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(sealedRoot, rel)
		// checking what the entry is, a directory is made, a regular file is copied, anything else is refused
		switch {
		case d.IsDir():
			return fSys.MkdirAll(dst)
		case d.Type().IsRegular():
			b, err := os.ReadFile(path) //#nosec G304,G122 -- a regular file in the directory the user asked to verify
			if err != nil {
				return err
			}
			return fSys.WriteFile(dst, b)
		default:
			return fmt.Errorf("%s is not a regular file: verify builds only an export's own files", path)
		}
	})
	return fSys, err
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
