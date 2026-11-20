// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Collecting from a Kubernetes API server (as opposed to a directory of manifests).

package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

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

	"setdrift.io/setdrift-engine/pkg/ownership"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// set defaults
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

// DefaultSkip lists types that hold runtime state rather than configuration: objects to ignore, e.g. events are too noisy
var DefaultSkip = map[schema.GroupResource]bool{
	{Group: "", Resource: "events"}:              true,
	{Group: "events.k8s.io", Resource: "events"}: true,
	// One per node, registered by the kubelet or a node provisioner.
	{Group: "", Resource: "nodes"}:                                         true,
	{Group: "storage.k8s.io", Resource: "csinodes"}:                        true,
	{Group: "storage.k8s.io", Resource: "volumeattachments"}:               true,
	{Group: "storage.k8s.io", Resource: "csistoragecapacities"}:            true,
	{Group: "resource.k8s.io", Resource: "resourceslices"}:                 true,
	{Group: "karpenter.sh", Resource: "nodeclaims"}:                        true,
	{Group: "vpcresources.k8s.aws", Resource: "cninodes"}:                  true,
	{Group: "eks.amazonaws.com", Resource: "cninodes"}:                     true,
	{Group: "eks.amazonaws.com", Resource: "nodediagnostics"}:              true,
	{Group: "certificates.k8s.io", Resource: "certificatesigningrequests"}: true,
	// Leader election and node heartbeats.
	{Group: "coordination.k8s.io", Resource: "leases"}: true,
	// One PodMetrics per Pod and NodeMetrics per Node, computed live.
	{Group: "metrics.k8s.io", Resource: "pods"}:  true,
	{Group: "metrics.k8s.io", Resource: "nodes"}: true,
	// Allocated per Service IP.
	{Group: "networking.k8s.io", Resource: "ipaddresses"}: true,
	// Deprecated, computed on read.
	{Group: "", Resource: "componentstatuses"}: true,
}

// DefaultSkipKinds is DefaultSkip by kind, for FileSource, which reads objects without discovery. Keep the two in step.
var DefaultSkipKinds = map[schema.GroupKind]bool{
	{Group: "", Kind: "Event"}:                                        true,
	{Group: "events.k8s.io", Kind: "Event"}:                           true,
	{Group: "", Kind: "Node"}:                                         true,
	{Group: "storage.k8s.io", Kind: "CSINode"}:                        true,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:               true,
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"}:             true,
	{Group: "resource.k8s.io", Kind: "ResourceSlice"}:                 true,
	{Group: "karpenter.sh", Kind: "NodeClaim"}:                        true,
	{Group: "vpcresources.k8s.aws", Kind: "CNINode"}:                  true,
	{Group: "eks.amazonaws.com", Kind: "CNINode"}:                     true,
	{Group: "eks.amazonaws.com", Kind: "NodeDiagnostic"}:              true,
	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}: true,
	{Group: "coordination.k8s.io", Kind: "Lease"}:                     true,
	{Group: "metrics.k8s.io", Kind: "PodMetrics"}:                     true,
	{Group: "metrics.k8s.io", Kind: "NodeMetrics"}:                    true,
	{Group: "networking.k8s.io", Kind: "IPAddress"}:                   true,
	{Group: "", Kind: "ComponentStatus"}:                              true,
}

// DefaultMetadataOnly lists high volume objects, usually children to other resources, only retrieve metadata to cut down on size and drops if child
var DefaultMetadataOnly = map[schema.GroupResource]bool{
	{Group: "", Resource: "pods"}:                           true,
	{Group: "apps", Resource: "replicasets"}:                true,
	{Group: "apps", Resource: "controllerrevisions"}:        true,
	{Group: "batch", Resource: "jobs"}:                      true,
	{Group: "discovery.k8s.io", Resource: "endpointslices"}: true,
}

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
	// types not to list; nil means DefaultSkip
	Skip func(schema.GroupResource) bool
	// resources to list metadata-only first; nil means DefaultMetadataOnly
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

// Collect implements Collector, gathers in order- Server Version, Discovery, Kinds by group, Scope, Tasks, Sort, listAll, merge in task order, fail if list empty, get ns resolve owners then hydrate
func (c *ClusterCollector) Collect(ctx context.Context) (*Result, error) {
	res := &Result{}

	// unknown k8s server version is warning not breaking error
	if v, err := c.Discovery.ServerVersion(); err != nil {
		res.warn("SERVER_VERSION_UNKNOWN", err.Error())
	} else {
		res.KubernetesVersion = v.GitVersion
	}

	// returns every type, one version per API group pulled from server
	lists, err := discovery.ServerPreferredResources(c.Discovery)
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) || len(lists) == 0 {
			return nil, fmt.Errorf("discovering API resources: %w", err)
		}
		// aggregated apis (e.g. metrics-server) may have registered api service that is unreachable
		// results in warning not terminal error
		res.warn("DISCOVERY_PARTIAL", err.Error())
	}

	// One entry per group and kind, for resolveOwners: owner references name a kind, not a resource.
	kinds := map[schema.GroupKind]*typeInfo{}
	var discovered []*typeInfo
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			res.warn("DISCOVERY_BAD_GROUP_VERSION", err.Error())
			continue
		}
		// iterates through each api resource as defined by server
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "get") {
				continue // subresource, or not listable e.g. SubjectAccessReview
			}
			info := &typeInfo{gvr: gv.WithResource(r.Name), kind: r.Kind, namespaced: r.Namespaced}
			kinds[schema.GroupKind{Group: gv.Group, Kind: r.Kind}] = info
			discovered = append(discovered, info)
		}
	}

	// A caller who may only read some namespaces gets the fallback namespace.
	c, err = c.scope(ctx, res, kinds)
	if err != nil {
		return nil, err
	}

	var tasks []listTask
	for _, info := range discovered {
		// apply skip rule
		if c.skip(info.gvr.GroupResource()) {
			continue
		}
		// apply namespace rule
		if !info.namespaced && len(c.Namespaces) > 0 {
			continue
		}
		// only pull metadata for types specified in metadata only above
		tasks = append(tasks, listTask{info: info, metadataOnly: c.Metadata != nil && c.metadataOnly(info.gvr.GroupResource())})
	}

	// Discovery order varies; sorting keeps the result the same every run.
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].info.gvr.String() < tasks[j].info.gvr.String() })
	results, err := c.listAll(ctx, tasks)
	if err != nil {
		return nil, err
	}
	// Merged in task order, so the result does not depend on which list finished first.
	var partials []*partialType
	listed := 0
	// types refused in some of the namespaces read, by namespace
	deniedIn := map[string][]string{}
	// what could not be read, for the scope
	var unread []*snapshotv1alpha1.Unread
	defer func() {
		if res.Scope != nil {
			res.Scope.Unread = unread
		}
	}()
	for i, r := range results {
		gvr := tasks[i].info.gvr
		// LIST_FAILED warning for any error the API server returned for one type, else breaks with error
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

	if len(deniedIn) > 0 {
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
			res.warn("LIST_DENIED", fmt.Sprintf("in namespace %s, %s may not list %d %s, so they were not read there: %s",
				ns, who, len(deniedIn[ns]), choose(len(deniedIn[ns]) == 1, "type", "types"), truncateList(deniedIn[ns], 12)))
		}
	}

	// Nothing at all could be listed: an empty export would look like an empty cluster.
	if listed == 0 && len(tasks) > 0 {
		if IsPermission(results[0].err) {
			return nil, &NoAccessError{Identity: Identity(ctx, c.Dynamic, results[0].err), Namespaces: c.namespaces(), Err: results[0].err}
		}
		return nil, fmt.Errorf("no resource type could be listed; first error: %v", results[0].err)
	}

	if err := c.getNamespaces(ctx, res, kinds); err != nil {
		return nil, err
	}
	if err := c.resolveOwners(ctx, res, kinds); err != nil {
		return nil, err
	}
	if err := c.hydrate(ctx, res, partials); err != nil {
		return nil, err
	}
	return res, nil
}

// fetch full object for meta-data only objects that ownership stage will keep (no controller owner, controller owner unknown)
// must match logic in ownership.ControllerOwnerResolver otherwise snapshot.Build will reject it
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
			need := map[types.UID]*resource.Object{}
			for _, o := range pt.objects {
				if gone[o] || tried[o] {
					continue
				}
				// Children of an object that went away this run are fetched either way, they may have gone too (cascading deletion), or still exist as garbage the ownership stage drops.
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
				if tolerable(err) {
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

// GET request for each stub by name
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

// pull full manifests by type, only keeps stubs matching desired uid, else discard, uses setFull instead of Unstructured for the full object
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

// listFull is a paged list through the dynamic client, optional keep filter for hydrateByList
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

// tagPodPhases: the metadata API doesn't return status, so this passes through to see Succeeded and Failed status.phase values enabling TERMINATED_PODS_TO_CLEANUP
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
				_ = unstructured.SetNestedField(o.Object, phase, "status", "phase")
			}
			return nil
		}, func(string) {})
		if err != nil {
			return err
		}
	}
	return nil
}

// metadata equivalent of listFull's paging
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

// wrapper for client-go's pager, runs once per set namespace for namespaced objects, otherwise once for all namespaces.
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

// skip uses c.Skip, which replaces DefaultSkip rather than adding to it
func (c *ClusterCollector) skip(gr schema.GroupResource) bool {
	if c.Skip != nil {
		return c.Skip(gr)
	}
	return DefaultSkip[gr]
}

// metadataOnly uses c.MetadataOnly, which replaces DefaultMetadataOnly
func (c *ClusterCollector) metadataOnly(gr schema.GroupResource) bool {
	if c.MetadataOnly != nil {
		return c.MetadataOnly(gr)
	}
	return DefaultMetadataOnly[gr]
}

// tolerable defines errors that are non-terminal for the Collector, stores as warning instead
func tolerable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || apierrors.IsUnauthorized(err) {
		return false
	}
	var status apierrors.APIStatus
	return errors.As(err, &status)
}

// namespaces returns c.Namespaces without duplicates, in the order given.
func (c *ClusterCollector) namespaces() []string {
	var out []string
	for _, ns := range c.Namespaces {
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// listAll runs the list tasks, at most c.Concurrency at a time. A result's err is set only for tolerable errors, any other error cancels the rest and is returned.
func (c *ClusterCollector) listAll(ctx context.Context, tasks []listTask) ([]listResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	n := c.Concurrency
	if n <= 0 {
		n = DefaultConcurrency
	}
	results := make([]listResult, len(tasks))
	sem := make(chan struct{}, n)
	var (
		wg    sync.WaitGroup
		once  sync.Once
		fatal error
	)
	for i := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			results[i] = c.listOne(ctx, tasks[i])
			if err := results[i].err; err != nil && !tolerable(err) {
				once.Do(func() {
					fatal = fmt.Errorf("listing %s: %w", tasks[i].info.gvr, err)
					cancel()
				})
			}
		}()
	}
	wg.Wait()
	if fatal != nil {
		return nil, fatal
	}
	return results, nil
}

// listOne lists one type, metadata-only or in full
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
	// full list for every type not defined as metadata only
	objs, err := c.listFull(ctx, t.info.gvr, t.info.kind, t.info.namespaced, nil, deny)
	return listResult{objects: objs, err: err, denied: denied}
}

// getNamespaces adds the Namespace objects themselves when requested
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
		case err != nil && tolerable(err):
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
			case err != nil && tolerable(err):
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

// ScopeError means the caller may not list the cluster as a whole (listing namespaces is forbidden), no namespaces were requested, and there is no fallback namespace.
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
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: "namespaces", Namespaces: c.namespaces()}
		return c, nil
	}
	if info == nil {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: "cluster"}
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
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: "context-namespace", Namespaces: scoped.Namespaces, Identity: Identity(ctx, c.Dynamic, err)}
		res.warn("NAMESPACE_SCOPED_ACCESS", fmt.Sprintf(
			"credentials can't list resources across the cluster (listing namespaces is forbidden), so only namespace %q was exported; %s",
			c.FallbackNamespace, c.accessHint(ctx, c.FallbackNamespace)))
		return &scoped, nil
	case err != nil:
		// other errors surface in the lists themselves
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: "cluster"}
		return c, nil
	}
	if CanReadCluster(ctx, c.Dynamic) {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: "cluster"}
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
	res.Scope = &snapshotv1alpha1.ReadScope{Mode: "readable-namespaces", Namespaces: readable, Unreadable: unreadable, Identity: identity}
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
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	more := ""
	if len(types) > maxHintTypes {
		more = fmt.Sprintf(" and %d more", len(types)-maxHintTypes)
		types = types[:maxHintTypes]
	}
	return "in it they can list " + strings.Join(types, ", ") + more
}
