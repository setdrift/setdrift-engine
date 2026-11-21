// collects from a live kubernetes API server, as opposed to a directory of manifests which files.go reads
// it discovers the types, works out what the credentials may read, lists everything and fetches what the export needs in full

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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/pager"

	"setdrift.io/setdrift-engine/internal/discover"
	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/ownership"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// the collector's defaults, used when the matching ClusterCollector field is zero
const (
	// DefaultPageSize is how many objects each list call asks for
	DefaultPageSize = 500
	// DefaultHydrateGetLimit caps the metadata-only objects fetched one GET at a time, above it every object of the type is listed in full instead
	DefaultHydrateGetLimit = 100
	// DefaultConcurrency is how many types are listed at once
	DefaultConcurrency = 8
	// DefaultOwnerGetLimit caps the GETs for controller owners missing from the lists, see resolveOwners
	DefaultOwnerGetLimit = 200
)

// runtimeState lists the types that hold runtime state rather than configuration, objects not to export, like Events which are noisy and gone soon
// each is given by resource for the cluster collector, which lists by resource, and by kind for FileSource, which reads objects without discovery
var runtimeState = []struct {
	group, resource, kind string
}{
	{"", "events", "Event"},
	{"events.k8s.io", "events", "Event"},
	// one per node, registered by the kubelet or a node provisioner
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
	// leader election and node heartbeats
	{"coordination.k8s.io", "leases", "Lease"},
	// one PodMetrics per Pod and NodeMetrics per Node, computed live
	{"metrics.k8s.io", "pods", "PodMetrics"},
	{"metrics.k8s.io", "nodes", "NodeMetrics"},
	// allocated per Service IP
	{"networking.k8s.io", "ipaddresses", "IPAddress"},
	// deprecated, computed on read
	{"", "componentstatuses", "ComponentStatus"},
}

// runtimeState as lookups, by resource and by kind, filled in by init below
var (
	skipResources = map[schema.GroupResource]bool{}
	skipKinds     = map[schema.GroupKind]bool{}
)

// init fills skipResources and skipKinds from runtimeState, so DefaultSkip and DefaultSkipKind are map lookups
func init() {
	// adding each runtime state type to both lookups
	for _, t := range runtimeState {
		skipResources[schema.GroupResource{Group: t.group, Resource: t.resource}] = true
		skipKinds[schema.GroupKind{Group: t.group, Kind: t.kind}] = true
	}
}

// DefaultSkip tells if the collector leaves out a type by default, one that holds runtime state rather than configuration
func DefaultSkip(gr schema.GroupResource) bool { return skipResources[gr] }

// DefaultSkipKind is DefaultSkip by kind, for objects read without discovery
func DefaultSkipKind(gk schema.GroupKind) bool { return skipKinds[gk] }

// metadataOnlyTypes are high-volume types whose objects are usually children of others
// theyre listed metadata-only first, and fetched in full only when nothing collected owns them
var metadataOnlyTypes = map[schema.GroupResource]bool{
	{Group: "", Resource: "pods"}:                           true,
	{Group: "apps", Resource: "replicasets"}:                true,
	{Group: "apps", Resource: "controllerrevisions"}:        true,
	{Group: "batch", Resource: "jobs"}:                      true,
	{Group: "discovery.k8s.io", Resource: "endpointslices"}: true,
}

// DefaultMetadataOnly tells if the collector lists a type metadata-only first, by default
func DefaultMetadataOnly(gr schema.GroupResource) bool { return metadataOnlyTypes[gr] }

// ClusterCollector lists every listable resource in a cluster, the clients and the knobs for how it lists
type ClusterCollector struct {
	// discovery finds the types
	Discovery discover.Interface
	// the dynamic client reads any type as Unstructured
	Dynamic dynamic.Interface
	// turns on metadata-only listing for the MetadataOnly types, nil lists everything in full
	Metadata metadata.Interface

	// limits the read to these namespaces, empty means all namespaces
	Namespaces []string
	// FallbackNamespace is exported instead of the whole cluster when Namespaces is empty and the caller may not list Namespaces, their access is limited to some namespaces
	// the cli passes the kubeconfig context's namespace, empty means fail with a ScopeError instead
	FallbackNamespace string
	// RequireNamespaces makes a requested or fallback namespace that doesnt exist a NamespaceError, found before anything is listed
	// without it a missing namespace is a NAMESPACE_NOT_FOUND finding, the cli sets it
	RequireNamespaces bool
	// objects per list call, zero means DefaultPageSize
	PageSize int64
	// Skip tells the types not to list, nil means DefaultSkip
	Skip func(schema.GroupResource) bool
	// MetadataOnly tells the types to list metadata-only first, nil means DefaultMetadataOnly
	MetadataOnly func(schema.GroupResource) bool
	// the most stubs fetched one GET at a time per type, zero means DefaultHydrateGetLimit
	HydrateGetLimit int
	// Concurrency is how many types are listed at once, zero means DefaultConcurrency
	// the client's QPS and burst limits still apply
	Concurrency int
	// OwnerGetLimit caps the GETs for missing controller owners, zero means DefaultOwnerGetLimit, negative turns them off
	OwnerGetLimit int
}

// partialType groups the metadata-only stubs of one type, so hydrate can fetch them in full by type
type partialType struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	// the stubs, Partial until hydrate fills them in
	objects []*resource.Object
}

// typeInfo is a discovered type
type typeInfo struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	// true once a list of it succeeded, so resolveOwners can tell an owner deleted from an owner whose type wasnt collected
	listed bool
}

// listTask is one type to list
type listTask struct {
	info *typeInfo
	// lists the type metadata-only, into stubs
	metadataOnly bool
}

// listResult is what one listTask gave back
type listResult struct {
	// the full objects, for a full list
	objects []*resource.Object
	// the stubs, for a metadata-only list
	partial *partialType
	err     error
	// denied are the namespaces that refused the list with Forbidden, when others allowed it
	denied []string
}

// finding codes the cli reads
const (
	// ListDenied is the code of the finding naming the types a namespace refused to list, when others allowed them
	ListDenied = "LIST_DENIED"
	// NamespaceScopedAccess is the code of the finding on a capture that read only the kubeconfig context's namespace
	NamespaceScopedAccess = "NAMESPACE_SCOPED_ACCESS"
)

// Collect reads the cluster, it makes ClusterCollector a Collector
// in order it gets the server version, discovery, what the credentials may read (scope), the lists (at most Concurrency at a time), then the requested Namespace objects, missing owners and the full objects behind the metadata-only stubs that are kept
func (c *ClusterCollector) Collect(ctx context.Context) (*Result, error) {
	res := &Result{}
	// checking if the server version can be read, an unknown version is a warning, not a terminal error
	if v, err := c.Discovery.ServerVersionWithContext(ctx); err != nil {
		res.warn("SERVER_VERSION_UNKNOWN", err.Error())
	} else {
		res.KubernetesVersion = v.GitVersion
	}
	// invoking discover to find every listable type, its defined below
	discovered, kinds, err := c.discover(ctx, res)
	if err != nil {
		return nil, err
	}
	// invoking scope to work out what the credentials may read, its defined below
	// a caller who may only read some namespaces lists with a collector scoped to them
	sc, err := c.scope(ctx, res, kinds)
	if err != nil {
		return nil, err
	}
	// invoking tasks to pick the types to list, its defined below
	tasks := sc.tasks(discovered)
	// invoking listAll to list them, its defined below
	results, err := sc.listAll(ctx, tasks)
	if err != nil {
		return nil, err
	}
	// invoking merge to add the lists to res in a stable order, its defined below
	partials, listed := sc.merge(ctx, res, tasks, results)
	// checking if nothing at all could be listed, if so its a terminal error since an empty export would look like an empty cluster
	if listed == 0 && len(tasks) > 0 {
		// invoking IsPermission to check if the first failure was a refusal, its defined in access.go
		// if so the error names the credentials, through Identity, its defined in access.go
		if IsPermission(results[0].err) {
			return nil, &NoAccessError{Identity: Identity(ctx, sc.Dynamic, results[0].err), Namespaces: sc.namespaces(), Err: results[0].err}
		}
		return nil, fmt.Errorf("no resource type could be listed; first error: %w", results[0].err)
	}
	// invoking getNamespaces to add the requested Namespace objects, its defined below
	if err := sc.getNamespaces(ctx, res, kinds); err != nil {
		return nil, err
	}
	// invoking resolveOwners to look up controller owners missing from the lists, its defined below
	if err := sc.resolveOwners(ctx, res, kinds); err != nil {
		return nil, err
	}
	// invoking hydrate to fetch in full the stubs the export keeps, its defined below
	if err := sc.hydrate(ctx, res, partials); err != nil {
		return nil, err
	}
	return res, nil
}

// discover gives back every listable type the server serves, one version per API group, and the same keyed by group and kind
// the by-kind map is for resolveOwners, since owner references name a kind, not a resource
func (c *ClusterCollector) discover(ctx context.Context, res *Result) ([]*typeInfo, map[schema.GroupKind]*typeInfo, error) {
	// invoking discover.PreferredResources for the preferred version of each group, its defined in internal/discover/preferred.go
	lists, err := discover.PreferredResources(ctx, c.Discovery)
	// checking if discovery failed in some way
	if err != nil {
		// invoking discover.IsPartial to check if only some groups failed, its defined in internal/discover/preferred.go
		// if every group failed, or it wasnt partial, thats a terminal error
		if !discover.IsPartial(err) || len(lists) == 0 {
			return nil, nil, fmt.Errorf("discovering API resources: %w", err)
		}
		// an aggregated API like metrics-server may be registered but unreachable, a warning, not a terminal error
		res.warn("DISCOVERY_PARTIAL", err.Error())
	}
	kinds := map[schema.GroupKind]*typeInfo{}
	var discovered []*typeInfo
	// going through each group version discovery found
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		// checking if the group version doesnt parse, if so its skipped with a warning
		if err != nil {
			res.warn("DISCOVERY_BAD_GROUP_VERSION", err.Error())
			continue
		}
		// going through each resource of that group version
		for _, r := range list.APIResources {
			// checking if its a subresource, or not listable like SubjectAccessReview, if so its skipped
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "get") {
				continue
			}
			// invoking plainKind to check the kind looks like a real kind name, its defined below
			// kinds name files of the export and an aggregated API server can serve any text, so an odd one is not read
			if !plainKind(r.Kind) {
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

// plainKind tells if kind is an ascii letter followed by letters and digits, as every kubernetes kind is
func plainKind(kind string) bool {
	// checking each character
	for i, r := range kind {
		// checking if its a letter, or a digit past the first character, anything else fails
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return kind != ""
}

// tasks gives back the types to list, sorted since discovery order varies and sorting keeps the result the same every run
func (c *ClusterCollector) tasks(discovered []*typeInfo) []listTask {
	var tasks []listTask
	// going through each discovered type
	for _, info := range discovered {
		// invoking skip to check if the type is left out, its defined below
		if c.skip(info.gvr.GroupResource()) {
			continue
		}
		// checking if namespaces were given and the type is cluster-scoped, if so its not read
		if !info.namespaced && len(c.Namespaces) > 0 {
			continue
		}
		// invoking metadataOnly to pick how to list it, only when theres a metadata client, its defined below
		tasks = append(tasks, listTask{info: info, metadataOnly: c.Metadata != nil && c.metadataOnly(info.gvr.GroupResource())})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].info.gvr.String() < tasks[j].info.gvr.String() })
	return tasks
}

// merge adds the lists' objects to res in task order, so the result doesnt depend on which list finished first, and records what couldnt be read
// it gives back the metadata-only types, and how many types were listed
func (c *ClusterCollector) merge(ctx context.Context, res *Result, tasks []listTask, results []listResult) ([]*partialType, int) {
	var partials []*partialType
	listed := 0
	// the types refused in some of the namespaces read, by namespace
	deniedIn := map[string][]string{}
	// what couldnt be read, for the scope
	var unread []*snapshotv1alpha1.Unread
	// going through each result, in task order
	for i, r := range results {
		gvr := tasks[i].info.gvr
		// checking if the list failed, an error the API server gave for one type is a warning and the type is recorded as unread
		if r.err != nil {
			res.warn("LIST_FAILED", fmt.Sprintf("%s: %v", gvr, r.err))
			unread = append(unread, &snapshotv1alpha1.Unread{Group: gvr.Group, Kind: tasks[i].info.kind})
			continue
		}
		tasks[i].info.listed = true
		listed++
		// recording each namespace that refused this type
		for _, ns := range r.denied {
			deniedIn[ns] = append(deniedIn[ns], gvr.GroupResource().String())
			unread = append(unread, &snapshotv1alpha1.Unread{Namespace: ns, Group: gvr.Group, Kind: tasks[i].info.kind})
		}
		// checking if it was a metadata-only list, if so its stubs are kept aside for hydrate as well
		if r.partial != nil {
			partials = append(partials, r.partial)
			res.Objects = append(res.Objects, r.partial.objects...)
		} else {
			res.Objects = append(res.Objects, r.objects...)
		}
	}
	// checking if scope recorded what is read, if so the unread types go with it
	if res.Scope != nil {
		res.Scope.Unread = unread
	}
	// invoking warnDenied to add a finding for each namespace that refused some types, its defined below
	c.warnDenied(ctx, res, deniedIn)
	return partials, listed
}

// warnDenied adds a LIST_DENIED finding for each namespace that refused some types, naming the credentials when known
func (c *ClusterCollector) warnDenied(ctx context.Context, res *Result, deniedIn map[string][]string) {
	// checking if no namespace refused anything, if so theres nothing to say
	if len(deniedIn) == 0 {
		return
	}
	identity := ""
	// checking if scope already found who the credentials are, if so thats reused
	if res.Scope != nil {
		identity = res.Scope.Identity
	}
	// checking if the identity is still unknown, if so we ask the cluster
	if identity == "" {
		// invoking Identity to ask the cluster who we are, its defined in access.go
		identity = Identity(ctx, c.Dynamic)
		// checking if theres a scope to keep the answer in
		if res.Scope != nil {
			res.Scope.Identity = identity
		}
	}
	who := "these credentials"
	// checking if we know who the credentials belong to, if so the finding names them
	if identity != "" {
		who += " (" + identity + ")"
	}
	nss := make([]string, 0, len(deniedIn))
	// collecting the namespaces, to sort them for a stable order
	for ns := range deniedIn {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	// adding one finding per namespace, truncateList caps the types named, its defined in access.go
	for _, ns := range nss {
		res.warn(ListDenied, fmt.Sprintf("in namespace %s, %s may not list %s, so they were not read there: %s",
			ns, who, text.Plural(len(deniedIn[ns]), "type", "types"), truncateList(deniedIn[ns], 12)))
	}
}

// hydrate fetches in full the metadata-only objects the ownership stage will keep, those with no controller owner or one not collected
// it must decide the way ownership.ControllerOwnerResolver does, or snapshot.Build rejects a stub left metadata-only
// it goes in rounds, since an object found gone can leave its children needing a fetch next round
func (c *ClusterCollector) hydrate(ctx context.Context, res *Result, partials []*partialType) error {
	// checking if there were no metadata-only lists, if so theres nothing to fetch
	if len(partials) == 0 {
		return nil
	}
	limit := c.HydrateGetLimit
	// checking if the limit was left unset, if so the default applies
	if limit == 0 {
		limit = DefaultHydrateGetLimit
	}

	// gone marks stubs that went away, theyre dropped at the end
	gone := map[*resource.Object]bool{}
	// unreadable marks gone objects that couldnt be fetched, as opposed to deleted ones, their children are kept, not dropped as garbage
	unreadable := map[*resource.Object]bool{}
	// tried marks stubs already fetched or attempted, so each is tried once
	tried := map[*resource.Object]bool{}
	// repeating rounds until one fetches nothing
	for {
		// recomputed every round, objects removed last round no longer count as owners
		present := make(map[types.UID]bool, len(res.Objects))
		goneStatus := map[types.UID]resource.OwnerStatus{}
		// sorting each object into present, gone unreadable or gone deleted
		for _, o := range res.Objects {
			// checking which of those it is
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
		// going through each metadata-only type
		for _, pt := range partials {
			// invoking toFetch to pick this type's stubs to fetch this round, its defined below
			need := toFetch(pt, gone, tried, present, goneStatus)
			// checking if none need fetching, if so the type is done this round
			if len(need) == 0 {
				continue
			}
			fetched = true

			var err error
			// checking if few enough are needed to GET them one by one
			if limit > 0 && len(need) <= limit {
				// invoking hydrateByGet to fetch each one by name, its defined below
				err = c.hydrateByGet(ctx, pt, need, gone)
			} else {
				// invoking hydrateByList to list every object of the type in full instead, its defined below
				err = c.hydrateByList(ctx, pt, need, gone)
			}
			// checking if the fetch failed
			if err != nil {
				// invoking kube.IsObjectError to check if the error is about this type only, its defined in internal/kube/kube.go
				// if so its a warning, the stubs are marked gone and unreadable and their children are fetched next round
				if kube.IsObjectError(err) {
					res.warn("HYDRATE_FAILED", fmt.Sprintf("%s: %v", pt.gvr, err))
					// marking each needed stub gone and unreadable
					for _, o := range need {
						gone[o] = true
						unreadable[o] = true
					}
					continue
				}
				return fmt.Errorf("fetching %s: %w", pt.gvr, err)
			}
		}
		// checking if this round fetched nothing, if so were done
		if !fetched {
			break
		}
	}
	// checking if any stubs went away, if so theyre removed from the objects
	if len(gone) > 0 {
		res.Objects = slices.DeleteFunc(res.Objects, func(o *resource.Object) bool { return gone[o] })
	}
	return nil
}

// toFetch gives back pt's stubs to fetch in full this round, and marks them tried
// those are the ones the ownership stage will keep, and the children of objects that went away this run, which may have gone too (cascading deletion) or still exist as garbage the ownership stage drops
func toFetch(pt *partialType, gone, tried map[*resource.Object]bool, present map[types.UID]bool, goneStatus map[types.UID]resource.OwnerStatus) map[types.UID]*resource.Object {
	need := map[types.UID]*resource.Object{}
	// going through each stub of the type
	for _, o := range pt.objects {
		// checking if its already gone or tried, if so its skipped
		if gone[o] || tried[o] {
			continue
		}
		ownerGone := false
		// checking if it has a controller owner
		if owner := metav1.GetControllerOfNoCopy(o); owner != nil {
			var st resource.OwnerStatus
			// checking if that owner went away, if so the stub takes on why
			if st, ownerGone = goneStatus[owner.UID]; ownerGone {
				o.OwnerStatus = st
			}
		}
		// checking if the owner went away, or else the same rule the ownership stage drops by
		// invoking ownership.IsChild for that rule, its defined in pkg/ownership/ownership.go
		if ownerGone || !ownership.IsChild(o, present) {
			need[o.GetUID()] = o
			tried[o] = true
		}
	}
	return need
}

// hydrateByGet fetches each needed stub by name, marking gone the ones deleted or recreated since the list
func (c *ClusterCollector) hydrateByGet(ctx context.Context, pt *partialType, need map[types.UID]*resource.Object, gone map[*resource.Object]bool) error {
	// fetching each stub
	for _, o := range need {
		u, err := c.Dynamic.Resource(pt.gvr).Namespace(o.GetNamespace()).Get(ctx, o.GetName(), metav1.GetOptions{})
		// checking what the GET gave back
		switch {
		// checking if its not found, if so its gone, deleted since the list
		case apierrors.IsNotFound(err):
			gone[o] = true
		case err != nil:
			return err
		case u.GetUID() != o.GetUID():
			// deleted and recreated since the list, the new object wasnt part of this capture
			gone[o] = true
		default:
			// invoking setFull to put the full object in place of the stub, its defined below
			setFull(o, u, pt)
		}
	}
	return nil
}

// hydrateByList lists every object of the type in full, filling in the needed stubs by uid, a stub not in the list is gone
func (c *ClusterCollector) hydrateByList(ctx context.Context, pt *partialType, need map[types.UID]*resource.Object, gone map[*resource.Object]bool) error {
	seen := map[types.UID]bool{}
	// invoking listFull with a keep func that fills the stubs as the items go by, its defined below
	_, err := c.listFull(ctx, pt.gvr, pt.kind, pt.namespaced, func(u *unstructured.Unstructured) bool {
		// checking if this item is one of the needed stubs, if so setFull fills it in, its defined below
		if o, ok := need[u.GetUID()]; ok {
			setFull(o, u, pt)
			seen[u.GetUID()] = true
		}
		return false // nothing to return, setFull updates the stubs
	}, func(string) {}) // a namespace that refused the list had no stubs to fill
	if err != nil {
		return err
	}
	// marking gone each needed stub the list didnt have
	for uid, o := range need {
		// checking if the list didnt have it
		if !seen[uid] {
			gone[o] = true
		}
	}
	return nil
}

// setFull replaces the stub o with its full object u, setting the api version and kind the stub had
func setFull(o *resource.Object, u *unstructured.Unstructured, pt *partialType) {
	u.SetAPIVersion(pt.gvr.GroupVersion().String())
	u.SetKind(pt.kind)
	o.Unstructured = u
	o.Partial = false
}

// listFull is a paged list of gvr through the dynamic client, keep filters the items when set, as hydrateByList does
// denied hears about each namespace that refused the list
func (c *ClusterCollector) listFull(ctx context.Context, gvr schema.GroupVersionResource, kind string, namespaced bool, keep func(*unstructured.Unstructured) bool, denied func(ns string)) ([]*resource.Object, error) {
	var out []*resource.Object
	// invoking paginate with a page func for each namespace and a func for each item, its defined below
	err := c.paginate(ctx, namespaced, metav1.ListOptions{}, func(ns string) pager.ListPageFunc {
		return func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, opts)
		}
	}, func(obj runtime.Object) error {
		u, ok := obj.(*unstructured.Unstructured)
		// checking if the item isnt Unstructured, the dynamic client always gives that so anything else is a terminal error
		if !ok {
			return fmt.Errorf("unexpected list item type %T", obj)
		}
		// checking if theres a filter and it leaves this item out
		if keep != nil && !keep(u) {
			return nil
		}
		// setting the api version and kind on each item by hand, some list items lack them
		u.SetAPIVersion(gvr.GroupVersion().String())
		u.SetKind(kind)
		out = append(out, resource.New(u))
		return nil
	}, denied)
	return out, err
}

// listMetadata lists pt.gvr metadata-only, into stubs in pt.objects, and for pods also tags the terminated ones with their phase
func (c *ClusterCollector) listMetadata(ctx context.Context, pt *partialType, denied func(ns string)) error {
	// invoking eachMetadata to page through the metadata, its defined below
	err := c.eachMetadata(ctx, pt.gvr, pt.namespaced, metav1.ListOptions{}, func(pom *metav1.PartialObjectMetadata) error {
		// turning the partial object metadata into a stub Unstructured, holding only metadata and marked Partial
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
	// checking if the list failed or the type isnt pods, either way were done
	if err != nil || pt.gvr.GroupResource() != podsResource {
		return err
	}
	// invoking tagPodPhases to mark the terminated pods, its defined below
	return c.tagPodPhases(ctx, pt, pt.namespaced)
}

// podsResource is the core pods type, the one listMetadata tags phases on
var podsResource = schema.GroupResource{Resource: "pods"}

// tagPodPhases sets status.phase on terminated pod stubs, for TERMINATED_PODS_TO_CLEANUP
// the metadata API gives back no status, so it lists the Succeeded and Failed pods by field selector
func (c *ClusterCollector) tagPodPhases(ctx context.Context, pt *partialType, namespaced bool) error {
	byUID := make(map[types.UID]*resource.Object, len(pt.objects))
	// indexing the stubs by uid
	for _, o := range pt.objects {
		byUID[o.GetUID()] = o
	}
	// listing the pods in each terminated phase
	for _, phase := range []string{"Succeeded", "Failed"} {
		opts := metav1.ListOptions{FieldSelector: "status.phase=" + phase}
		// invoking eachMetadata to page through the pods in this phase, its defined below
		err := c.eachMetadata(ctx, pt.gvr, namespaced, opts, func(pom *metav1.PartialObjectMetadata) error {
			// checking if this pod has a stub, if so the phase goes on it
			if o, ok := byUID[pom.UID]; ok {
				_ = unstructured.SetNestedField(o.Object, phase, "status", "phase") // cant fail, a stub has no status yet
			}
			return nil
		}, func(string) {})
		if err != nil {
			return err
		}
	}
	return nil
}

// eachMetadata is listFull's paging, metadata-only, calling fn with each item
func (c *ClusterCollector) eachMetadata(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, opts metav1.ListOptions, fn func(*metav1.PartialObjectMetadata) error, denied func(ns string)) error {
	// invoking paginate with a page func for each namespace and a func for each item, its defined below
	return c.paginate(ctx, namespaced, opts, func(ns string) pager.ListPageFunc {
		return func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return c.Metadata.Resource(gvr).Namespace(ns).List(ctx, opts)
		}
	}, func(obj runtime.Object) error {
		pom, ok := obj.(*metav1.PartialObjectMetadata)
		// checking if the item isnt PartialObjectMetadata, the metadata client always gives that so anything else is a terminal error
		if !ok {
			return fmt.Errorf("unexpected list item type %T", obj)
		}
		return fn(pom)
	}, denied)
}

// paginate wraps client-go's pager, once per namespace for namespaced types when namespaces are set, otherwise once for all namespaces
// listing several namespaces, one that refuses the type with Forbidden is skipped and passed to denied, when set, since a role can allow ConfigMaps in one namespace and not another
// the type fails only if every namespace refuses it
func (c *ClusterCollector) paginate(ctx context.Context, namespaced bool, opts metav1.ListOptions, page func(ns string) pager.ListPageFunc, fn func(runtime.Object) error, denied func(ns string)) error {
	namespaces := []string{metav1.NamespaceAll}
	// checking if the type is namespaced and namespaces were given, if so each is listed on its own
	if namespaced && len(c.Namespaces) > 0 {
		// invoking namespaces for them without repeats, its defined below
		namespaces = c.namespaces()
	}
	var refused []string
	var first error
	// listing each namespace page by page
	for _, ns := range namespaces {
		p := pager.New(page(ns))
		p.PageSize = c.PageSize
		// checking if the page size was left unset, if so the default applies
		if p.PageSize == 0 {
			p.PageSize = DefaultPageSize
		}
		// checking if listing this namespace failed
		if err := p.EachListItem(ctx, opts, fn); err != nil {
			// checking if the failure cant be skipped, no denied func, a single list, or not a Forbidden, if so it goes back as is
			if denied == nil || len(namespaces) == 1 || !apierrors.IsForbidden(err) {
				return err
			}
			refused = append(refused, ns)
			// checking if this is the first refusal, if so its kept to give back when every namespace refuses
			if first == nil {
				first = err
			}
		}
	}
	// checking if every namespace refused, if so the type fails with the first refusal
	if len(refused) == len(namespaces) {
		return first
	}
	// passing each refused namespace to denied
	for _, ns := range refused {
		denied(ns)
	}
	return nil
}

// skip tells if a type is left out, using c.Skip, which replaces DefaultSkip rather than adding to it
func (c *ClusterCollector) skip(gr schema.GroupResource) bool {
	// checking if a Skip func was given, if so it decides alone
	if c.Skip != nil {
		return c.Skip(gr)
	}
	return DefaultSkip(gr)
}

// metadataOnly tells if a type is listed metadata-only first, using c.MetadataOnly, which replaces DefaultMetadataOnly
func (c *ClusterCollector) metadataOnly(gr schema.GroupResource) bool {
	// checking if a MetadataOnly func was given, if so it decides alone
	if c.MetadataOnly != nil {
		return c.MetadataOnly(gr)
	}
	return DefaultMetadataOnly(gr)
}

// namespaces gives back c.Namespaces without repeats, in the order given, through dedupe, its defined in access.go
func (c *ClusterCollector) namespaces() []string { return dedupe(c.Namespaces) }

// listAll runs the list tasks, at most c.Concurrency at a time
// a result's err is set only for an error about that type (kube.IsObjectError), any other error stops the rest and the first one is given back
func (c *ClusterCollector) listAll(ctx context.Context, tasks []listTask) ([]listResult, error) {
	n := c.Concurrency
	// checking if the concurrency was left unset or is negative, if so the default applies
	if n <= 0 {
		n = DefaultConcurrency
	}
	results := make([]listResult, len(tasks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(n)
	// listing each type in its own goroutine, each writes only its own slot of results
	for i := range tasks {
		g.Go(func() error {
			// invoking listOne to list the type, its defined below
			results[i] = c.listOne(gctx, tasks[i])
			// checking if it failed with an error not about this type, invoking kube.IsObjectError, its defined in internal/kube/kube.go
			// if so the run stops
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

// listOne lists one type, metadata-only or in full, and records the namespaces that refused it
func (c *ClusterCollector) listOne(ctx context.Context, t listTask) listResult {
	var denied []string
	deny := func(ns string) { denied = append(denied, ns) }
	// checking if the type is listed metadata-only
	if t.metadataOnly {
		pt := &partialType{gvr: t.info.gvr, kind: t.info.kind, namespaced: t.info.namespaced}
		// invoking listMetadata to list the stubs, its defined above
		if err := c.listMetadata(ctx, pt, deny); err != nil {
			return listResult{err: err}
		}
		return listResult{partial: pt, denied: denied}
	}
	// invoking listFull to list the type in full, its defined above
	objs, err := c.listFull(ctx, t.info.gvr, t.info.kind, t.info.namespaced, nil, deny)
	return listResult{objects: objs, err: err, denied: denied}
}

// getNamespaces adds the requested Namespace objects themselves, since a namespaced read leaves out cluster-scoped types and so the Namespaces too
func (c *ClusterCollector) getNamespaces(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) error {
	info := kinds[schema.GroupKind{Kind: "Namespace"}]
	// checking if no namespaces were requested, the server has no Namespace type, or Namespaces are skipped, if so theres nothing to add
	if len(c.Namespaces) == 0 || info == nil || c.skip(info.gvr.GroupResource()) {
		return nil
	}
	// getting each requested namespace, invoking namespaces for them without repeats, its defined above
	for _, ns := range c.namespaces() {
		u, err := c.Dynamic.Resource(info.gvr).Get(ctx, ns, metav1.GetOptions{})
		// checking what the GET gave back
		// a missing namespace or an error about this one, invoking kube.IsObjectError, its defined in internal/kube/kube.go, is a warning, anything else is a terminal error
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

// resolveOwners looks up the controller owners missing from the lists, and sets each child's OwnerStatus from what it finds
// added owners can have missing owners too, so it repeats until nothing is added
func (c *ClusterCollector) resolveOwners(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) error {
	limit := c.OwnerGetLimit
	// checking if the limit was left unset, if so the default applies
	if limit == 0 {
		limit = DefaultOwnerGetLimit
	}
	present := make(map[types.UID]bool, len(res.Objects))
	// marking every collected object present
	for _, o := range res.Objects {
		present[o.GetUID()] = true
	}
	examined := 0 // res.Objects before this index have been examined
	gets := 0
	// repeating while the last round added objects not yet examined
	for examined < len(res.Objects) {
		// missing is one owner not collected, with where to get it and the children waiting on it
		type missing struct {
			ref      metav1.OwnerReference
			info     *typeInfo
			ns       string
			children []*resource.Object
		}
		byOwner := map[types.UID]*missing{}
		// going through the objects not yet examined
		for _, o := range res.Objects[examined:] {
			// invoking ownership.IsChild to check if the owner is already accounted for, its defined in pkg/ownership/ownership.go
			if ownership.IsChild(o, present) {
				continue
			}
			owner := metav1.GetControllerOfNoCopy(o)
			// checking if the object has no controller owner, if so theres nothing to look up
			if owner == nil {
				continue
			}
			gv, err := schema.ParseGroupVersion(owner.APIVersion)
			info := kinds[schema.GroupKind{Group: gv.Group, Kind: owner.Kind}]
			// checking if the owner's type is unknown or wasnt listed, if so the owner counts as not collected
			if err != nil || info == nil || !info.listed {
				o.OwnerStatus = resource.OwnerNotCollected
				continue
			}
			m := byOwner[owner.UID]
			// checking if this owner is new this round, if so its added
			if m == nil {
				ns := ""
				// checking if the owner's type is namespaced, if so its in the child's namespace
				if info.namespaced {
					ns = o.GetNamespace()
				}
				m = &missing{ref: *owner, info: info, ns: ns}
				byOwner[owner.UID] = m
			}
			m.children = append(m.children, o)
		}
		examined = len(res.Objects)

		// sorting the uids, so the GET limit always picks the same owners
		uids := make([]types.UID, 0, len(byOwner))
		// collecting the uids
		for uid := range byOwner {
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		// getting each missing owner
		for _, uid := range uids {
			m := byOwner[uid]
			// checking if GETs are off or the limit is used up, if so the rest stay OwnerUnknown
			if limit < 0 || gets >= limit {
				break // left OwnerUnknown
			}
			gets++
			u, err := c.Dynamic.Resource(m.info.gvr).Namespace(m.ns).Get(ctx, m.ref.Name, metav1.GetOptions{})
			status := resource.OwnerUnknown
			// checking what the GET gave back
			// not found or a different uid means the owner was deleted, an error about this one, invoking kube.IsObjectError, its defined in internal/kube/kube.go, leaves it unknown with a warning
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
			// setting the status on each child of this owner
			for _, o := range m.children {
				o.OwnerStatus = status
			}
		}
	}
	return nil
}

// ScopeError means the caller may not list the cluster as a whole (listing namespaces is forbidden), no namespaces were requested, and theres no fallback namespace
type ScopeError struct {
	// the refusal from the API server
	Err error
	// Identity is who the cluster says the credentials are, when known
	Identity string
}

// Error builds the message, saying namespaces to export must be given
func (e *ScopeError) Error() string {
	return "credentials can't list resources across the cluster (listing namespaces is forbidden); namespaces to export must be given: " + e.Err.Error()
}

// Unwrap gives back the refusal, so IsPermission still sees the Forbidden underneath
func (e *ScopeError) Unwrap() error { return e.Err }

// scope gives back the collector to list with, and records in res what it reads
// namespaces asked for must exist and be readable, or its a NamespaceError
// with none asked for, credentials that may not list namespaces read the fallback (the context's namespace), which must be readable, or get a ScopeError
// credentials that may list namespaces but not read across them read the namespaces they can, and the others are recorded as unreadable
func (c *ClusterCollector) scope(ctx context.Context, res *Result, kinds map[schema.GroupKind]*typeInfo) (*ClusterCollector, error) {
	info := kinds[schema.GroupKind{Kind: "Namespace"}]
	// checking if namespaces were asked for, if so theyre checked and read as given
	if len(c.Namespaces) > 0 {
		// invoking CheckNamespaces to make sure each can be read, its defined in access.go
		if err := CheckNamespaces(ctx, c.Dynamic, c.Namespaces, false, c.RequireNamespaces); err != nil {
			return nil, err
		}
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeNamespaces, Namespaces: c.namespaces()}
		return c, nil
	}
	// checking if the server has no Namespace type, if so theres nothing to probe and the cluster is read across every namespace
	if info == nil {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	// probing with a one-item list of namespaces, metadata and full lists need the same list permission and the metadata one is cheaper
	var err error
	// checking if theres a metadata client to probe with
	if c.Metadata != nil {
		_, err = c.Metadata.Resource(info.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	} else {
		_, err = c.Dynamic.Resource(info.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	}
	// checking what the probe gave back
	switch {
	case apierrors.IsForbidden(err):
		// checking if theres no fallback namespace, if so its a ScopeError naming the credentials, through Identity, its defined in access.go
		if c.FallbackNamespace == "" {
			return nil, &ScopeError{Err: err, Identity: Identity(ctx, c.Dynamic, err)}
		}
		// invoking CheckNamespaces to make sure the fallback can be read, its defined in access.go
		if err := CheckNamespaces(ctx, c.Dynamic, []string{c.FallbackNamespace}, true, c.RequireNamespaces); err != nil {
			return nil, err
		}
		scoped := *c
		scoped.Namespaces = []string{c.FallbackNamespace}
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeContextNamespace, Namespaces: scoped.Namespaces, Identity: Identity(ctx, c.Dynamic, err)}
		// invoking accessHint to say what they can list there, its defined below
		res.warn(NamespaceScopedAccess, fmt.Sprintf(
			"credentials can't list resources across the cluster (listing namespaces is forbidden), so only namespace %q was exported; %s",
			c.FallbackNamespace, c.accessHint(ctx, c.FallbackNamespace)))
		return &scoped, nil
	case err != nil:
		// other errors show up in the lists themselves
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	// invoking CanReadCluster to check if they can read across every namespace, its defined in access.go
	if CanReadCluster(ctx, c.Dynamic) {
		res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeCluster}
		return c, nil
	}
	// namespaces can be listed but not read across, so the ones that can be read are
	// invoking readableNamespaces to split them, its defined in access.go
	readable, unreadable, err := readableNamespaces(ctx, c.Dynamic)
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	// invoking Identity to name the credentials in the findings, its defined in access.go
	identity := Identity(ctx, c.Dynamic)
	// checking if none can be read, if so its a terminal ReadNothingError
	if len(readable) == 0 {
		return nil, &ReadNothingError{Identity: identity, Namespaces: len(unreadable)}
	}
	scoped := *c
	scoped.Namespaces = readable
	res.Scope = &snapshotv1alpha1.ReadScope{Mode: snapshot.ScopeReadableNamespaces, Namespaces: readable, Unreadable: unreadable, Identity: identity}
	// invoking truncateList to cap the namespaces named, its defined in access.go
	res.warn("NAMESPACE_READABLE_ONLY", fmt.Sprintf(
		"credentials can't read resources across the cluster, so the %d namespaces they can read were exported (%s), and cluster-scoped resources were not; "+
			"not readable with them: %s", len(readable), truncateList(readable, 10), truncateList(unreadable, 10)))
	return &scoped, nil
}

// maxHintTypes caps the types an access hint names
const maxHintTypes = 8

// rulesReviewGVR is how the dynamic client sends a SelfSubjectRulesReview, accessHint goes through it
var rulesReviewGVR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectrulesreviews"}

// accessHint says what the caller may list in ns, from a SelfSubjectRulesReview, the API behind kubectl auth can-i --list
func (c *ClusterCollector) accessHint(ctx context.Context, ns string) string {
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectRulesReview",
		"spec":       map[string]any{"namespace": ns},
	}}
	out, err := c.Dynamic.Resource(rulesReviewGVR).Create(ctx, review, metav1.CreateOptions{})
	// checking if the review failed, if so the hint says its unknown
	if err != nil {
		return "what they can read in it is unknown (" + err.Error() + ")"
	}
	rules, _, _ := unstructured.NestedSlice(out.Object, "status", "resourceRules")
	seen := map[string]bool{}
	// going through each rule
	for _, r := range rules {
		m, _ := r.(map[string]any)
		verbs, _, _ := unstructured.NestedStringSlice(m, "verbs")
		// checking if the rule doesnt allow list, if so its skipped
		if !slices.Contains(verbs, "list") && !slices.Contains(verbs, "*") {
			continue
		}
		groups, _, _ := unstructured.NestedStringSlice(m, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(m, "resources")
		// going through each group and resource the rule names
		for _, g := range groups {
			// pairing the group with each resource the rule names
			for _, r := range resources {
				// checking if its every resource in every group, if so they can list everything
				if r == "*" && g == "*" {
					return "in it they can list every type"
				}
				// checking if its a subresource, if so its skipped
				if strings.Contains(r, "/") {
					continue // subresource
				}
				// checking if its not the core group, if so the group goes on the name
				if g != "" {
					r += "." + g
				}
				seen[r] = true
			}
		}
	}
	// checking if nothing listable was found, if so the hint says to check the namespace
	if len(seen) == 0 {
		return "they can't list anything in it either; check the namespace"
	}
	listable := slices.Sorted(maps.Keys(seen))
	more := ""
	// checking if there are more types than the hint names, if so it says how many more
	if len(listable) > maxHintTypes {
		more = fmt.Sprintf(" and %d more", len(listable)-maxHintTypes)
		listable = listable[:maxHintTypes]
	}
	return "in it they can list " + strings.Join(listable, ", ") + more
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
