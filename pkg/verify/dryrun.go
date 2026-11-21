// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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

// FieldManager is the field manager of the dry-run applies.
const FieldManager = "setdrift-verify"

// maxPaths caps the differing fields a Diff lists.
const maxPaths = 5

// Cluster is what DryRunVerifier needs from a cluster.
type Cluster interface {
	// Get returns the live object.
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	// DryRunApply server-side applies obj with dryRun=All and returns the
	// result.
	DryRunApply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// DynamicCluster implements Cluster with a dynamic client.
type DynamicCluster struct{ Client dynamic.Interface }

// Get implements Cluster.
func (c DynamicCluster) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	return retry(ctx, func() (*unstructured.Unstructured, error) {
		return c.Client.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	})
}

// retry runs call again, twice at most, when the connection to the API server broke (reset, or closed mid-response):
// both calls only read, so they are safe to repeat, and one broken connection should not end a run over hundreds
// of resources.
func retry(ctx context.Context, call func() (*unstructured.Unstructured, error)) (*unstructured.Unstructured, error) {
	for attempt := 1; ; attempt++ {
		u, err := call()
		if err == nil || attempt == 3 || !(utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err)) {
			return u, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
}

// DryRunApply implements Cluster.
func (c DynamicCluster) DryRunApply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}
	force := true
	return retry(ctx, func() (*unstructured.Unstructured, error) {
		return c.Client.Resource(gvr).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
			metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldManager, Force: &force})
	})
}

// DryRunVerifier runs `kubectl apply --server-side --dry-run=server` for
// every exported resource and compares the result with the live object. A
// clean resource means applying the export would change nothing.
//
// Force is set: the question is what the export would do, not whether
// another field manager owns a field. Only what the cluster's root
// kustomization.yaml builds is checked, as kubectl kustomize would build it:
// resources written for reference only (redacted/) are counted in
// Report.Skipped, and external-secrets/, which is not applied, is left out.
//
// A server-side apply only changes the fields it sends, so a field missing
// from the export does not show up as a difference. Lost fields are caught
// only when a resource lost all of its spec or data (see lostFields).
type DryRunVerifier struct {
	Cluster Cluster
	Mapper  meta.RESTMapper
	// Concurrency is how many resources are checked at once; zero means 8.
	Concurrency int
	// Set is the directory under the cluster's directory to build and check,
	// e.g. platform/aws/eks for the platform set; empty for the generic set.
	Set string
	// ExportedAt is when the export was taken, from .setdrift-export: writes
	// to a differing live object after it are reported as its cause. Zero
	// when unknown.
	ExportedAt time.Time
	// Threshold is the share of resources, in percent, that must not change;
	// zero means AcceptancePercent.
	Threshold int
	// Namespaces limits the check to resources in these namespaces and their
	// Namespace objects, as export -n reads them; empty checks everything.
	Namespaces []string
}

// Verify checks the export at repoDir: a cluster's directory
// (clusters/<cluster>, with a kustomization.yaml) or an export holding one
// cluster.
func (v DryRunVerifier) Verify(ctx context.Context, repoDir string) (*Report, error) {
	root, err := ClusterRoot(repoDir)
	if err != nil {
		return nil, err
	}
	if err := localOnly(root); err != nil {
		return nil, err
	}
	objs, err := build(filepath.Join(root, filepath.FromSlash(v.Set)))
	if err != nil {
		return nil, err
	}
	if len(v.Namespaces) > 0 {
		objs = slices.DeleteFunc(objs, func(o *unstructured.Unstructured) bool {
			if o.GetNamespace() != "" {
				return !slices.Contains(v.Namespaces, o.GetNamespace())
			}
			gvk := o.GroupVersionKind()
			return !(gvk.Group == "" && gvk.Kind == "Namespace" && slices.Contains(v.Namespaces, o.GetName()))
		})
	}
	report := &Report{Total: len(objs), Skipped: map[string]int{}, Threshold: v.Threshold, Namespaces: v.Namespaces}
	if n, err := countRedacted(root); err != nil {
		return nil, err
	} else if n > 0 {
		report.Skipped["redacted (Secret values were replaced; not applied on purpose)"] = n
	}

	missing, err := v.missingCRDs(ctx, objs)
	if err != nil {
		return nil, err
	}
	report.MissingCRDs = slices.Sorted(maps.Values(missing))

	n := v.Concurrency
	if n <= 0 {
		n = 8
	}
	diffs := make([]*Diff, len(objs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(n)
	for i, o := range objs {
		g.Go(func() error {
			var err error
			diffs[i], err = v.check(gctx, o, missing)
			return err
		})
	}
	// The first error that is not about one resource (credentials,
	// cancellation) stops the rest; only it is returned.
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for i, d := range diffs {
		gvk := objs[i].GroupVersionKind()
		switch {
		case d == nil:
			report.Clean++
		case d.Kind == KindNotChecked && len(v.Namespaces) > 0 && gvk.Group == "" && gvk.Kind == "Namespace":
			// namespace-scoped access can't get or patch the cluster-scoped Namespace object; export -n leaves it out the same way
			report.Skipped["Namespace objects these credentials may not get or patch (cluster-scoped)"]++
			report.Total--
		case d.Kind == KindNotChecked:
			// says nothing about the export: out of the count
			report.NotChecked = append(report.NotChecked, *d)
			report.Total--
		default:
			report.Diffs = append(report.Diffs, *d)
		}
	}
	sort.Slice(report.Diffs, func(i, j int) bool { return report.Diffs[i].Ref < report.Diffs[j].Ref })
	sort.Slice(report.NotChecked, func(i, j int) bool { return report.NotChecked[i].Ref < report.NotChecked[j].Ref })
	return report, nil
}

// ClusterRoot returns the directory to verify: dir itself if it has a
// kustomization.yaml, else the one cluster under dir/clusters/.
func ClusterRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err == nil {
		return dir, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "clusters", "*", "kustomization.yaml"))
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s has no kustomization.yaml and no clusters/<cluster>/kustomization.yaml: not an export", dir)
	case 1:
		return filepath.Dir(matches[0]), nil
	}
	var dirs []string
	for _, m := range matches {
		dirs = append(dirs, filepath.Dir(m))
	}
	return "", fmt.Errorf("%s holds %d clusters; verify one of them:\n  %s", dir, len(dirs), strings.Join(dirs, "\n  "))
}

// localOnly refuses an export whose kustomizations pull in anything remote:
// kustomize would clone it (running git) and build it, so verifying a
// directory someone changed could fetch and apply-check code from anywhere.
// An export only ever lists its own files.
func localOnly(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch d.Name() {
		case "kustomization.yaml", "kustomization.yml", "Kustomization":
		default:
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: an export's kustomizations are files", path)
		}
		b, err := os.ReadFile(path) //#nosec G304,G122 -- a file in the directory the user asked to verify
		if err != nil {
			return err
		}
		var k struct {
			Resources  []string `json:"resources"`
			Components []string `json:"components"`
			Bases      []string `json:"bases"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, ref := range append(append(k.Resources, k.Components...), k.Bases...) {
			if remoteRef(ref) {
				return fmt.Errorf("%s lists %s, which is not a file of the export: verify builds only local files", path, ref)
			}
		}
		return nil
	})
}

// remoteRef reports whether a kustomization entry names something remote:
// a URL, a git repository or a hosted repository path.
func remoteRef(ref string) bool {
	for _, marker := range []string{"://", "?ref=", "git@", "github.com/", "gitlab.com/", "bitbucket.org/"} {
		if strings.Contains(ref, marker) {
			return true
		}
	}
	return false
}

// build returns the objects the kustomization in root builds, as kubectl
// kustomize does: what applying root would apply.
func build(root string) ([]*unstructured.Unstructured, error) {
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), root)
	if err != nil {
		return nil, fmt.Errorf("building %s: %w", root, err)
	}
	var out []*unstructured.Unstructured
	for _, r := range resMap.Resources() {
		m, err := r.Map()
		if err != nil {
			return nil, fmt.Errorf("building %s: %w", root, err)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// Ref names an object as the snapshot does: [group/]Kind [namespace/]name.
func Ref(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	gk := gvk.Kind
	if gvk.Group != "" {
		gk = gvk.Group + "/" + gvk.Kind
	}
	if ns := obj.GetNamespace(); ns != "" {
		return gk + " " + ns + "/" + obj.GetName()
	}
	return gk + " " + obj.GetName()
}

// check returns nil if applying obj changes nothing. Errors that concern
// only this resource become a Diff; others (cancellation, credentials) are
// returned.
func (v DryRunVerifier) check(ctx context.Context, obj *unstructured.Unstructured, missingCRDs map[schema.GroupKind]string) (*Diff, error) {
	gvk := obj.GroupVersionKind()
	ref := Ref(obj)
	diff := func(kind Kind, explanation string, paths ...string) (*Diff, error) {
		return &Diff{Ref: ref, Kind: kind, Paths: paths, Explanation: explanation}, nil
	}
	notChecked := func(refused, why string) (*Diff, error) {
		return &Diff{Ref: ref, Kind: KindNotChecked, Refused: refused, Paths: []string{why}}, nil
	}

	mapping, err := v.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if crd, ok := missingCRDs[gvk.GroupKind()]; ok {
			return diff(KindNotServed, "", "type not served by the cluster: its CustomResourceDefinition "+crd+" is in crds/ but not in the cluster yet")
		}
		return diff(KindNotServed, "", "type not served by the cluster: "+err.Error())
	}
	live, err := v.Cluster.Get(ctx, mapping.Resource, obj.GetNamespace(), obj.GetName())
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
	if lost := lostFields(obj, live); len(lost) > 0 {
		d, _ := diff(KindLost, "", lost...)
		d.ChangedAfterExport = v.writesAfterExport(live)
		return d, nil
	}
	result, err := v.Cluster.DryRunApply(ctx, mapping.Resource, obj)
	switch {
	case apierrors.IsForbidden(err):
		return notChecked(RefusedPatch, "these credentials may not patch it, which the dry-run needs (it changes nothing): "+err.Error())
	case err != nil && kube.IsObjectError(err):
		return diff(KindRejected, explainApplyError(gvk, err), err.Error())
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	fields := sdiff.Fields(applyComparable(live.Object), applyComparable(result.Object))
	if len(fields) == 0 {
		return nil, nil
	}
	// generation moves with any spec change: noise beside the fields that moved it
	if len(fields) > 1 {
		kept := fields[:0]
		for _, f := range fields {
			if f.Path != "metadata.generation" {
				kept = append(kept, f)
			}
		}
		fields = kept
	}
	owned := ownership(live)
	d, _ := diff(KindChanged, "")
	for i, f := range fields {
		if i < maxPaths {
			d.Paths = append(d.Paths, f.Path)
		}
		if i < maxFields {
			// the live value never went through the redactor: a secret in it is
			// replaced before it reaches the terminal or the report
			d.Fields = append(d.Fields, Field{Path: f.Path, Cluster: redact.FieldValue(f.Path, f.A), Export: redact.FieldValue(f.Path, f.B),
				SetBy: owned.owners(f.Keys)})
		}
	}
	if len(fields) > maxPaths {
		d.Paths = append(d.Paths, fmt.Sprintf("and %d more", len(fields)-maxPaths))
	}
	d.ChangedAfterExport = v.writesAfterExport(live)
	return d, nil
}

// fieldOwners is a live object's managedFields, decoded.
type fieldOwners []fieldOwner

// fieldOwner is one field manager and the fields it owns, as fieldsV1.
type fieldOwner struct {
	owner  Owner
	fields map[string]any
}

// ownership decodes a live object's managedFields; verify's own dry-runs
// are left out.
func ownership(live *unstructured.Unstructured) fieldOwners {
	var out fieldOwners
	for _, m := range live.GetManagedFields() {
		if m.Manager == FieldManager || m.FieldsV1 == nil {
			continue
		}
		var f map[string]any
		if json.Unmarshal(m.FieldsV1.GetRawBytes(), &f) != nil {
			continue
		}
		out = append(out, fieldOwner{Owner{Manager: m.Manager, Operation: string(m.Operation), Subresource: m.Subresource}, f})
	}
	return out
}

// owners returns who owns the field at keys (see diff.Field.Keys).
func (fo fieldOwners) owners(keys []string) []Owner {
	var out []Owner
	for _, o := range fo {
		if owns(o.fields, keys) {
			out = append(out, o.owner)
		}
	}
	return out
}

// owns reports whether a manager's fieldsV1 includes the field at keys:
// the field itself, or an atomic value holding it (a leaf before keys end).
func owns(node map[string]any, keys []string) bool {
	for _, k := range keys {
		if len(node) == 0 {
			// a leaf: the manager owns the whole value, e.g. an atomic list
			return true
		}
		var next any
		switch {
		case strings.HasPrefix(k, "f:"):
			next = node[k]
		case strings.HasPrefix(k, "n:"):
			// a list item keyed by name: k:{"name":"web"}
			name := strings.TrimPrefix(k, "n:")
			for key, v := range node {
				var item map[string]any
				if strings.HasPrefix(key, "k:") && json.Unmarshal([]byte(strings.TrimPrefix(key, "k:")), &item) == nil && item["name"] == name {
					next = v
					break
				}
			}
		default:
			return false
		}
		m, ok := next.(map[string]any)
		if !ok {
			return false
		}
		node = m
	}
	return true
}

// maxFields caps the fields with values a Diff keeps.
const maxFields = 50

// writesAfterExport lists who wrote the live object after the export was
// taken, from its managedFields: writes to its status (controllers report
// progress there) and verify's own dry-runs are left out.
func (v DryRunVerifier) writesAfterExport(live *unstructured.Unstructured) []Write {
	if v.ExportedAt.IsZero() {
		return nil
	}
	var out []Write
	for _, m := range live.GetManagedFields() {
		if m.Time == nil || !m.Time.After(v.ExportedAt) || m.Subresource == "status" || m.Manager == FieldManager {
			continue
		}
		out = append(out, Write{Manager: m.Manager, Operation: string(m.Operation), Time: m.Time.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// missingCRDs returns the export's CustomResourceDefinitions that the cluster lacks, by the group and kind they
// define. Errors about one CRD are ignored (its own check reports them); others are returned.
func (v DryRunVerifier) missingCRDs(ctx context.Context, objs []*unstructured.Unstructured) (map[schema.GroupKind]string, error) {
	missing := map[schema.GroupKind]string{}
	mapping, err := v.Mapper.RESTMapping(crdGVK.GroupKind(), crdGVK.Version)
	if err != nil {
		return missing, nil // no CRDs served at all; every CRD's own check says so
	}
	for _, o := range objs {
		if o.GroupVersionKind().GroupKind() != crdGVK.GroupKind() {
			continue
		}
		_, err := v.Cluster.Get(ctx, mapping.Resource, "", o.GetName())
		switch {
		case apierrors.IsNotFound(err):
			group, _, _ := unstructured.NestedString(o.Object, "spec", "group")
			kind, _, _ := unstructured.NestedString(o.Object, "spec", "names", "kind")
			missing[schema.GroupKind{Group: group, Kind: kind}] = o.GetName()
		case err != nil && !kube.IsObjectError(err):
			return nil, fmt.Errorf("%s: %w", Ref(o), err)
		}
	}
	return missing, nil
}

// lostFields reports spec or data that the live object has and the export
// lost entirely: the apply would not notice, but a new cluster would get an
// object without them. Namespaces (whose only spec is the default
// finalizer) and service account token Secrets (whose data the token
// controller writes) lose them on purpose.
func lostFields(exported, live *unstructured.Unstructured) []string {
	gk := live.GroupVersionKind().GroupKind()
	if gk.Group == "" && gk.Kind == "Namespace" {
		return nil
	}
	var out []string
	for _, f := range []string{"spec", "data"} {
		if f == "data" && gk.Group == "" && gk.Kind == "Secret" {
			if t, _, _ := unstructured.NestedString(live.Object, "type"); t == "kubernetes.io/service-account-token" {
				continue
			}
		}
		if !emptyField(live.Object[f]) && emptyField(exported.Object[f]) {
			out = append(out, "."+f+": in the cluster but missing from the export, so a new cluster would get the object without it")
		}
	}
	return out
}

func emptyField(v any) bool {
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

// explainApplyError names known reasons an apply of an unchanged export is
// rejected. It matches the API server's message: the Invalid error carries
// no structured cause for it.
func explainApplyError(gvk schema.GroupVersionKind, err error) string {
	if gvk.Group == "" && gvk.Kind == "Pod" && apierrors.IsInvalid(err) && strings.Contains(err.Error(), "pod updates may not change") {
		return "Pods can't be updated in place; a bare Pod in Git is recreated, not applied"
	}
	return ""
}

// applyComparable drops what an apply always changes: field ownership and the
// resource version. generation is kept; it only moves when the spec does.
func applyComparable(obj map[string]any) map[string]any {
	c := (&unstructured.Unstructured{Object: obj}).DeepCopy().Object
	unstructured.RemoveNestedField(c, "metadata", "managedFields")
	unstructured.RemoveNestedField(c, "metadata", "resourceVersion")
	// An apply never changes status, which controllers keep changing (a Pod starting) between the read and the
	// dry-run.
	delete(c, "status")
	return c
}

// countRedacted counts the reference-only files under dir.
func countRedacted(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, kustomize.RedactedExt) {
			n++
		}
		return err
	})
	return n, err
}
