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
	as, platA, err := index(a, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.A.Label, err)
	}
	bs, platB, err := index(b, back)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", res.B.Label, err)
	}
	res.A.PlatformObjects, res.B.PlatformObjects = platA, platB

	subA, subB := substitutions(res.A, opts.NamespaceMap, true), substitutions(res.B, opts.NamespaceMap, false)
	for _, k := range sortedKeys(as) {
		ea := as[k]
		eb, ok := bs[k]
		if !ok {
			res.OnlyA = append(res.OnlyA, ea.obj)
			continue
		}
		raw := compare("", ea.manifest, eb.manifest, nil)
		if len(raw) == 0 {
			res.Identical++
			continue
		}
		c := Change{A: ea.obj, B: eb.obj}
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
		c.Reasons = reasons(ea, eb, res.A.Label, res.B.Label)
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
func index(s *snapshotv1alpha1.Snapshot, back map[string]string) (map[Key]entry, int, error) {
	helm := map[string]*snapshotv1alpha1.HelmRelease{}
	for _, h := range s.GetInventory().GetHelmReleases() {
		helm[h.GetNamespace()+"/"+h.GetName()] = h
	}
	out := map[Key]entry{}
	platformObjects := 0
	for _, r := range s.GetResources() {
		p := r.GetProvenance()
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_PLATFORM {
			platformObjects++
			continue
		}
		var m any
		if err := json.Unmarshal([]byte(r.GetManifestJson()), &m); err != nil {
			return nil, 0, fmt.Errorf("%s %s/%s: %w", r.GetKind(), r.GetNamespace(), r.GetName(), err)
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
		e := entry{
			obj:      Object{Key: k, APIVersion: r.GetApiVersion(), Namespace: r.GetNamespace(), Manager: Manager(p), Source: source(p.GetSource())},
			manifest: m,
		}
		if p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM {
			e.helm = helm[p.GetManager()]
		}
		out[k] = e
	}
	return out, platformObjects, nil
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
