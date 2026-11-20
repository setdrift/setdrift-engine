// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package diff compares two snapshots: two clusters, or one cluster at two
// times. It pairs resources by group, kind, namespace and name, compares
// their normalized manifests field by field, and says for each difference
// what it can tell from the snapshots: who manages the resource on each
// side, whether only the clusters' own values (name, region, account)
// differ, and whether a Helm chart version or API version does.
//
// Both snapshots are normalized and redacted, so server defaults, status
// and secret values never show up as differences: Secrets compare by key.
package diff

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Options tune the comparison.
type Options struct {
	// NamespaceMap pairs a namespace in the first snapshot with its name in
	// the second, for environments that name them differently (web-staging
	// in A is web in B). Unmapped namespaces pair by name.
	NamespaceMap map[string]string
	// LabelA and LabelB name the sides in the result and its reasons
	// (default: the cluster names, with the capture times when they match).
	LabelA, LabelB string
}

// Key identifies a resource across the two snapshots, by its namespace in
// the first. The API version is not part of it: two clusters can serve the
// same kind at different versions.
type Key struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// Side describes one snapshot.
type Side struct {
	// Label names the side in output: the cluster name, with the capture
	// time when both snapshots are of the same cluster.
	Label             string `json:"label"`
	Cluster           string `json:"cluster"`
	CapturedAt        string `json:"capturedAt,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Region            string `json:"region,omitempty"`
	Account           string `json:"account,omitempty"`
	// Platform counts the platform's own objects (EKS's, kind's), which
	// are not compared: each platform creates its own.
	PlatformObjects int `json:"platformObjects"`
}

// Object is one side of a resource.
type Object struct {
	Key        Key    `json:"key"`
	APIVersion string `json:"apiVersion"`
	// Namespace is the resource's namespace in this snapshot, which differs
	// from Key.Namespace in the second snapshot under NamespaceMap.
	Namespace string `json:"namespace,omitempty"`
	// Manager says who manages it: "Argo CD payments", "Helm shop/web",
	// "Flux flux-system/apps", "operator Kafka/main", "applied by hand".
	Manager string `json:"manager"`
	Source  string `json:"source"`
}

// Field is one differing field: A or B is nil where the field is absent.
type Field struct {
	Path string `json:"path"`
	A    any    `json:"a,omitempty"`
	B    any    `json:"b,omitempty"`
}

// Change is a resource in both snapshots whose manifests differ.
type Change struct {
	A Object `json:"a"`
	B Object `json:"b"`
	// Fields differ even with each cluster's own values (its name, region,
	// account, mapped namespaces) set aside.
	Fields []Field `json:"fields,omitempty"`
	// EnvFields differ only in those values.
	EnvFields []Field `json:"envFields,omitempty"`
	// Reasons are what the snapshots tell about why it differs.
	Reasons []string `json:"reasons,omitempty"`
}

// EnvOnly reports whether the resource differs only in the clusters' own values.
func (c Change) EnvOnly() bool { return len(c.Fields) == 0 }

// Result is the comparison of two snapshots.
type Result struct {
	A Side `json:"a"`
	B Side `json:"b"`
	// Identical counts the resources in both with the same manifest.
	Identical int      `json:"identical"`
	Changed   []Change `json:"changed,omitempty"`
	OnlyA     []Object `json:"onlyA,omitempty"`
	OnlyB     []Object `json:"onlyB,omitempty"`
	// Nodes lists how the clusters' nodes differ in shape (count, instance
	// types, zones, allocatable capacity). Nodes come and go with
	// autoscaling, so these do not count as differences for Same.
	Nodes []NodeChange `json:"nodes,omitempty"`
	// NodesUnknown names the sides whose nodes the snapshot lacks: read
	// from a directory, or with credentials that can't list nodes.
	NodesUnknown []string `json:"nodesUnknown,omitempty"`
}

// NodeChange is one way the clusters' nodes differ.
type NodeChange struct {
	What string `json:"what"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// Same reports whether the snapshots match, apart from the clusters' own values.
func (r Result) Same() bool {
	if len(r.OnlyA) > 0 || len(r.OnlyB) > 0 {
		return false
	}
	for _, c := range r.Changed {
		if !c.EnvOnly() {
			return false
		}
	}
	return true
}

// Counts returns how many resources differ beyond the clusters' own values, and how many only in them.
func (r Result) Counts() (differ, envOnly int) {
	for _, c := range r.Changed {
		if c.EnvOnly() {
			envOnly++
		} else {
			differ++
		}
	}
	return differ, envOnly
}

type entry struct {
	obj      Object
	manifest any
	helm     *snapshotv1alpha1.HelmRelease
	// volume is a claim's bound volume, from the snapshot's inventory
	volume *snapshotv1alpha1.Volume
	// hpa: a HorizontalPodAutoscaler scales it, so its replicas are left out
	hpa bool
}

// indexed is a snapshot's resources by key, and what they need from each other.
type indexed struct {
	entries  map[Key]entry
	platform int
	// mounts lists the workloads mounting each claim, by "namespace/claim"
	mounts map[string][]string
}

// Compare compares snapshot a with snapshot b.
func Compare(a, b *snapshotv1alpha1.Snapshot, opts Options) (Result, error) {
	res := Result{A: side(a), B: side(b)}
	if res.A.Label == res.B.Label {
		res.A.Label += " (" + res.A.CapturedAt + ")"
		res.B.Label += " (" + res.B.CapturedAt + ")"
	}
	if opts.LabelA != "" {
		res.A.Label = opts.LabelA
	}
	if opts.LabelB != "" {
		res.B.Label = opts.LabelB
	}
	back := map[string]string{}
	for from, to := range opts.NamespaceMap {
		back[to] = from
	}
	ia, err := index(a, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.A.Label, err)
	}
	ib, err := index(b, back)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.B.Label, err)
	}
	as, bs := ia.entries, ib.entries
	res.A.PlatformObjects, res.B.PlatformObjects = ia.platform, ib.platform
	res.Nodes, res.NodesUnknown = compareNodes(a.GetInventory().GetNodes(), b.GetInventory().GetNodes(), res.A.Label, res.B.Label)

	subA, subB := substitutions(res.A, opts.NamespaceMap, true), substitutions(res.B, opts.NamespaceMap, false)
	for _, k := range sortedKeys(as) {
		ea := as[k]
		eb, ok := bs[k]
		if !ok {
			res.OnlyA = append(res.OnlyA, ea.obj)
			continue
		}
		raw := compare("", ea.manifest, eb.manifest, nil)
		vol, volWhy := compareVolumes(ea, eb, raw, ia.mounts[ea.obj.Namespace+"/"+ea.obj.Key.Name], res.A.Label, res.B.Label)
		if len(raw) == 0 && len(vol) == 0 {
			res.Identical++
			continue
		}
		c := Change{A: ea.obj, B: eb.obj, Fields: vol}
		for _, f := range raw {
			if f.Path == "metadata.namespace" && opts.NamespaceMap[ea.obj.Namespace] == eb.obj.Namespace {
				continue
			}
			if reflect.DeepEqual(subA.apply(f.A), subB.apply(f.B)) {
				c.EnvFields = append(c.EnvFields, f)
			} else {
				c.Fields = append(c.Fields, f)
			}
		}
		if len(c.Fields) == 0 && len(c.EnvFields) == 0 {
			res.Identical++
			continue
		}
		c.Reasons = append(reasons(ea, eb, res.A.Label, res.B.Label), volWhy...)
		res.Changed = append(res.Changed, c)
	}
	for _, k := range sortedKeys(bs) {
		if _, ok := as[k]; !ok {
			res.OnlyB = append(res.OnlyB, bs[k].obj)
		}
	}
	return res, nil
}

func side(s *snapshotv1alpha1.Snapshot) Side {
	c := s.GetCluster()
	sd := Side{
		Label:             c.GetName(),
		Cluster:           c.GetName(),
		KubernetesVersion: c.GetKubernetesVersion(),
		Platform:          c.GetClassification().GetDistribution(),
		Region:            c.GetClassification().GetRegion(),
	}
	if sd.Platform == "" {
		sd.Platform = c.GetPlatform()
	}
	if t := s.GetCapturedAt(); t != nil {
		sd.CapturedAt = t.AsTime().UTC().Format("2006-01-02 15:04:05Z")
	}
	if aws := s.GetInventory().GetAws(); aws != nil {
		sd.Account = aws.GetAccountId()
		if sd.Region == "" {
			sd.Region = aws.GetRegion()
		}
	}
	// arn:aws:eks:<region>:<account>:cluster/<name>
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) >= 6 && parts[0] == "arn" {
		if sd.Region == "" {
			sd.Region = parts[3]
		}
		if sd.Account == "" {
			sd.Account = parts[4]
		}
	}
	return sd
}

// index keys a snapshot's resources, leaving out the platform's own. back
// maps the second snapshot's namespaces to the first's.
func index(s *snapshotv1alpha1.Snapshot, back map[string]string) (indexed, error) {
	helm := map[string]*snapshotv1alpha1.HelmRelease{}
	for _, h := range s.GetInventory().GetHelmReleases() {
		helm[h.GetNamespace()+"/"+h.GetName()] = h
	}
	volumes := map[string]*snapshotv1alpha1.Volume{}
	for _, v := range s.GetInventory().GetVolumes() {
		volumes[v.GetNamespace()+"/"+v.GetClaim()] = v
	}
	ix := indexed{entries: map[Key]entry{}, mounts: map[string][]string{}}
	for _, r := range s.GetResources() {
		p := r.GetProvenance()
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_PLATFORM {
			ix.platform++
			continue
		}
		var m any
		if err := json.Unmarshal([]byte(r.GetManifestJson()), &m); err != nil {
			return indexed{}, fmt.Errorf("%s %s/%s: %w", r.GetKind(), r.GetNamespace(), r.GetName(), err)
		}
		g, _, found := strings.Cut(r.GetApiVersion(), "/")
		if !found {
			g = ""
		}
		k := Key{Group: g, Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName()}
		if ns, ok := back[k.Namespace]; ok {
			k.Namespace = ns
		}
		if ns, ok := back[k.Name]; ok && k.Group == "" && k.Kind == "Namespace" {
			k.Name = ns
		}
		if prev, dup := ix.entries[k]; dup {
			// namespace x is mapped to y, and this snapshot has both x and y
			x, y := prev.obj.Namespace, r.GetNamespace()
			if k.Kind == "Namespace" && k.Group == "" {
				// the Namespace objects themselves: the earlier one's own name is in its manifest
				name, _ := mapKey(prev.manifest, "metadata", "name")
				x, _ = name.(string)
				y = r.GetName()
			}
			return indexed{}, fmt.Errorf("it has both namespace %s and namespace %s, which --map-namespace pairs with the same one: "+
				"limit the comparison to the mapped namespaces with -n", x, y)
		}
		e := entry{
			obj:      Object{Key: k, APIVersion: r.GetApiVersion(), Namespace: r.GetNamespace(), Manager: Manager(p), Source: source(p.GetSource())},
			manifest: m,
		}
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM {
			e.helm = helm[p.GetManager()]
		}
		if k.Group == "" && k.Kind == "PersistentVolumeClaim" {
			e.volume = volumes[r.GetNamespace()+"/"+r.GetName()]
		}
		for _, f := range r.GetFindings() {
			if f.GetCode() == "REPLICAS_FROM_HPA" {
				e.hpa = true
			}
		}
		for _, claim := range claims(m) {
			key := r.GetNamespace() + "/" + claim
			ix.mounts[key] = append(ix.mounts[key], r.GetKind()+" "+r.GetName())
		}
		ix.entries[k] = e
	}
	return ix, nil
}

// claims lists the claims a workload's pods mount.
func claims(m any) []string {
	var out []string
	for _, path := range [][]string{
		{"spec", "template", "spec", "volumes"},                        // Deployment, StatefulSet, DaemonSet, Job, ReplicaSet
		{"spec", "jobTemplate", "spec", "template", "spec", "volumes"}, // CronJob
		{"spec", "volumes"}, // Pod
	} {
		v := m
		for _, p := range path {
			mm, _ := v.(map[string]any)
			v = mm[p]
		}
		vols, _ := v.([]any)
		for _, vol := range vols {
			vm, _ := vol.(map[string]any)
			pvc, _ := vm["persistentVolumeClaim"].(map[string]any)
			if name, _ := pvc["claimName"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// compareVolumes compares two claims' bound volumes, as the inventory
// recorded them: the claim's manifest leaves out what a cluster's defaults
// set, such as the default storage class, so two claims can match while
// their volumes do not. It returns the differing fields, as "(volume) ..."
// paths, and why.
func compareVolumes(a, b entry, manifestFields []Field, mountedBy []string, la, lb string) ([]Field, []string) {
	va, vb := a.volume, b.volume
	if va == nil || vb == nil {
		return nil, nil
	}
	named := false
	for _, f := range manifestFields {
		if f.Path == "spec.storageClassName" {
			named = true
		}
	}
	var fields []Field
	var why []string
	add := func(what, x, y string) {
		if x != y {
			fields = append(fields, Field{Path: "(volume) " + what, A: orNil(x), B: orNil(y)})
		}
	}
	if !named {
		add("storageClass", va.GetStorageClass(), vb.GetStorageClass())
		if va.GetStorageClass() != vb.GetStorageClass() {
			_, setA := mapKey(a.manifest, "spec", "storageClassName")
			_, setB := mapKey(b.manifest, "spec", "storageClassName")
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
	if (len(fields) > 0 || named) && len(mountedBy) > 0 {
		why = append(why, "mounted by "+strings.Join(mountedBy, ", "))
	}
	return fields, why
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mapKey(m any, path ...string) (any, bool) {
	v := m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return v, true
}

// compareNodes lists how two node summaries differ.
func compareNodes(a, b *snapshotv1alpha1.NodeSummary, la, lb string) ([]NodeChange, []string) {
	var unknown []string
	if a == nil {
		unknown = append(unknown, la)
	}
	if b == nil {
		unknown = append(unknown, lb)
	}
	if unknown != nil {
		return nil, unknown
	}
	var out []NodeChange
	add := func(what, x, y string) {
		if x != y {
			out = append(out, NodeChange{What: what, A: x, B: y})
		}
	}
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

// gib renders a memory quantity in GiB: "7.7Gi".
func gib(q string) string {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return q
	}
	return strconv.FormatFloat(float64(v.Value())/(1<<30), 'f', 1, 64) + "Gi"
}

// counts renders "m5.large x3, m5.xlarge x1".
func counts(m map[string]int32) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, m[k]))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// Manager names who manages a resource, for people.
func Manager(p *snapshotv1alpha1.Provenance) string {
	var name string
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
	default:
		return "applied by hand"
	}
	if m := p.GetManager(); m != "" {
		return name + " " + m
	}
	return name
}

func source(s snapshotv1alpha1.Source) string {
	if s == snapshotv1alpha1.Source_SOURCE_UNSPECIFIED {
		s = snapshotv1alpha1.Source_SOURCE_UNMANAGED
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "SOURCE_"))
}

// reasons says what the snapshots tell about why a resource differs.
func reasons(a, b entry, la, lb string) []string {
	var out []string
	if a.obj.Manager != b.obj.Manager {
		out = append(out, fmt.Sprintf("managed differently: %s in %s, %s in %s", a.obj.Manager, la, b.obj.Manager, lb))
	}
	if a.hpa != b.hpa {
		in := la
		if b.hpa {
			in = lb
		}
		out = append(out, fmt.Sprintf("a HorizontalPodAutoscaler scales it in %s only, so its replicas are left out there", in))
	}
	if a.obj.APIVersion != b.obj.APIVersion {
		out = append(out, fmt.Sprintf("read as %s in %s, %s in %s", a.obj.APIVersion, la, b.obj.APIVersion, lb))
	}
	if a.helm != nil && b.helm != nil {
		ca, cb := a.helm.GetChart()+" "+a.helm.GetChartVersion(), b.helm.GetChart()+" "+b.helm.GetChartVersion()
		if ca != cb {
			out = append(out, fmt.Sprintf("Helm chart %s in %s, %s in %s", ca, la, cb, lb))
		} else {
			out = append(out, fmt.Sprintf("same Helm chart (%s): the release values differ, or the resource was changed after install", ca))
		}
	}
	return out
}

// compare lists the fields that differ between a and b, in path order.
func compare(path string, a, b any, out []Field) []Field {
	if reflect.DeepEqual(a, b) {
		return out
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			break
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		for _, k := range sortedStrings(keys) {
			out = compare(join(path, k), av[k], bv[k], out)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok {
			break
		}
		// lists of named items (containers, env, ports, volumes) pair by name
		if na, nb := byName(av), byName(bv); na != nil && nb != nil {
			names := map[string]bool{}
			for k := range na {
				names[k] = true
			}
			for k := range nb {
				names[k] = true
			}
			for _, n := range sortedStrings(names) {
				out = compare(path+"["+n+"]", na[n], nb[n], out)
			}
			return out
		}
		for i := 0; i < max(len(av), len(bv)); i++ {
			var x, y any
			if i < len(av) {
				x = av[i]
			}
			if i < len(bv) {
				y = bv[i]
			}
			out = compare(path+"["+strconv.Itoa(i)+"]", x, y, out)
		}
		return out
	}
	return append(out, Field{Path: path, A: a, B: b})
}

// byName indexes a list whose items all have a distinct string name, or returns nil.
func byName(l []any) map[string]any {
	if len(l) == 0 {
		return nil
	}
	out := make(map[string]any, len(l))
	for _, it := range l {
		m, ok := it.(map[string]any)
		if !ok {
			return nil
		}
		n, ok := m["name"].(string)
		if !ok || n == "" {
			return nil
		}
		if _, dup := out[n]; dup {
			return nil
		}
		out[n] = it
	}
	return out
}

var plainKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func join(path, k string) string {
	if !plainKey.MatchString(k) {
		k = "[" + strconv.Quote(k) + "]"
		return path + k
	}
	if path == "" {
		return k
	}
	return path + "." + k
}

// substitution replaces a cluster's own values in strings with placeholders,
// so that values differing only in them compare equal.
type substitution []struct {
	re   *regexp.Regexp
	with string
}

// substitutions for one side: its cluster name, region and account, and
// the mapped namespaces (first is true for the first snapshot's side).
// Placeholders hold no letters or digits, so no value matches inside one.
func substitutions(s Side, nsMap map[string]string, first bool) substitution {
	var sub substitution
	add := func(value string, slot int) {
		// short values (a cluster named "a") would match inside other words
		if len(value) < 3 {
			return
		}
		with := "\x00" + strings.Repeat("\x01", slot+1) + "\x00"
		sub = append(sub, struct {
			re   *regexp.Regexp
			with string
		}{regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(value) + `($|[^A-Za-z0-9])`), "${1}" + with + "${2}"})
	}
	add(s.Account, 0)
	add(s.Region, 1)
	add(s.Cluster, 2)
	// each mapped pair gets one slot, the same on both sides
	froms := make([]string, 0, len(nsMap))
	for from := range nsMap {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	for i, from := range froms {
		if first {
			add(from, 3+i)
		} else {
			add(nsMap[from], 3+i)
		}
	}
	// longest first, so a cluster named after its region doesn't split it
	sort.SliceStable(sub, func(i, j int) bool { return len(sub[i].re.String()) > len(sub[j].re.String()) })
	return sub
}

func (s substitution) apply(v any) any {
	switch x := v.(type) {
	case string:
		for _, r := range s {
			// twice: the boundaries of adjacent matches overlap
			x = r.re.ReplaceAllString(r.re.ReplaceAllString(x, r.with), r.with)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = s.apply(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = s.apply(e)
		}
		return out
	}
	return v
}

func sortedKeys(m map[Key]entry) []Key {
	keys := make([]Key, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Name < b.Name
	})
	return keys
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
