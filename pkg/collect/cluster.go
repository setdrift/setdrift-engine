// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Collecting from a Kubernetes API server (as opposed to a directory of
// manifests).

package collect

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/pager"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/ownership"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// The collector's defaults.
const (
	// DefaultPageSize is the number of objects requested per list call.
	DefaultPageSize = 500
	// DefaultHydrateGetLimit caps metadata-only objects individually fetched via GETs, above this just get the type as full list
	DefaultHydrateGetLimit = 100
	// DefaultConcurrency is how many types are listed at once.
	DefaultConcurrency = 8
	// DefaultOwnerGetLimit caps the GETs for controller owners missing from
	// the lists (see resolveOwners).
	DefaultOwnerGetLimit = 200
)

// runtimeState lists the types that hold runtime state rather than
// configuration, by resource (for the cluster collector, which lists by
// resource) and by kind (for FileSource, which reads objects without
// discovery): objects not to export, e.g. Events, too noisy and gone soon.
var runtimeState = []struct {
	group, resource, kind string
}{
	{"", "events", "Event"},
	{"events.k8s.io", "events", "Event"},
	// One per node, registered by the kubelet or a node provisioner.
	{"", "nodes", "Node"},
	{"storage.k8s.io", "csinodes", "CSINode"},
	{"storage.k8s.io", "volumeattachments", "VolumeAttachment"},
	{"storage.k8s.io", "csistoragecapacities", "CSIStorageCapacity"},
	{"resource.k8s.io", "resourceslices", "ResourceSlice"},
	{"karpenter.sh", "nodeclaims", "NodeClaim"},
	{"vpcresources.k8s.aws", "cninodes", "CNINode"},
	{"eks.amazonaws.com", "cninodes", "CNINode"},
	{"eks.amazonaws.com", "nodediagnostics", "NodeDiagnostic"},
	{"certificates.k8s.io", "certificatesigningrequests", "CertificateSigningRequest"},
	// Leader election and node heartbeats.
	{"coordination.k8s.io", "leases", "Lease"},
	// One PodMetrics per Pod and NodeMetrics per Node, computed live.
	{"metrics.k8s.io", "pods", "PodMetrics"},
	{"metrics.k8s.io", "nodes", "NodeMetrics"},
	// Allocated per Service IP.
	{"networking.k8s.io", "ipaddresses", "IPAddress"},
	// Deprecated, computed on read.
	{"", "componentstatuses", "ComponentStatus"},
}

var (
	skipResources = map[schema.GroupResource]bool{}
	skipKinds     = map[schema.GroupKind]bool{}
)

func init() {
	for _, t := range runtimeState {
		skipResources[schema.GroupResource{Group: t.group, Resource: t.resource}] = true
		skipKinds[schema.GroupKind{Group: t.group, Kind: t.kind}] = true
	}
}

// DefaultSkip reports whether the collector leaves out a type by default:
// one that holds runtime state rather than configuration.
func DefaultSkip(gr schema.GroupResource) bool { return skipResources[gr] }

// DefaultSkipKind is DefaultSkip by kind, for objects read without
// discovery.
func DefaultSkipKind(gk schema.GroupKind) bool { return skipKinds[gk] }

// metadataOnlyTypes are high-volume types whose objects are usually
// children of others: listed metadata-only first, and fetched in full only
// when nothing collected owns them.
var metadataOnlyTypes = map[schema.GroupResource]bool{
	{Group: "", Resource: "pods"}:                           true,
	{Group: "apps", Resource: "replicasets"}:                true,
	{Group: "apps", Resource: "controllerrevisions"}:        true,
	{Group: "batch", Resource: "jobs"}:                      true,
	{Group: "discovery.k8s.io", Resource: "endpointslices"}: true,
}

// DefaultMetadataOnly reports whether the collector lists a type
// metadata-only first, by default.
func DefaultMetadataOnly(gr schema.GroupResource) bool { return metadataOnlyTypes[gr] }

// ClusterCollector lists every listable resource in a cluster.
type ClusterCollector struct {
	// discovery finds types
	Discovery discovery.DiscoveryInterface
	// dynamic client reads any type as Unstructured
	Dynamic dynamic.Interface
	// enables metadata-only listing for MetadataOnly types
	Metadata metadata.Interface

	// limit scope to particular namespaces, defaults to all namespaces
	Namespaces []string
	// FallbackNamespace is exported instead of the whole cluster when
	// Namespaces is empty and the caller may not list Namespaces: their
	// access is limited to some namespaces. The CLI passes the kubeconfig
	// context's namespace. Empty means fail with a ScopeError instead.
	FallbackNamespace string
	// RequireNamespaces makes a requested (or fallback) namespace that does
	// not exist an error (a NamespaceError), found before anything is
	// listed; otherwise it is a NAMESPACE_NOT_FOUND finding. The CLI sets it.
	RequireNamespaces bool
	// objects per call; zero means DefaultPageSize
	PageSize int64
	// Skip reports the types not to list; nil means DefaultSkip.
	Skip func(schema.GroupResource) bool
	// MetadataOnly reports the types to list metadata-only first; nil means
	// DefaultMetadataOnly.
	MetadataOnly func(schema.GroupResource) bool
	// zero means DefaultHydrateGetLimit
	HydrateGetLimit int
	// Concurrency is how many types are listed at once. Zero means
	// DefaultConcurrency. The client's QPS and burst limits still apply.
	Concurrency int
	// OwnerGetLimit caps GETs for missing controller owners. Zero means
	// DefaultOwnerGetLimit; negative disables them.
	OwnerGetLimit int
}

// group metadata-only stubs by type for hydration
type partialType struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	objects    []*resource.Object
}

// typeInfo is a discovered type. listed is true once a list of it succeeded,
// so resolveOwners can tell "owner deleted" from "owner's type not collected".
type typeInfo struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	listed     bool
}

// listTask is one type to list.
type listTask struct {
	info         *typeInfo
	metadataOnly bool
}

// listResult is the outcome of one listTask.
type listResult struct {
	objects []*resource.Object
	partial *partialType
	err     error
	// denied are the namespaces that refused the list (Forbidden), when others allowed it
	denied []string
}

// Finding codes the CLI reads.
const (
	// ListDenied is the code of the finding naming the types a namespace
	// refused to list, when others allowed them.
	ListDenied = "LIST_DENIED"
	// NamespaceScopedAccess is the code of the finding on a capture that
	// read only the kubeconfig context's namespace.
	NamespaceScopedAccess = "NAMESPACE_SCOPED_ACCESS"
)

// Collect implements Collector. In order: the server version, discovery,
// what the credentials may read (scope), the lists (at most Concurrency at a
// time), then the requested Namespace objects, missing owners and the full
// objects behind metadata-only stubs that are kept.
func (c *ClusterCollector) Collect(ctx context.Context) (*Result, error) {
	res := &Result{}
	// an unknown version is a warning, not a failure
	if v, err := c.Discovery.ServerVersion(); err != nil {
		res.warn("SERVER_VERSION_UNKNOWN", err.Error())
	} else {
		res.KubernetesVersion = v.GitVersion
	}
	discovered, kinds, err := c.discover(res)
	if err != nil {
		return nil, err
	}
	// A caller who may only read some namespaces lists with a collector
	// scoped to them.
	sc, err := c.scope(ctx, res, kinds)
	if err != nil {
		return nil, err
	}
	tasks := sc.tasks(discovered)
	results, err := sc.listAll(ctx, tasks)
	if err != nil {
		return nil, err
	}
	partials, listed := sc.merge(ctx, res, tasks, results)
	// Nothing at all could be listed: an empty export would look like an empty cluster.
	if listed == 0 && len(tasks) > 0 {
		if IsPermission(results[0].err) {
			return nil, &NoAccessError{Identity: Identity(ctx, sc.Dynamic, results[0].err), Namespaces: sc.namespaces(), Err: results[0].err}
		}
		return nil, fmt.Errorf("no resource type could be listed; first error: %w", results[0].err)
	}
	if err := sc.getNamespaces(ctx, res, kinds); err != nil {
		return nil, err
	}
	if err := sc.resolveOwners(ctx, res, kinds); err != nil {
		return nil, err
	}
	if err := sc.hydrate(ctx, res, partials); err != nil {
		return nil, err
	}
	return res, nil
}

// discover returns every listable type the server serves, one version per
// API group, and the same by group and kind, for resolveOwners: owner
// references name a kind, not a resource.
func (c *ClusterCollector) discover(res *Result) ([]*typeInfo, map[schema.GroupKind]*typeInfo, error) {
	lists, err := discovery.ServerPreferredResources(c.Discovery)
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) || len(lists) == 0 {
			return nil, nil, fmt.Errorf("discovering API resources: %w", err)
		}
		// An aggregated API (e.g. metrics-server) may be registered but
		// unreachable: a warning, not a failure.
		res.warn("DISCOVERY_PARTIAL", err.Error())
	}
	kinds := map[schema.GroupKind]*typeInfo{}
	var discovered []*typeInfo
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			res.warn("DISCOVERY_BAD_GROUP_VERSION", err.Error())
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "get") {
				continue // subresource, or not listable e.g. SubjectAccessReview
			}
			if !plainKind(r.Kind) {
				// kinds name files of the export: an aggregated API server can serve any text
				res.warn("DISCOVERY_BAD_KIND", fmt.Sprintf("%s: resource %s has kind %q, not a plain name: not read", list.GroupVersion, r.Name, r.Kind))
				continue
			}
			info := &typeInfo{gvr: gv.WithResource(r.Name), kind: r.Kind, namespaced: r.Namespaced}
			kinds[schema.GroupKind{Group: gv.Group, Kind: r.Kind}] = info
			discovered = append(discovered, info)
		}
	}
	return discovered, kinds, nil
}

// plainKind reports whether kind is an ASCII letter followed by letters and
// digits, as every Kubernetes kind is.
func plainKind(kind string) bool {
	for i, r := range kind {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return kind != ""
}

// tasks returns the types to list, in a stable order: discovery order
// varies, and sorting keeps the result the same every run.
func (c *ClusterCollector) tasks(discovered []*typeInfo) []listTask {
	var tasks []listTask
	for _, info := range discovered {
		if c.skip(info.gvr.GroupResource()) {
			continue
		}
		// with namespaces given, cluster-scoped types are not read
		if !info.namespaced && len(c.Namespaces) > 0 {
			continue
		}
		tasks = append(tasks, listTask{info: info, metadataOnly: c.Metadata != nil && c.metadataOnly(info.gvr.GroupResource())})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].info.gvr.String() < tasks[j].info.gvr.String() })
	return tasks
}

// merge adds the lists' objects to res in task order, so the result does not
// depend on which list finished first, and records what could not be read.
// It returns the metadata-only types, and how many types were listed.
func (c *ClusterCollector) merge(ctx context.Context, res *Result, tasks []listTask, results []listResult) ([]*partialType, int) {
	var partials []*partialType
	listed := 0
	// types refused in some of the namespaces read, by namespace
	deniedIn := map[string][]string{}
	// what could not be read, for the scope
	var unread []*snapshotv1alpha1.Unread
	for i, r := range results {
		gvr := tasks[i].info.gvr
		// an error the API server returned for one type is a warning
		if r.err != nil {
			res.warn("LIST_FAILED", fmt.Sprintf("%s: %v", gvr, r.err))
			unread = append(unread, &snapshotv1alpha1.Unread{Group: gvr.Group, Kind: tasks[i].info.kind})
			continue
		}
		tasks[i].info.listed = true
		listed++
		for _, ns := range r.denied {
			deniedIn[ns] = append(deniedIn[ns], gvr.GroupResource().String())
			unread = append(unread, &snapshotv1alpha1.Unread{Namespace: ns, Group: gvr.Group, Kind: tasks[i].info.kind})
		}
		if r.partial != nil {
			partials = append(partials, r.partial)
			res.Objects = append(res.Objects, r.partial.objects...)
		} else {
			res.Objects = append(res.Objects, r.objects...)
		}
	}
	if res.Scope != nil {
		res.Scope.Unread = unread
	}
	c.warnDenied(ctx, res, deniedIn)
	return partials, listed
}

// warnDenied adds a LIST_DENIED finding for each namespace that refused
// some types.
func (c *ClusterCollector) warnDenied(ctx context.Context, res *Result, deniedIn map[string][]string) {
	if len(deniedIn) == 0 {
		return
	}
	identity := ""
	if res.Scope != nil {
		identity = res.Scope.Identity
	}
	if identity == "" {
		identity = Identity(ctx, c.Dynamic)
		if res.Scope != nil {
			res.Scope.Identity = identity
		}
	}
	who := "these credentials"
	if identity != "" {
		who += " (" + identity + ")"
	}
	nss := make([]string, 0, len(deniedIn))
	for ns := range deniedIn {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		res.warn(ListDenied, fmt.Sprintf("in namespace %s, %s may not list %s, so they were not read there: %s",
			ns, who, text.Plural(len(deniedIn[ns]), "type", "types"), truncateList(deniedIn[ns], 12)))
	}
}

// hydrate fetches in full the metadata-only objects the ownership stage
// will keep (no controller owner, or one not collected). It must decide as
// ownership.ControllerOwnerResolver does, or snapshot.Build rejects a stub
// left metadata-only.
func (c *ClusterCollector) hydrate(ctx context.Context, res *Result, partials []*partialType) error {
	if len(partials) == 0 {
		return nil
	}
	limit := c.HydrateGetLimit
	if limit == 0 {
		limit = DefaultHydrateGetLimit
	}

	gone := map[*resource.Object]bool{}
	// unreadable marks gone objects that could not be fetched, as opposed to deleted ones, their children are kept, not dropped as garbage
	unreadable := map[*resource.Object]bool{}
	tried := map[*resource.Object]bool{}
	for {
		// recomputed every round: objects removed last round no longer count as owners
		present := make(map[types.UID]bool, len(res.Objects))
		goneStatus := map[types.UID]resource.OwnerStatus{}
		for _, o := range res.Objects {
			switch {
			case !gone[o]:
				present[o.GetUID()] = true
			case unreadable[o]:
				goneStatus[o.GetUID()] = resource.OwnerNotCollected
			default:
				goneStatus[o.GetUID()] = resource.OwnerDeleted
			}
		}

		fetched := false
		for _, pt := range partials {
			need := toFetch(pt, gone, tried, present, goneStatus)
			if len(need) == 0 {
				continue
			}
			fetched = true

			var err error
			if limit > 0 && len(need) <= limit {
				// hydrateByGet individually retrieves any partials full manifests so long as the count is below the limit
				err = c.hydrateByGet(ctx, pt, need, gone)
			} else {
				// hydrateByList pulls the manifests for the whole type when the needed partials exceed the limit
				err = c.hydrateByList(ctx, pt, need, gone)
			}
			if err != nil {
				if kube.IsObjectError(err) {
					// warning, not breaking error when stubs cant be fully listed, marks as gone;
					// their children are fetched next round
					res.warn("HYDRATE_FAILED", fmt.Sprintf("%s: %v", pt.gvr, err))
					for _, o := range need {
						gone[o] = true
						unreadable[o] = true
					}
					continue
				}
				return fmt.Errorf("fetching %s: %w", pt.gvr, err)
			}
		}
		if !fetched {
			break
		}
	}
	// remove stubs labelled gone
	if len(gone) > 0 {
		res.Objects = slices.DeleteFunc(res.Objects, func(o *resource.Object) bool { return gone[o] })
	}
	return nil
}

// toFetch returns pt's stubs to fetch in full this round, marking them
// tried: those the ownership stage will keep, and the children of objects
// that went away this run, which may have gone too (cascading deletion) or
// still exist as garbage the ownership stage drops.
func toFetch(pt *partialType, gone, tried map[*resource.Object]bool, present map[types.UID]bool, goneStatus map[types.UID]resource.OwnerStatus) map[types.UID]*resource.Object {
	need := map[types.UID]*resource.Object{}
	for _, o := range pt.objects {
		if gone[o] || tried[o] {
			continue
		}
		ownerGone := false
		if owner := metav1.GetControllerOfNoCopy(o); owner != nil {
			var st resource.OwnerStatus
			if st, ownerGone = goneStatus[owner.UID]; ownerGone {
				o.OwnerStatus = st
			}
		}
		// otherwise the same rule the ownership stage drops by
		if ownerGone || !ownership.IsChild(o, present) {
			need[o.GetUID()] = o
			tried[o] = true
		}
	}
	return need
}

// hydrateByGet fetches each stub by name.
func (c *ClusterCollector) hydrateByGet(ctx context.Context, pt *partialType, need map[types.UID]*resource.Object, gone map[*resource.Object]bool) error {
	for _, o := range need {
		u, err := c.Dynamic.Resource(pt.gvr).Namespace(o.GetNamespace()).Get(ctx, o.GetName(), metav1.GetOptions{})
		switch {
		// if not found, its gone, deleted since list operation
		case apierrors.IsNotFound(err):
			gone[o] = true
		case err != nil:
			return err
		case u.GetUID() != o.GetUID():
			// Deleted and recreated since the list; the new object was not
			// part of this capture.
			gone[o] = true
		default:
			setFull(o, u, pt)
		}
	}
	return nil
}

// hydrateByList lists the whole type in full, filling in the stubs in need
// by UID; a stub not in the list is gone.
func (c *ClusterCollector) hydrateByList(ctx context.Context, pt *partialType, need map[types.UID]*resource.Object, gone map[*resource.Object]bool) error {
	seen := map[types.UID]bool{}
	_, err := c.listFull(ctx, pt.gvr, pt.kind, pt.namespaced, func(u *unstructured.Unstructured) bool {
		if o, ok := need[u.GetUID()]; ok {
			setFull(o, u, pt)
			seen[u.GetUID()] = true
		}
		return false // nothing to return, setFull updates the stubs
	}, func(string) {}) // a namespace that refused the list had no stubs to fill
	if err != nil {
		return err
	}
	for uid, o := range need {
		if !seen[uid] {
			gone[o] = true
		}
	}
	return nil
}

// setFull replaces the stub o with its full object u
func setFull(o *resource.Object, u *unstructured.Unstructured, pt *partialType) {
	u.SetAPIVersion(pt.gvr.GroupVersion().String())
	u.SetKind(pt.kind)
	o.Unstructured = u
	o.Partial = false
}

// listFull is a paged list through the dynamic client; keep, when set,
// filters the items (hydrateByList).
func (c *ClusterCollector) listFull(ctx context.Context, gvr schema.GroupVersionResource, kind string, namespaced bool, keep func(*unstructured.Unstructured) bool, denied func(ns string)) ([]*resource.Object, error) {
	var out []*resource.Object
	err := c.paginate(ctx, namespaced, metav1.ListOptions{}, func(ns string) pager.ListPageFunc {
		return func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, opts)
		}
	}, func(obj runtime.Object) error {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("unexpected list item type %T", obj)
		}
		if keep != nil && !keep(u) {
			return nil
		}
		// Manually set api version and kind for each item, some list items lack them
		u.SetAPIVersion(gvr.GroupVersion().String())
		u.SetKind(kind)
		out = append(out, resource.New(u))
		return nil
	}, denied)
	return out, err
}

// listMetadata lists pt.gvr metadata-only, into partial objects
func (c *ClusterCollector) listMetadata(ctx context.Context, pt *partialType, denied func(ns string)) error {
	err := c.eachMetadata(ctx, pt.gvr, pt.namespaced, metav1.ListOptions{}, func(pom *metav1.PartialObjectMetadata) error {
		// turn partial object metadata into a stub Unstructured, containing only metadata and Partial = true
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pom.ObjectMeta)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{Object: map[string]any{"metadata": m}}
		u.SetAPIVersion(pt.gvr.GroupVersion().String())
		u.SetKind(pt.kind)
		o := resource.New(u)
		o.Partial = true
		pt.objects = append(pt.objects, o)
		return nil
	}, denied)
	if err != nil || pt.gvr.GroupResource() != podsResource {
		return err
	}
	return c.tagPodPhases(ctx, pt, pt.namespaced)
}

var podsResource = schema.GroupResource{Resource: "pods"}

// tagPodPhases sets status.phase on terminated Pod stubs: the metadata API
// returns no status, so it lists Succeeded and Failed Pods by field selector
// (for TERMINATED_PODS_TO_CLEANUP).
func (c *ClusterCollector) tagPodPhases(ctx context.Context, pt *partialType, namespaced bool) error {
	byUID := make(map[types.UID]*resource.Object, len(pt.objects))
	for _, o := range pt.objects {
		byUID[o.GetUID()] = o
	}
	for _, phase := range []string{"Succeeded", "Failed"} {
		// attach phase status to the stub by uid
		opts := metav1.ListOptions{FieldSelector: "status.phase=" + phase}
		err := c.eachMetadata(ctx, pt.gvr, namespaced, opts, func(pom *metav1.PartialObjectMetadata) error {
			if o, ok := byUID[pom.UID]; ok {
				_ = unstructured.SetNestedField(o.Object, phase, "status", "phase") // cannot fail: a stub has no status yet
			}
			return nil
		}, func(string) {})
		if err != nil {
			return err
		}
	}
	return nil
}

// eachMetadata is listFull's paging, metadata-only.
func (c *ClusterCollector) eachMetadata(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, opts metav1.ListOptions, fn func(*metav1.PartialObjectMetadata) error, denied func(ns string)) error {
	return c.paginate(ctx, namespaced, opts, func(ns string) pager.ListPageFunc {
		return func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return c.Metadata.Resource(gvr).Namespace(ns).List(ctx, opts)
		}
	}, func(obj runtime.Object) error {
		pom, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok {
			return fmt.Errorf("unexpected list item type %T", obj)
		}
		return fn(pom)
	}, denied)
}

// paginate wraps client-go's pager: once per namespace for namespaced types
// when namespaces are set, otherwise once for all namespaces.
// Listing several namespaces, a namespace that refuses the type (Forbidden) is skipped and passed to denied, when set:
// a role can allow ConfigMaps in one namespace and not another. The type fails only if every namespace refuses it.
func (c *ClusterCollector) paginate(ctx context.Context, namespaced bool, opts metav1.ListOptions, page func(ns string) pager.ListPageFunc, fn func(runtime.Object) error, denied func(ns string)) error {
	namespaces := []string{metav1.NamespaceAll}
	if namespaced && len(c.Namespaces) > 0 {
		namespaces = c.namespaces()
	}
	var refused []string
	var first error
	for _, ns := range namespaces {
		p := pager.New(page(ns))
		p.PageSize = c.PageSize
		if p.PageSize == 0 {
			p.PageSize = DefaultPageSize
		}
		if err := p.EachListItem(ctx, opts, fn); err != nil {
			if denied == nil || len(namespaces) == 1 || !apierrors.IsForbidden(err) {
				return err
			}
			refused = append(refused, ns)
			if first == nil {
				first = err
			}
		}
	}
	if len(refused) == len(namespaces) {
		return first
	}
	for _, ns := range refused {
		denied(ns)
	}
	return nil
}

// skip uses c.Skip, which replaces DefaultSkip rather than adding to it.
func (c *ClusterCollector) skip(gr schema.GroupResource) bool {
	if c.Skip != nil {
		return c.Skip(gr)
	}
	return DefaultSkip(gr)
}

// metadataOnly uses c.MetadataOnly, which replaces DefaultMetadataOnly.
func (c *ClusterCollector) metadataOnly(gr schema.GroupResource) bool {
	if c.MetadataOnly != nil {
		return c.MetadataOnly(gr)
	}
	return DefaultMetadataOnly(gr)
}

// namespaces returns c.Namespaces without duplicates, in the order given.
func (c *ClusterCollector) namespaces() []string { return dedupe(c.Namespaces) }

// listAll runs the list tasks, at most c.Concurrency at a time. A result's
// err is set only for an error about that type (kube.IsObjectError); any
// other error stops the rest, and the first is returned.
func (c *ClusterCollector) listAll(ctx context.Context, tasks []listTask) ([]listResult, error) {
	n := c.Concurrency
	if n <= 0 {
		n = DefaultConcurrency
	}
	results := make([]listResult, len(tasks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(n)
	for i := range tasks {
		g.Go(func() error {
			results[i] = c.listOne(gctx, tasks[i])
			if err := results[i].err; err != nil && !kube.IsObjectError(err) {
				return fmt.Errorf("listing %s: %w", tasks[i].info.gvr, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// listOne lists one type, metadata-only or in full.
func (c *ClusterCollector) listOne(ctx context.Context, t listTask) listResult {
	var denied []string
	deny := func(ns string) { denied = append(denied, ns) }
	if t.metadataOnly {
		pt := &partialType{gvr: t.info.gvr, kind: t.info.kind, namespaced: t.info.namespaced}
		if err := c.listMetadata(ctx, pt, deny); err != nil {
			return listResult{err: err}
		}
		return listResult{partial: pt, denied: denied}
	}
	objs, err := c.listFull(ctx, t.info.gvr, t.info.kind, t.info.namespaced, nil, deny)
	return listResult{objects: objs, err: err, denied: denied}
}

// getNamespaces adds the requested Namespace objects themselves.
func (c *ClusterCollector) getNamespaces(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) error {
	info := kinds[schema.GroupKind{Kind: "Namespace"}]
	if len(c.Namespaces) == 0 || info == nil || c.skip(info.gvr.GroupResource()) {
		return nil
	}
	for _, ns := range c.namespaces() {
		u, err := c.Dynamic.Resource(info.gvr).Get(ctx, ns, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			res.warn("NAMESPACE_NOT_FOUND", fmt.Sprintf("requested namespace %q does not exist", ns))
		case err != nil && kube.IsObjectError(err):
			res.warn("NAMESPACE_GET_FAILED", fmt.Sprintf("namespace %q: %v", ns, err))
		case err != nil:
			return fmt.Errorf("getting namespace %s: %w", ns, err)
		default:
			u.SetAPIVersion(info.gvr.GroupVersion().String())
			u.SetKind(info.kind)
			res.Objects = append(res.Objects, resource.New(u))
		}
	}
	return nil
}

// resolveOwners looks up controller owners missing from the lists. Added owners can have missing owners too, so it repeats until nothing is
// added.
func (c *ClusterCollector) resolveOwners(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) error {
	limit := c.OwnerGetLimit
	if limit == 0 {
		limit = DefaultOwnerGetLimit
	}
	present := make(map[types.UID]bool, len(res.Objects))
	for _, o := range res.Objects {
		present[o.GetUID()] = true
	}
	examined := 0 // res.Objects before this index have been examined
	gets := 0
	for examined < len(res.Objects) {
		type missing struct {
			ref      metav1.OwnerReference
			info     *typeInfo
			ns       string
			children []*resource.Object
		}
		byOwner := map[types.UID]*missing{}
		for _, o := range res.Objects[examined:] {
			if ownership.IsChild(o, present) {
				continue
			}
			owner := metav1.GetControllerOfNoCopy(o)
			if owner == nil {
				continue
			}
			gv, err := schema.ParseGroupVersion(owner.APIVersion)
			info := kinds[schema.GroupKind{Group: gv.Group, Kind: owner.Kind}]
			if err != nil || info == nil || !info.listed {
				o.OwnerStatus = resource.OwnerNotCollected
				continue
			}
			m := byOwner[owner.UID]
			if m == nil {
				ns := ""
				if info.namespaced {
					ns = o.GetNamespace()
				}
				m = &missing{ref: *owner, info: info, ns: ns}
				byOwner[owner.UID] = m
			}
			m.children = append(m.children, o)
		}
		examined = len(res.Objects)

		// Sorted, so the GET limit always picks the same owners.
		uids := make([]types.UID, 0, len(byOwner))
		for uid := range byOwner {
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		for _, uid := range uids {
			m := byOwner[uid]
			if limit < 0 || gets >= limit {
				break // left OwnerUnknown
			}
			gets++
			u, err := c.Dynamic.Resource(m.info.gvr).Namespace(m.ns).Get(ctx, m.ref.Name, metav1.GetOptions{})
			status := resource.OwnerUnknown
			switch {
			case apierrors.IsNotFound(err):
				status = resource.OwnerDeleted
			case err != nil && kube.IsObjectError(err):
				res.warn("OWNER_GET_FAILED", fmt.Sprintf("%s %s: %v", m.info.gvr, m.ref.Name, err))
			case err != nil:
				return fmt.Errorf("getting %s %s: %w", m.info.gvr, m.ref.Name, err)
			case u.GetUID() != uid:
				status = resource.OwnerDeleted
			default:
				u.SetAPIVersion(m.info.gvr.GroupVersion().String())
				u.SetKind(m.info.kind)
				res.Objects = append(res.Objects, resource.New(u))
				present[uid] = true
			}
			for _, o := range m.children {
				o.OwnerStatus = status
			}
		}
	}
	return nil
}

// ScopeError means the caller may not list the cluster as a whole (listing
// namespaces is forbidden), no namespaces were requested, and there is no
// fallback namespace.
type ScopeError struct {
	Err error
	// Identity is who the cluster says the credentials are, when known.
	Identity string
}

func (e *ScopeError) Error() string {
	return "credentials can't list resources across the cluster (listing namespaces is forbidden); namespaces to export must be given: " + e.Err.Error()
}

func (e *ScopeError) Unwrap() error { return e.Err }

// scope returns the collector to list with, and records in res what it reads:
//   - namespaces asked for must exist and be readable (a NamespaceError);
//   - with none asked for, credentials that may not list namespaces read the
//     fallback (the context's namespace), which must be readable, or get a
//     ScopeError;
//   - credentials that may list namespaces but not read across them read the
//     namespaces they can, and the others are recorded as unreadable.
func (c *ClusterCollector) scope(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) (*ClusterCollector, error) {
	info := kinds[schema.GroupKind{Kind: "Namespace"}]
	if len(c.Namespaces) > 0 {
		if err := CheckNamespaces(ctx, c.Dynamic, c.Namespaces, false, c.RequireNamespaces); err != nil {
			return nil, err
		}
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeNamespaces, Namespaces: c.namespaces()}
		return c, nil
	}
	if info == nil {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	// Metadata and full lists need the same list permission; the metadata one is cheaper.
	var err error
	if c.Metadata != nil {
		_, err = c.Metadata.Resource(info.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	} else {
		_, err = c.Dynamic.Resource(info.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	}
	switch {
	case apierrors.IsForbidden(err):
		if c.FallbackNamespace == "" {
			return nil, &ScopeError{Err: err, Identity: Identity(ctx, c.Dynamic, err)}
		}
		if err := CheckNamespaces(ctx, c.Dynamic, []string{c.FallbackNamespace}, true, c.RequireNamespaces); err != nil {
			return nil, err
		}
		scoped := *c
		scoped.Namespaces = []string{c.FallbackNamespace}
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeContextNamespace, Namespaces: scoped.Namespaces, Identity: Identity(ctx, c.Dynamic, err)}
		res.warn(NamespaceScopedAccess, fmt.Sprintf(
			"credentials can't list resources across the cluster (listing namespaces is forbidden), so only namespace %q was exported; %s",
			c.FallbackNamespace, c.accessHint(ctx, c.FallbackNamespace)))
		return &scoped, nil
	case err != nil:
		// other errors surface in the lists themselves
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	if CanReadCluster(ctx, c.Dynamic) {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	// Namespaces can be listed, but not read across: read the ones that can be.
	readable, unreadable, err := readableNamespaces(ctx, c.Dynamic)
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	identity := Identity(ctx, c.Dynamic)
	if len(readable) == 0 {
		return nil, &ReadNothingError{Identity: identity, Namespaces: len(unreadable)}
	}
	scoped := *c
	scoped.Namespaces = readable
	res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeReadableNamespaces, Namespaces: readable, Unreadable: unreadable, Identity: identity}
	res.warn("NAMESPACE_READABLE_ONLY", fmt.Sprintf(
		"credentials can't read resources across the cluster, so the %d namespaces they can read were exported (%s), and cluster-scoped resources were not; "+
			"not readable with them: %s", len(readable), truncateList(readable, 10), truncateList(unreadable, 10)))
	return &scoped, nil
}

// maxHintTypes caps the types an access hint names.
const maxHintTypes = 8

var rulesReviewGVR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectrulesreviews"}

// accessHint says what the caller may list in ns, from a SelfSubjectRulesReview (the API behind kubectl auth can-i --list).
func (c *ClusterCollector) accessHint(ctx context.Context, ns string) string {
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectRulesReview",
		"spec":       map[string]any{"namespace": ns},
	}}
	out, err := c.Dynamic.Resource(rulesReviewGVR).Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "what they can read in it is unknown (" + err.Error() + ")"
	}
	rules, _, _ := unstructured.NestedSlice(out.Object, "status", "resourceRules")
	seen := map[string]bool{}
	for _, r := range rules {
		m, _ := r.(map[string]any)
		verbs, _, _ := unstructured.NestedStringSlice(m, "verbs")
		if !slices.Contains(verbs, "list") && !slices.Contains(verbs, "*") {
			continue
		}
		groups, _, _ := unstructured.NestedStringSlice(m, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(m, "resources")
		for _, g := range groups {
			for _, r := range resources {
				if r == "*" && g == "*" {
					return "in it they can list every type"
				}
				if strings.Contains(r, "/") {
					continue // subresource
				}
				if g != "" {
					r += "." + g
				}
				seen[r] = true
			}
		}
	}
	if len(seen) == 0 {
		return "they can't list anything in it either; check the namespace"
	}
	listable := slices.Sorted(maps.Keys(seen))
	more := ""
	if len(listable) > maxHintTypes {
		more = fmt.Sprintf(" and %d more", len(listable)-maxHintTypes)
		listable = listable[:maxHintTypes]
	}
	return "in it they can list " + strings.Join(listable, ", ") + more
}
