// Package diff compares two snapshots, two clusters or one cluster at two times, pairing resources by group, kind, namespace and name and comparing their normalized manifests field by field
// for each difference it says what it can tell from the snapshots, who manages the resource on each side, whether only the clusters' own values (name, region, account) differ, and whether a Helm chart version or API version does
// both snapshots are normalized and redacted, so server defaults, status and secret values never show up as differences, Secrets compare by key
package diff

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/normalize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Options holds what tunes the comparison, the diff command fills it from its flags
type Options struct {
	// NamespaceMap pairs a namespace in the first snapshot with its name in the second, for environments that name them differently, like web-staging in A and web in B
	// unmapped namespaces pair by name
	NamespaceMap map[string]string
	// ValueMap pairs any other string of the first cluster's with the second's, set apart as the clusters' own values, like a registry host harbor-a.acme.com in A and harbor-b.acme.com in B
	ValueMap map[string]string
	// LabelA and LabelB name the sides in the result and its reasons, by default the cluster names, with the capture times when they match
	LabelA, LabelB string
	// Capability is the capability map, each side's objects are checked against its own platform with it, nil skips the check
	Capability *capability.Map
}

// Key identifies a resource across the two snapshots, by its namespace in the first
// the API version isnt part of it, two clusters can serve the same kind at different versions
type Key struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// Side describes one snapshot, the cluster it was taken of and when
type Side struct {
	// Label names the side in output, the cluster name, with the capture time when both snapshots are of the same cluster
	Label             string `json:"label"`
	Cluster           string `json:"cluster"`
	CapturedAt        string `json:"capturedAt,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Region            string `json:"region,omitempty"`
	Account           string `json:"account,omitempty"`
	// PlatformObjects counts the platform's own objects (EKS's, kind's), which arent compared since each platform creates its own
	PlatformObjects int `json:"platformObjects"`
}

// Object is one side of a resource, as the result shows it
type Object struct {
	Key        Key    `json:"key"`
	APIVersion string `json:"apiVersion"`
	// Namespace is the resource's namespace in this snapshot, which differs from Key.Namespace in the second snapshot under NamespaceMap
	Namespace string `json:"namespace,omitempty"`
	// Manager says who manages it, like "Argo CD payments", "Helm shop/web", "Flux flux-system/apps", "operator Kafka/main" or "applied by hand"
	Manager string `json:"manager"`
	// Source is the provenance source in lower case, like helm or unmanaged
	Source string `json:"source"`
}

// Field is one differing field, A or B is nil where the field is absent
type Field struct {
	Path string `json:"path"`
	A    any    `json:"a,omitempty"`
	B    any    `json:"b,omitempty"`
	// Keys are Path's steps as managedFields names them, "f:spec" for a map key, "n:web" for the item named web of a list, "i:0" for an index
	Keys []string `json:"-"`
}

// Change is a resource in both snapshots whose manifests differ
type Change struct {
	A Object `json:"a"`
	B Object `json:"b"`
	// Fields differ even with each cluster's own values (its name, region, account, mapped namespaces) set aside
	Fields []Field `json:"fields,omitempty"`
	// EnvFields differ only in those values
	EnvFields []Field `json:"envFields,omitempty"`
	// Reasons are what the snapshots tell about why it differs
	Reasons []string `json:"reasons,omitempty"`
}

// EnvOnly tells if the resource differs only in the clusters' own values
func (c Change) EnvOnly() bool { return len(c.Fields) == 0 }

// Result is the comparison of two snapshots, what Compare gives back
type Result struct {
	A Side `json:"a"`
	B Side `json:"b"`
	// Identical counts the resources in both with the same manifest
	Identical int      `json:"identical"`
	Changed   []Change `json:"changed,omitempty"`
	OnlyA     []Object `json:"onlyA,omitempty"`
	OnlyB     []Object `json:"onlyB,omitempty"`
	// Nodes lists how the clusters' nodes differ in shape (count, instance types, zones, allocatable capacity)
	// nodes come and go with autoscaling, so these dont count as differences for Same
	Nodes []NodeChange `json:"nodes,omitempty"`
	// NodesUnknown names the sides whose nodes the snapshot lacks, read from a directory or with credentials that cant list nodes
	NodesUnknown []string `json:"nodesUnknown,omitempty"`
	// Limited says what the comparison left out because a side read only some namespaces
	// only what both read is compared, so nothing shows as in one side only for not having been read in the other
	Limited []string `json:"limited,omitempty"`
	// WontWork are the resources that name another platform's features than their own side's, like an EKS load balancer class in a GKE cluster
	// a migration can look identical and not work, so they count as differences for Same
	WontWork []WontWork `json:"wontWork,omitempty"`
	// Translation is the code of the translation compared through, like te2g, empty without one, see ApplyTranslation in translation.go
	Translation string `json:"translation,omitempty"`
	// AsTranslated counts the resources in both whose every difference the translation sorted out
	AsTranslated int `json:"asTranslated,omitempty"`
	// Translated, Decided and NotMoved are the differences sorted out through it, shown and not counted
	Translated []Sorted `json:"translated,omitempty"`
	Decided    []Sorted `json:"decided,omitempty"`
	NotMoved   []Sorted `json:"notMoved,omitempty"`
	// OnSource are the source's own objects that name another platform's features, already true of it before the translation, shown and not counted
	OnSource []Sorted `json:"onSource,omitempty"`
}

// WontWork is a resource on one side that names features its side's platform doesnt have, from the capability map
type WontWork struct {
	// Side is the label of the side its on
	Side    string   `json:"side"`
	Object  Object   `json:"object"`
	Reasons []string `json:"reasons"`
}

// NodeChange is one way the clusters' nodes differ, what differs and its value on each side
type NodeChange struct {
	What string `json:"what"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// Same tells if the snapshots match, apart from the clusters' own values
func (r Result) Same() bool {
	// checking if any resource is in one side only, or wont work on its side, if so they dont match
	if len(r.OnlyA) > 0 || len(r.OnlyB) > 0 || len(r.WontWork) > 0 {
		return false
	}
	// going through the changed resources, looking for one that differs beyond the clusters' own values
	for _, c := range r.Changed {
		// invoking EnvOnly and checking if this one differs in more than those, its defined above
		if !c.EnvOnly() {
			return false
		}
	}
	return true
}

// Counts gives back how many resources differ beyond the clusters' own values, and how many only in them
func (r Result) Counts() (differ, envOnly int) {
	// going through the changed resources, counting each in one of the two
	for _, c := range r.Changed {
		// invoking EnvOnly to tell which of the two this one is, its defined above
		if c.EnvOnly() {
			envOnly++
		} else {
			differ++
		}
	}
	return differ, envOnly
}

// entry is one resource of a snapshot with what the comparison needs about it besides its manifest
type entry struct {
	obj Object
	// the decoded manifest json
	manifest any
	// the Helm release that installed it, for the chart version in the reasons
	helm *snapshotv1alpha1.HelmRelease
	// volume is a claim's bound volume, from the snapshot's inventory
	volume *snapshotv1alpha1.Volume
	// set when a HorizontalPodAutoscaler scales it, so its replicas are left out
	hpa bool
}

// indexed is a snapshot's resources by key, and what they need from each other
type indexed struct {
	entries map[Key]entry
	// how many of the platform's own objects were left out
	platform int
	// mounts lists the workloads mounting each claim, by "namespace/claim"
	mounts map[string][]string
}

// Compare takes snapshot a and snapshot b and compares them, giving back every resource that changed, is in one side only or is identical, and how the nodes differ
// it fails when a snapshot cant be indexed, like a manifest that doesnt parse or two namespaces --map-namespace pairs with the same one
func Compare(a, b *snapshotv1alpha1.Snapshot, opts Options) (Result, error) {
	// invoking side to describe each snapshot, its defined below
	res := Result{A: side(a), B: side(b)}
	// checking if both sides have the same label, the same cluster twice, if so the capture times tell them apart
	if res.A.Label == res.B.Label {
		res.A.Label += " (" + res.A.CapturedAt + ")"
		res.B.Label += " (" + res.B.CapturedAt + ")"
	}
	// checking if labels were given, if so they replace the ones from the snapshots
	if opts.LabelA != "" {
		res.A.Label = opts.LabelA
	}
	// checking if a label was given for side B, if so it replaces the default
	if opts.LabelB != "" {
		res.B.Label = opts.LabelB
	}
	back := map[string]string{}
	// turning the namespace map around, so the second snapshot's namespaces can be keyed by the first's names
	for from, to := range opts.NamespaceMap {
		back[to] = from
	}
	// invoking index to key each snapshot's resources, the second one through the turned around map, its defined below
	ia, err := index(a, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.A.Label, err)
	}
	ib, err := index(b, back)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.B.Label, err)
	}
	as, bs := ia.entries, ib.entries
	// invoking pruneToCommon to keep only the namespaces both sides read, its defined below
	res.Limited = pruneToCommon(a.GetScope(), b.GetScope(), back, res.A.Label, res.B.Label, as, bs)
	// invoking pruneUnread and checking if it left out types a side couldnt list, if so its note is added, its defined below
	if n := pruneUnread(a.GetScope(), b.GetScope(), back, as, bs); n != "" {
		res.Limited = append(res.Limited, n)
	}
	res.A.PlatformObjects, res.B.PlatformObjects = ia.platform, ib.platform
	// invoking compareNodes to say how the node summaries differ, its defined below
	res.Nodes, res.NodesUnknown = compareNodes(a.GetInventory().GetNodes(), b.GetInventory().GetNodes(), res.A.Label, res.B.Label)

	// invoking substitutions to set aside each side's own values, so fields differing only in them count as env fields, its defined below
	subA, subB := substitutions(res.A, opts.NamespaceMap, opts.ValueMap, true), substitutions(res.B, opts.NamespaceMap, opts.ValueMap, false)
	// going through the first side's resources in sorted order, sortedKeys is defined below
	for _, k := range sortedKeys(as) {
		ea := as[k]
		eb, ok := bs[k]
		// checking if the second side lacks it, if so its in the first side only
		if !ok {
			res.OnlyA = append(res.OnlyA, ea.obj)
			continue
		}
		// invoking compareKeys to list the fields that differ, its defined below
		// invoking WithoutTranslationMark on both, so a translation's stamp isnt a difference, its defined below
		raw := compareKeys("", nil, WithoutTranslationMark(ea.manifest), WithoutTranslationMark(eb.manifest), nil)
		// invoking compareVolumes to compare a claim's bound volumes too, with the workloads that mount it, its defined below
		vol, volWhy := compareVolumes(ea, eb, raw, ia.mounts[ea.obj.Namespace+"/"+ea.obj.Key.Name], res.A.Label, res.B.Label)
		// checking if neither the manifests nor the volumes differ, if so its identical
		if len(raw) == 0 && len(vol) == 0 {
			res.Identical++
			continue
		}
		c := Change{A: ea.obj, B: eb.obj, Fields: vol}
		// sorting each differing field into the real differences and those only in the clusters' own values
		for _, f := range raw {
			// checking if its the namespace and the map pairs the two, if so thats no difference at all
			if f.Path == "metadata.namespace" && opts.NamespaceMap[ea.obj.Namespace] == eb.obj.Namespace {
				continue
			}
			// invoking apply on both values and checking if they match once each side's own values are set aside, its defined below
			if reflect.DeepEqual(subA.apply(f.A), subB.apply(f.B)) {
				c.EnvFields = append(c.EnvFields, f)
			} else {
				c.Fields = append(c.Fields, f)
			}
		}
		// checking if the only difference was the mapped namespace, if so its identical
		if len(c.Fields) == 0 && len(c.EnvFields) == 0 {
			res.Identical++
			continue
		}
		// invoking reasons to say what the snapshots tell about why it differs, its defined below
		c.Reasons = append(reasons(ea, eb, res.A.Label, res.B.Label), volWhy...)
		res.Changed = append(res.Changed, c)
	}
	// going through the second side's resources in sorted order, for those the first side lacks
	for _, k := range sortedKeys(bs) {
		// checking if the first side lacks it, if so its in the second side only
		if _, ok := as[k]; !ok {
			res.OnlyB = append(res.OnlyB, bs[k].obj)
		}
	}
	// invoking wontWork to check each side's resources against its own platform, its defined below
	res.WontWork = append(wontWork(opts.Capability, a, as, res.A.Label), wontWork(opts.Capability, b, bs, res.B.Label)...)
	return res, nil
}

// wontWork checks one side's resources against its own platform, classified with confidence, and gives back those naming another platform's features
func wontWork(m *capability.Map, s *snapshotv1alpha1.Snapshot, es map[Key]entry, label string) []WontWork {
	if m == nil {
		return nil
	}
	// invoking platform.FromProto to read the side's classification, its defined in pkg/platform/probe.go
	cls := platform.FromProto(s.GetCluster().GetClassification())
	// checking if the platform is known with confidence, a guess would flag the user's objects
	if !cls.Is(cls.Distribution) || cls.Distribution == platform.Unknown {
		return nil
	}
	var objs []capability.Object
	byRef := map[string]Object{}
	for _, k := range sortedKeys(es) {
		e := es[k]
		c, _ := e.manifest.(map[string]any)
		ref := k.Kind + " " + k.Name
		if e.obj.Namespace != "" {
			ref = k.Kind + " " + e.obj.Namespace + "/" + k.Name
		}
		objs = append(objs, capability.Object{Ref: ref, Group: k.Group, Kind: k.Kind, Content: c})
		byRef[ref] = e.obj
	}
	var out []WontWork
	// invoking Misfits to find what names another platform's features, its defined in pkg/capability/capability.go
	for _, mf := range m.Misfits(objs, cls.Distribution) {
		out = append(out, WontWork{Side: label, Object: byRef[mf.Ref], Reasons: mf.Reasons})
	}
	return out
}

// side takes a snapshot and describes it, its cluster, when it was taken, platform, region and account
// the region and account come from the classification first, then the AWS inventory, then the cluster's EKS ARN
func side(s *snapshotv1alpha1.Snapshot) Side {
	c := s.GetCluster()
	sd := Side{
		Label:             c.GetName(),
		Cluster:           c.GetName(),
		KubernetesVersion: c.GetKubernetesVersion(),
		Platform:          c.GetClassification().GetDistribution(),
		Region:            c.GetClassification().GetRegion(),
	}
	// checking if the classification named no distribution, if so the recorded platform is used
	if sd.Platform == "" {
		sd.Platform = c.GetPlatform()
	}
	// checking if the capture time was recorded, if so its shown in UTC
	if t := s.GetCapturedAt(); t != nil {
		sd.CapturedAt = t.AsTime().UTC().Format("2006-01-02 15:04:05Z")
	}
	// checking if the inventory has AWS details, if so they give the account and, when still unknown, the region
	if aws := s.GetInventory().GetAws(); aws != nil {
		sd.Account = aws.GetAccountId()
		// checking if the region is still unknown
		if sd.Region == "" {
			sd.Region = aws.GetRegion()
		}
	}
	// checking if the provider id is an ARN, arn:aws:eks:<region>:<account>:cluster/<name>, if so it fills in what is still unknown
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) >= 6 && parts[0] == "arn" {
		// checking if the region is still unknown
		if sd.Region == "" {
			sd.Region = parts[3]
		}
		// checking if the account is still unknown
		if sd.Account == "" {
			sd.Account = parts[4]
		}
	}
	return sd
}

// index takes a snapshot and keys its resources, leaving out the platform's own, with the Helm release, bound volume and autoscaling each one needs
// back maps the second snapshot's namespaces to the first's, its nil for the first snapshot
func index(s *snapshotv1alpha1.Snapshot, back map[string]string) (indexed, error) {
	helm := map[string]*snapshotv1alpha1.HelmRelease{}
	// keying the inventory's Helm releases by namespace/name, as a Helm resource's provenance manager names them
	for _, h := range s.GetInventory().GetHelmReleases() {
		helm[h.GetNamespace()+"/"+h.GetName()] = h
	}
	volumes := map[string]*snapshotv1alpha1.Volume{}
	// keying the inventory's volumes by namespace/claim
	for _, v := range s.GetInventory().GetVolumes() {
		volumes[v.GetNamespace()+"/"+v.GetClaim()] = v
	}
	ix := indexed{entries: map[Key]entry{}, mounts: map[string][]string{}}
	// going through each resource of the snapshot
	for _, r := range s.GetResources() {
		p := r.GetProvenance()
		// checking if the platform created it, if so its only counted, each platform creates its own
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_PLATFORM {
			ix.platform++
			continue
		}
		var m any
		// decoding the manifest, one that doesnt parse is a terminal error naming the resource
		if err := json.Unmarshal([]byte(r.GetManifestJson()), &m); err != nil {
			return indexed{}, fmt.Errorf("%s %s/%s: %w", r.GetKind(), r.GetNamespace(), r.GetName(), err)
		}
		// invoking kube.Group to take the group out of the API version, its defined in internal/kube/kube.go
		k := Key{Group: kube.Group(r.GetApiVersion()), Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName()}
		// checking if the namespace is mapped, if so its keyed by the first snapshot's name
		if ns, ok := back[k.Namespace]; ok {
			k.Namespace = ns
		}
		// checking if its a Namespace object of a mapped namespace, if so its name is keyed by the first snapshot's name too
		if ns, ok := back[k.Name]; ok && k.Group == "" && k.Kind == "Namespace" {
			k.Name = ns
		}
		// checking if the key is already taken, if so thats a terminal error
		if prev, dup := ix.entries[k]; dup {
			// namespace x is mapped to y, and this snapshot has both x and y
			x, y := prev.obj.Namespace, r.GetNamespace()
			// checking if its the Namespace objects themselves, the earlier one's own name is in its manifest
			if k.Kind == "Namespace" && k.Group == "" {
				// invoking mapKey to read that name, its defined below
				name, _ := mapKey(prev.manifest, "metadata", "name")
				x, _ = name.(string)
				y = r.GetName()
			}
			return indexed{}, fmt.Errorf("it has both namespace %s and namespace %s, which --map-namespace pairs with the same one: "+
				"limit the comparison to the mapped namespaces with -n", x, y)
		}
		// invoking Manager and source to say who manages it, theyre defined below
		e := entry{
			obj:      Object{Key: k, APIVersion: r.GetApiVersion(), Namespace: r.GetNamespace(), Manager: Manager(p), Source: source(p.GetSource())},
			manifest: m,
		}
		// checking if Helm manages it, if so its release is looked up by the provenance manager
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM {
			e.helm = helm[p.GetManager()]
		}
		// checking if its a claim, if so its bound volume is looked up
		if k.Group == "" && k.Kind == "PersistentVolumeClaim" {
			e.volume = volumes[r.GetNamespace()+"/"+r.GetName()]
		}
		// going through its findings, looking for the one saying an autoscaler sets its replicas
		for _, f := range r.GetFindings() {
			// checking if its the finding normalize.ReplicasFromHPA names, its defined in pkg/normalize/normalize.go
			if f.GetCode() == normalize.ReplicasFromHPA {
				e.hpa = true
			}
		}
		// invoking claims to list the claims its pods mount, each one records it as a workload mounting it, its defined below
		for _, claim := range claims(m) {
			key := r.GetNamespace() + "/" + claim
			ix.mounts[key] = append(ix.mounts[key], r.GetKind()+" "+r.GetName())
		}
		ix.entries[k] = e
	}
	return ix, nil
}

// claims takes a decoded manifest and lists the claims its pods mount, for workloads, CronJobs and Pods
func claims(m any) []string {
	var out []string
	// trying each place the pod volumes can be, by kind
	for _, path := range [][]string{
		{"spec", "template", "spec", "volumes"},                        // Deployment, StatefulSet, DaemonSet, Job, ReplicaSet
		{"spec", "jobTemplate", "spec", "template", "spec", "volumes"}, // CronJob
		{"spec", "volumes"}, // Pod
	} {
		v := m
		// stepping down the path, a missing step leaves v nil
		for _, p := range path {
			mm, _ := v.(map[string]any)
			v = mm[p]
		}
		vols, _ := v.([]any)
		// going through the volumes for the ones backed by a claim
		for _, vol := range vols {
			vm, _ := vol.(map[string]any)
			pvc, _ := vm["persistentVolumeClaim"].(map[string]any)
			// checking if this volume names a claim
			if name, _ := pvc["claimName"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// compareVolumes takes two claims' entries, the fields their manifests differ in and the workloads mounting the claim, and compares their bound volumes as the inventory recorded them
// the claim's manifest leaves out what a cluster's defaults set, such as the default storage class, so two claims can match while their volumes dont
// it gives back the differing fields, as "(volume) ..." paths, and why
func compareVolumes(a, b entry, manifestFields []Field, mountedBy []string, la, lb string) ([]Field, []string) {
	va, vb := a.volume, b.volume
	// checking if either side has no bound volume recorded, if so theres nothing to compare
	if va == nil || vb == nil {
		return nil, nil
	}
	named := false
	// checking if the manifests already differ in the storage class they name, so the volume's class isnt reported twice
	for _, f := range manifestFields {
		// checking if this is the storage class field
		if f.Path == "spec.storageClassName" {
			named = true
		}
	}
	var fields []Field
	var why []string
	// add records one volume field when the two sides differ, an empty value shows as absent
	add := func(what, x, y string) {
		// checking if the two values differ
		if x != y {
			// invoking orNil so an empty value shows as absent, its defined below
			fields = append(fields, Field{Path: "(volume) " + what, A: orNil(x), B: orNil(y)})
		}
	}
	// checking if the manifests dont already show a storage class difference, if so the volumes' classes are compared
	if !named {
		add("storageClass", va.GetStorageClass(), vb.GetStorageClass())
		// checking if the volumes' classes differ, if so the reason may be each cluster's default
		if va.GetStorageClass() != vb.GetStorageClass() {
			// invoking mapKey to tell if either claim names a class, its defined below
			_, setA := mapKey(a.manifest, "spec", "storageClassName")
			_, setB := mapKey(b.manifest, "spec", "storageClassName")
			// checking if neither claim names one, if so each cluster's default applied
			if !setA && !setB {
				w := fmt.Sprintf("the claim names no storage class, so each cluster's default applied: %s in %s, %s in %s",
					va.GetStorageClass(), la, vb.GetStorageClass(), lb)
				why = append(why, w)
			}
		}
	}
	add("capacity", va.GetCapacity(), vb.GetCapacity())
	add("driver", va.GetDriver(), vb.GetDriver())
	add("reclaimPolicy", va.GetReclaimPolicy(), vb.GetReclaimPolicy())
	add("volumeMode", va.GetVolumeMode(), vb.GetVolumeMode())
	add("fsType", va.GetFsType(), vb.GetFsType())
	// checking if the volume or its storage class differs and workloads mount it, if so the reasons name them
	if (len(fields) > 0 || named) && len(mountedBy) > 0 {
		why = append(why, "mounted by "+strings.Join(mountedBy, ", "))
	}
	return fields, why
}

// orNil gives back nil for an empty string and the string otherwise, so an empty volume field shows as absent
func orNil(s string) any {
	// checking if the string is empty
	if s == "" {
		return nil
	}
	return s
}

// mapKey takes a decoded document and a path of map keys and gives back the value there, and whether the path exists at all
func mapKey(m any, path ...string) (any, bool) {
	v := m
	// stepping down the path one key at a time
	for _, p := range path {
		mm, ok := v.(map[string]any)
		// checking if this step isnt a map, if so the path doesnt exist
		if !ok {
			return nil, false
		}
		// checking if the map lacks the key, if so the path doesnt exist
		if v, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return v, true
}

// compareNodes takes two node summaries and their sides' labels, and lists how they differ
// a side without a summary is named in the second list instead, then nothing is compared
func compareNodes(a, b *snapshotv1alpha1.NodeSummary, la, lb string) ([]NodeChange, []string) {
	var unknown []string
	// checking if each side has a node summary, a side without one is unknown
	if a == nil {
		unknown = append(unknown, la)
	}
	// checking if side B didnt read its nodes, if so its listed as unknown too
	if b == nil {
		unknown = append(unknown, lb)
	}
	// checking if either side is unknown, if so theres nothing to compare
	if unknown != nil {
		return nil, unknown
	}
	var out []NodeChange
	// add records one node property when the two sides differ
	add := func(what, x, y string) {
		// checking if the two values differ
		if x != y {
			out = append(out, NodeChange{What: what, A: x, B: y})
		}
	}
	// invoking counts and gib to render each property the same way on both sides, theyre defined below
	add("nodes", strconv.Itoa(int(a.GetCount())), strconv.Itoa(int(b.GetCount())))
	add("instance types", counts(a.GetInstanceTypes()), counts(b.GetInstanceTypes()))
	add("node groups", counts(a.GetPools()), counts(b.GetPools()))
	add("zones", counts(a.GetZones()), counts(b.GetZones()))
	add("OS/architecture", counts(a.GetPlatforms()), counts(b.GetPlatforms()))
	add("kubelet versions", counts(a.GetKubeletVersions()), counts(b.GetKubeletVersions()))
	add("allocatable CPU", a.GetCpu(), b.GetCpu())
	add("allocatable memory", gib(a.GetMemory()), gib(b.GetMemory()))
	add("allocatable pods", strconv.FormatInt(a.GetPods(), 10), strconv.FormatInt(b.GetPods(), 10))
	return out, nil
}

// gib takes a memory quantity and renders it in GiB, like "7.7Gi"
func gib(q string) string {
	v, err := resource.ParseQuantity(q)
	// checking if its not a quantity, if so its shown as it was
	if err != nil {
		return q
	}
	return strconv.FormatFloat(float64(v.Value())/(1<<30), 'f', 1, 64) + "Gi"
}

// counts takes how many nodes have each value and renders it sorted, like "m5.large x3, m5.xlarge x1", or "-" when theres none
func counts(m map[string]int32) string {
	keys := make([]string, 0, len(m))
	// collecting the values so they can be sorted
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	// rendering each value with its count
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, m[k]))
	}
	// checking if there was nothing to render
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// readNamespaces takes a snapshot's read scope and gives back the set of namespaces it read, by the first snapshot's names (back maps the second's)
// nil means every namespace, the side read the full cluster
func readNamespaces(sc *snapshotv1alpha1.ReadScope, back map[string]string) map[string]bool {
	// checking if the scope is one of the namespace-limited modes from pkg/snapshot/snapshot.go, any other means the whole cluster
	switch sc.GetMode() {
	case snapshot.ScopeNamespaces, snapshot.ScopeContextNamespace, snapshot.ScopeReadableNamespaces:
	default:
		return nil
	}
	out := map[string]bool{}
	// adding each namespace read, under the first snapshot's name
	for _, ns := range sc.GetNamespaces() {
		// checking if the namespace is mapped, if so its the first snapshot's name that goes in
		if from, ok := back[ns]; ok {
			ns = from
		}
		out[ns] = true
	}
	return out
}

// pruneToCommon takes both read scopes and both sides' entries and deletes from as and bs what only one side read, giving back notes on what was left out
// when a side read only some namespaces, cluster-scoped resources are left out too, except the Namespace objects of namespaces both read
func pruneToCommon(sa, sb *snapshotv1alpha1.ReadScope, back map[string]string, la, lb string, as, bs map[Key]entry) []string {
	// invoking readNamespaces for each side, its defined above
	na, nb := readNamespaces(sa, nil), readNamespaces(sb, back)
	// checking if both sides read the full cluster, if so theres nothing to prune
	if na == nil && nb == nil {
		return nil
	}
	common := map[string]bool{}
	// checking which sides read only some namespaces, the common ones are those of the limited side, or those both read
	switch {
	case na == nil:
		common = nb
	case nb == nil:
		common = na
	default:
		// keeping the namespaces the second side read too
		for ns := range na {
			// checking if the second side read this one
			if nb[ns] {
				common[ns] = true
			}
		}
	}
	// keep tells if a resource is in what both sides read
	keep := func(k Key) bool {
		// checking if its cluster-scoped
		if k.Namespace == "" {
			// a Namespace object of a namespace both read stays, other cluster-scoped resources need the full cluster on both sides
			return k.Group == "" && k.Kind == "Namespace" && common[k.Name]
		}
		return common[k.Namespace]
	}
	dropped := 0
	// deleting from both sides what isnt kept, counting it for the note
	for _, m := range []map[Key]entry{as, bs} {
		// going through each resource of this side
		for k := range m {
			// checking if its outside what both read
			if !keep(k) {
				delete(m, k)
				dropped++
			}
		}
	}
	// limitedBy tells if a side was limited by its credentials rather than by -n
	// -n on both sides alike is what was asked, nothing to say
	limitedBy := func(sc *snapshotv1alpha1.ReadScope) bool {
		return sc.GetMode() == snapshot.ScopeContextNamespace || sc.GetMode() == snapshot.ScopeReadableNamespaces
	}
	// both sides read the same namespaces only when both were limited to the same set, a full side against a limited one always leaves something out
	sameSets := na != nil && nb != nil && len(na) == len(nb) && len(common) == len(na)
	// checking if neither side was limited by its credentials and they read the same namespaces, if so theres no note
	if !limitedBy(sa) && !limitedBy(sb) && sameSets {
		return nil
	}
	var notes []string
	// writing a note for each side that read only some namespaces, with why and what it couldnt read
	for _, side := range []struct {
		label string
		sc    *snapshotv1alpha1.ReadScope
		read  map[string]bool
	}{{la, sa, na}, {lb, sb, nb}} {
		// checking if this side read the full cluster, if so it gets no note
		if side.read == nil {
			continue
		}
		// invoking scopeReason to say why this side read only those, its defined below
		n := fmt.Sprintf("%s read only namespaces %s (%s)", side.label, strings.Join(slices.Sorted(maps.Keys(side.read)), ", "), scopeReason(side.sc.GetMode()))
		// checking if some namespaces werent readable there, if so the note names them
		if len(side.sc.GetUnreadable()) > 0 {
			n += fmt.Sprintf("; not readable there: %s", strings.Join(side.sc.GetUnreadable(), ", "))
		}
		notes = append(notes, n)
	}
	// checking if the sides have no namespace in common, if so the note says nothing was compared, otherwise it says what was
	if len(common) == 0 {
		notes = append(notes, "the two sides read no namespace in common, so nothing was compared")
	} else {
		notes = append(notes, fmt.Sprintf("compared namespaces %s only, and no cluster-scoped resources; %d resources outside them were left out",
			strings.Join(slices.Sorted(maps.Keys(common)), ", "), dropped))
	}
	return notes
}

// scopeReason takes a read scope mode and says why a side read only some namespaces, for the notes
func scopeReason(mode string) string {
	// checking which namespace-limited mode it is
	switch mode {
	case snapshot.ScopeNamespaces:
		return "as asked (-n)"
	case snapshot.ScopeContextNamespace:
		return "its credentials may not list namespaces, so its kubeconfig context's namespace"
	case snapshot.ScopeReadableNamespaces:
		return "the ones its credentials can read"
	}
	return ""
}

// pruneUnread takes both read scopes and both sides' entries and deletes from as and bs the resources of a type either side couldnt list, where it couldnt
// that way they dont show as in one side only, and it gives back a note on what was dropped, empty when nothing was
func pruneUnread(sa, sb *snapshotv1alpha1.ReadScope, back map[string]string, as, bs map[Key]entry) string {
	// gk is a resource type, its group and kind
	type gk struct{ group, kind string }
	// all holds the types a side couldnt list anywhere, in holds the namespaces where a side couldnt list a type
	all := map[gk]bool{}
	in := map[gk]map[string]bool{}
	// add records one unread type, b says its from the second side, so its namespace goes through back
	add := func(u *snapshotv1alpha1.Unread, b bool) {
		k := gk{u.GetGroup(), u.GetKind()}
		// checking if it couldnt be listed anywhere, if so the type is left out everywhere
		if u.GetNamespace() == "" {
			all[k] = true
			return
		}
		ns := u.GetNamespace()
		// checking if its the second side and the namespace is mapped, if so its the first snapshot's name that goes in
		if from, ok := back[ns]; b && ok {
			ns = from
		}
		// checking if this type has no namespace recorded yet
		if in[k] == nil {
			in[k] = map[string]bool{}
		}
		in[k][ns] = true
	}
	// recording the first side's unread types, then the second's
	for _, u := range sa.GetUnread() {
		add(u, false)
	}
	// adding what side B couldnt read, marked as side B
	for _, u := range sb.GetUnread() {
		add(u, true)
	}
	// checking if both sides could list everything, if so theres nothing to drop
	if len(all)+len(in) == 0 {
		return ""
	}
	dropped := map[string]int{}
	// deleting from both sides the resources of an unread type, counting them by kind
	for _, m := range []map[Key]entry{as, bs} {
		// going through each resource of this side
		for k := range m {
			t := gk{k.Group, k.Kind}
			// checking if its type couldnt be listed anywhere or in its namespace
			if all[t] || in[t][k.Namespace] {
				delete(m, k)
				dropped[k.Kind]++
			}
		}
	}
	// checking if nothing was dropped, if so theres no note
	if len(dropped) == 0 {
		return ""
	}
	var kinds []string
	// listing each kind dropped with how many
	for kind, n := range dropped {
		kinds = append(kinds, fmt.Sprintf("%s (%d)", kind, n))
	}
	sort.Strings(kinds)
	return "left out where a side could not list them: " + strings.Join(kinds, ", ")
}

// Manager takes a resource's provenance and names who manages it, for people, like "Helm shop/web" or "applied by hand"
func Manager(p *snapshotv1alpha1.Provenance) string {
	var name string
	// checking which source manages it, anything not listed was applied by hand
	switch p.GetSource() {
	case snapshotv1alpha1.Source_SOURCE_ARGO_CD:
		name = "Argo CD"
	case snapshotv1alpha1.Source_SOURCE_FLUX:
		name = "Flux"
	case snapshotv1alpha1.Source_SOURCE_HELM:
		name = "Helm"
	case snapshotv1alpha1.Source_SOURCE_OPERATOR:
		name = "operator"
	case snapshotv1alpha1.Source_SOURCE_PLATFORM:
		name = "platform"
	case snapshotv1alpha1.Source_SOURCE_CI_CD:
		name = "CI/CD"
	default:
		return "applied by hand"
	}
	// checking if the provenance names the manager, if so it follows the source
	if m := p.GetManager(); m != "" {
		return name + " " + m
	}
	return name
}

// source takes a provenance source and gives back its name in lower case without the SOURCE_ prefix, an unspecified one counts as unmanaged
func source(s snapshotv1alpha1.Source) string {
	// checking if no source was recorded, if so its unmanaged
	if s == snapshotv1alpha1.Source_SOURCE_UNSPECIFIED {
		s = snapshotv1alpha1.Source_SOURCE_UNMANAGED
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "SOURCE_"))
}

// reasons takes both sides of a changed resource and their labels and says what the snapshots tell about why it differs
func reasons(a, b entry, la, lb string) []string {
	var out []string
	// checking if different managers manage it
	if a.obj.Manager != b.obj.Manager {
		out = append(out, fmt.Sprintf("managed differently: %s in %s, %s in %s", a.obj.Manager, la, b.obj.Manager, lb))
	}
	// checking if an autoscaler scales it on one side only, so its replicas were left out there
	if a.hpa != b.hpa {
		in := la
		// checking if its the second side the autoscaler is on
		if b.hpa {
			in = lb
		}
		out = append(out, fmt.Sprintf("a HorizontalPodAutoscaler scales it in %s only, so its replicas are left out there", in))
	}
	// checking if the two clusters served it at different API versions
	if a.obj.APIVersion != b.obj.APIVersion {
		out = append(out, fmt.Sprintf("read as %s in %s, %s in %s", a.obj.APIVersion, la, b.obj.APIVersion, lb))
	}
	// checking if Helm installed it on both sides, if so the chart versions are compared
	if a.helm != nil && b.helm != nil {
		ca, cb := a.helm.GetChart()+" "+a.helm.GetChartVersion(), b.helm.GetChart()+" "+b.helm.GetChartVersion()
		// checking if the charts differ, if not its the release values or a change after install
		if ca != cb {
			out = append(out, fmt.Sprintf("Helm chart %s in %s, %s in %s", ca, la, cb, lb))
		} else {
			out = append(out, fmt.Sprintf("same Helm chart (%s): the release values differ, or the resource was changed after install", ca))
		}
	}
	return out
}

// Fields takes two decoded JSON documents and lists the fields that differ between them, in path order, pairing the items of named lists (containers, env, ports, volumes) by name
// compareKeys does the work, its defined below, and pkg/verify/dryrun.go uses it to compare a dry-run result with the live object
func Fields(a, b any) []Field { return compareKeys("", nil, a, b, nil) }

// WithoutTranslationMark gives back a manifest without the annotations a translation keeps for itself, the one it stamps on every object it applies, on the object and on its Pod template, and a StorageClass's record of its original
// it copies only the maps it changes, the manifest itself is left as it is
func WithoutTranslationMark(m any) any {
	var strip func(v any, path []string) any
	strip = func(v any, path []string) any {
		mv, ok := v.(map[string]any)
		if !ok {
			return v
		}
		if len(path) == 0 {
			_, a := mv[capability.TranslationAnnotation]
			_, b := mv[capability.SourceClassAnnotation]
			if !a && !b {
				return v
			}
			out := maps.Clone(mv)
			delete(out, capability.TranslationAnnotation)
			delete(out, capability.SourceClassAnnotation)
			if len(out) == 0 {
				return nil
			}
			return out
		}
		child, has := mv[path[0]]
		if !has {
			return v
		}
		nc := strip(child, path[1:])
		if reflect.DeepEqual(nc, child) {
			return v
		}
		out := maps.Clone(mv)
		if nc == nil {
			delete(out, path[0])
			// a map the stamp alone was in, like a template's metadata, goes too, as it was before the translation
			if len(out) == 0 {
				return nil
			}
		} else {
			out[path[0]] = nc
		}
		return out
	}
	for _, p := range [][]string{
		{"metadata", "annotations"},
		{"spec", "template", "metadata", "annotations"},
		{"spec", "jobTemplate", "metadata", "annotations"},
		{"spec", "jobTemplate", "spec", "template", "metadata", "annotations"},
	} {
		m = strip(m, p)
	}
	return m
}

// compareKeys takes a and b under path, with steps as path's managedFields keys (Field.Keys), and appends to out the fields that differ, in path order
// it goes down maps and lists, a value of different types on the two sides is one differing field
func compareKeys(path string, steps []string, a, b any, out []Field) []Field {
	// checking if the two are equal, if so nothing below differs
	if reflect.DeepEqual(a, b) {
		return out
	}
	// checking what kind of value a is, maps and lists are compared entry by entry when b is the same kind
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		// checking if b isnt a map, if so the value differs as one field
		if !ok {
			break
		}
		keys := map[string]bool{}
		// collecting the keys of both maps
		for k := range av {
			keys[k] = true
		}
		// adding the keys only side B has
		for k := range bv {
			keys[k] = true
		}
		// comparing each key in sorted order, join and step are defined below
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			out = compareKeys(join(path, k), step(steps, "f:"+k), av[k], bv[k], out)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		// checking if b isnt a list, if so the value differs as one field
		if !ok {
			break
		}
		// invoking byName and checking if both lists are of named items (containers, env, ports, volumes), if so they pair by name, its defined below
		if na, nb := byName(av), byName(bv); na != nil && nb != nil {
			names := map[string]bool{}
			// collecting the names of both lists
			for k := range na {
				names[k] = true
			}
			// adding the names only side B has
			for k := range nb {
				names[k] = true
			}
			// comparing each named item in sorted order
			for _, n := range slices.Sorted(maps.Keys(names)) {
				out = compareKeys(path+"["+n+"]", step(steps, "n:"+n), na[n], nb[n], out)
			}
			return out
		}
		// comparing the items by index, up to the longer list, a missing item is nil
		for i := 0; i < max(len(av), len(bv)); i++ {
			var x, y any
			// checking if a has an item at this index
			if i < len(av) {
				x = av[i]
			}
			// checking if b has an item at this index
			if i < len(bv) {
				y = bv[i]
			}
			out = compareKeys(path+"["+strconv.Itoa(i)+"]", step(steps, "i:"+strconv.Itoa(i)), x, y, out)
		}
		return out
	}
	return append(out, Field{Path: path, A: a, B: b, Keys: steps})
}

// step gives back keys with k appended, without sharing keys' backing array, so sibling paths dont overwrite each other's steps
func step(keys []string, k string) []string {
	return append(keys[:len(keys):len(keys)], k)
}

// byName takes a list and indexes it by name when its items all have a distinct non-empty string name, or gives back nil
func byName(l []any) map[string]any {
	// checking if the list is empty, an empty list isnt a named one
	if len(l) == 0 {
		return nil
	}
	out := make(map[string]any, len(l))
	// going through the items, any one without a distinct name means the list isnt named
	for _, it := range l {
		m, ok := it.(map[string]any)
		// checking if the item isnt a map
		if !ok {
			return nil
		}
		n, ok := m["name"].(string)
		// checking if it has no string name or an empty one
		if !ok || n == "" {
			return nil
		}
		// checking if the name was already seen
		if _, dup := out[n]; dup {
			return nil
		}
		out[n] = it
	}
	return out
}

// plainKey matches the map keys a path can show after a dot, any other key is written in brackets and quoted
var plainKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// join takes a path and a map key and gives back the path to that key, path.k for an identifier-like key, path["k"] otherwise
func join(path, k string) string {
	// checking if the key isnt identifier-like, like one with a dot or a slash, if so its quoted in brackets
	if !plainKey.MatchString(k) {
		k = "[" + strconv.Quote(k) + "]"
		return path + k
	}
	// checking if its the first step, if so theres no dot before it
	if path == "" {
		return k
	}
	return path + "." + k
}

// substitution replaces a cluster's own values in strings with placeholders, so values differing only in them compare equal
type substitution []replacement

// replacement is one value of a cluster's and its placeholder
type replacement struct {
	// matches the value with a non-alphanumeric boundary or the string's edge on each side
	re *regexp.Regexp
	// the replacement, keeping the boundaries the expression captured
	with string
}

// substitutions builds the substitution for one side, its cluster name, region and account, the mapped namespaces and the mapped values (first is true for the first snapshot's side)
// placeholders hold no letters or digits, so no value matches inside one, and each kind of value gets its own slot, the same on both sides
func substitutions(s Side, nsMap, valueMap map[string]string, first bool) substitution {
	var sub substitution
	// add records one value with the placeholder for its slot
	add := func(value string, slot int) {
		// checking if the value is too short, short values (a cluster named "a") would match inside other words
		if len(value) < 3 {
			return
		}
		with := "\x00" + strings.Repeat("\x01", slot+1) + "\x00"
		sub = append(sub, replacement{regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(value) + `($|[^A-Za-z0-9])`), "${1}" + with + "${2}"})
	}
	add(s.Account, 0)
	add(s.Region, 1)
	add(s.Cluster, 2)
	// each mapped pair gets one slot, the same on both sides
	froms := make([]string, 0, len(nsMap))
	// collecting the mapped namespaces so they get their slots in sorted order
	for from := range nsMap {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	// adding each mapped namespace, the first side's name or the second side's
	for i, from := range froms {
		// checking which side this is, it decides which name of the pair to set aside
		if first {
			add(from, 3+i)
		} else {
			add(nsMap[from], 3+i)
		}
	}
	// each mapped value gets the next slot, the same on both sides, after the namespaces
	values := make([]string, 0, len(valueMap))
	// collecting the mapped values so they get their slots in sorted order
	for from := range valueMap {
		values = append(values, from)
	}
	sort.Strings(values)
	// adding each mapped value, the first side's or the second side's
	for i, from := range values {
		// checking which side this is, it decides which value of the pair to set aside
		if first {
			add(from, 3+len(froms)+i)
		} else {
			add(valueMap[from], 3+len(froms)+i)
		}
	}
	// longest first, so a cluster named after its region doesnt split it
	sort.SliceStable(sub, func(i, j int) bool { return len(sub[i].re.String()) > len(sub[j].re.String()) })
	return sub
}

// apply takes a decoded value and gives back a copy with the cluster's own values replaced by their placeholders, in every string at any depth
func (s substitution) apply(v any) any {
	// checking what kind of value it is, strings are replaced, maps and lists are copied with each entry applied
	switch x := v.(type) {
	case string:
		// replacing each value in turn
		for _, r := range s {
			// twice, the boundaries of adjacent matches overlap
			x = r.re.ReplaceAllString(r.re.ReplaceAllString(x, r.with), r.with)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		// applying to each entry of the map
		for k, e := range x {
			out[k] = s.apply(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		// applying to each item of the list
		for i, e := range x {
			out[i] = s.apply(e)
		}
		return out
	}
	return v
}

// sortedKeys gives back a side's keys sorted by namespace, kind, group and name, so the result comes out in the same order every run
func sortedKeys(m map[Key]entry) []Key {
	keys := make([]Key, 0, len(m))
	// collecting the keys so they can be sorted
	for k := range m {
		keys = append(keys, k)
	}
	// sorting by namespace first, then kind, group and name
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		// checking each part in turn, the first that differs decides
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		// checking if the kinds differ, if so they decide the order
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		// checking if the groups differ, if so they decide the order
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Name < b.Name
	})
	return keys
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
