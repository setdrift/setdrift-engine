// Package translate writes an export's set for another platform, the generic set with its platform-specific settings rewritten for the target, like a storage class's provisioner, a load balancer's exposure or a Pod's node pool label
// the result is a kustomize overlay at the target's platform directory, like platform/gcp/gke, so the generic set itself is never changed
// what has no 1:1 equivalent is a decision for the user, kept in translate-decisions.yaml beside the overlay, with a safe default where there is one and none where there isnt
// were migrating, not modernizing, so an object keeps its kind, its names and its intent, only what the target reads differently is rewritten
package translate

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// the choices a decision takes
const (
	// KeepSource keeps using the source platform's service, like an AWS role through federation, and for an object that manages a cloud resource leaves it to the source cluster
	KeepSource = "keep-source"
	// Equivalent asks for the target's nearest equivalent, where SetDrift knows one
	Equivalent = "equivalent"
	// LeaveOut leaves the object or setting out of the translation, and RESTORE.md lists it
	LeaveOut = "leave-out"
)

// Decision is one feature with no 1:1 equivalent on the target, and the user's choice for it
type Decision struct {
	// Feature names it uniquely, like identity/ServiceAccount/shop/orders
	Feature string `json:"feature"`
	// What says what it is and what it does on the source, for the person choosing
	What string `json:"what"`
	// Options are the choices that make sense for it
	Options []string `json:"options"`
	// Choose is the choice, empty until decided
	Choose string `json:"choose"`
	// Default is the safe choice a translation takes when Choose is empty, empty when there is none and the user must choose
	Default string `json:"default,omitempty"`
}

// choice gives back what the translation acts on, the user's choice or else the default
func (d Decision) choice() string {
	if d.Choose != "" {
		return d.Choose
	}
	return d.Default
}

// Patch is one change to one object of the generic set, as kustomize applies it
type Patch struct {
	// Group, Version, Kind, Namespace and Name pick the object
	Group, Version, Kind, Namespace, Name string
	// Ops are JSON patch operations, Delete removes the object from the translation instead
	Ops    []Op
	Delete bool
	// Why says what changed and why, for the overlay's README
	Why []string
}

// Op is one JSON patch operation
type Op struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Result is a translation, its patches, its decisions and what the README should say
type Result struct {
	From, To platform.Distribution
	// Dir is the target's platform directory under the cluster's, like platform/gcp/gke
	Dir       string
	Patches   []Patch
	Decisions []Decision
	// Notes are what the user should know, like a placement that was relaxed
	Notes []string
	// Stale are the features of earlier decisions with nothing left to decide in the export, like an object deleted since, theyre dropped
	Stale []string
}

// Undecided gives back the decisions with neither a choice nor a default, which stop a translation
func (r *Result) Undecided() []Decision {
	var out []Decision
	for _, d := range r.Decisions {
		if d.choice() == "" {
			out = append(out, d)
		}
	}
	return out
}

// Translate works out the translation of the generic set's objects from one platform to another
// from may be Unknown, for an export of a cluster on no known cloud, then only markers owned by a platform other than the target are rewritten
// earlier holds the decisions from a previous run, their choices carry over by feature
func Translate(objs []*unstructured.Unstructured, from, to platform.Distribution, m *capability.Map, earlier []Decision) (*Result, error) {
	tgt := m.For(to)
	if tgt == nil {
		return nil, fmt.Errorf("no capability map for %s: translate knows eks, aks and gke", to)
	}
	if from == to {
		return nil, fmt.Errorf("this export is already for %s", tgt.Name)
	}
	r := &Result{From: from, To: to, Dir: tgt.Translate.Dir}
	chosen := map[string]string{}
	for _, d := range earlier {
		chosen[d.Feature] = d.Choose
	}
	t := &translator{m: m, from: from, to: to, tgt: tgt, chosen: chosen, r: r}
	for _, o := range objs {
		// invoking object to work out one object's patch and decisions, its defined below
		t.object(o)
	}
	// checking each choice is one its feature offers, a choice that doesnt fit must not pass as decided
	var bad []string
	seen := map[string]bool{}
	for _, d := range r.Decisions {
		seen[d.Feature] = true
		if d.Choose != "" && !slices.Contains(d.Options, d.Choose) {
			bad = append(bad, fmt.Sprintf("%s: %s isn't an option here, choose %s", d.Feature, d.Choose, strings.Join(d.Options, " or ")))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%s: %s", DecisionsFile, strings.Join(bad, "; "))
	}
	// the earlier decisions with nothing left to decide are dropped and reported
	for _, d := range earlier {
		if !seen[d.Feature] {
			r.Stale = append(r.Stale, d.Feature)
		}
	}
	sort.Slice(r.Patches, func(i, j int) bool { return patchKey(r.Patches[i]) < patchKey(r.Patches[j]) })
	sort.Slice(r.Decisions, func(i, j int) bool { return r.Decisions[i].Feature < r.Decisions[j].Feature })
	r.Notes = slices.Compact(r.Notes)
	return r, nil
}

// patchKey orders patches by kind, namespace and name
func patchKey(p Patch) string { return p.Kind + "/" + p.Namespace + "/" + p.Name }

// translator holds one translation's state
type translator struct {
	m        *capability.Map
	from, to platform.Distribution
	tgt      *capability.Platform
	chosen   map[string]string
	r        *Result
}

// decide records a decision and gives back what to act on, the earlier choice when there was one, else the default
func (t *translator) decide(d Decision) string {
	if c, ok := t.chosen[d.Feature]; ok {
		d.Choose = c
	}
	t.r.Decisions = append(t.r.Decisions, d)
	return d.choice()
}

// foreign tells if a value is owned only by platforms other than the target
func (t *translator) foreign(owners []platform.Distribution) bool {
	return len(owners) > 0 && !slices.Contains(owners, t.to)
}

// object works out one object's translation
func (t *translator) object(o *unstructured.Unstructured) {
	gvk := o.GroupVersionKind()
	p := Patch{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: o.GetNamespace(), Name: o.GetName()}
	ref := strings.TrimPrefix(gvk.Kind+" "+qualified(o), " ")
	c := o.Object

	// an object whose kind belongs to another platform, like an ACK Bucket or an EC2NodeClass, has nothing to run it here
	if owners := t.m.GroupOwners(gvk.Group); t.foreign(owners) {
		what := fmt.Sprintf("%s (%s) is a %s object: on %s nothing serves or acts on it", ref, gvk.Group, names(t.m, owners), t.tgt.Name)
		switch t.decide(Decision{Feature: "object/" + gvk.Kind + "/" + qualified(o), What: what, Options: []string{KeepSource, LeaveOut}}) {
		case KeepSource, LeaveOut:
			p.Delete = true
			p.Why = append(p.Why, "left out of the translation, as decided: "+what)
		}
		t.r.Patches = append(t.r.Patches, p)
		return
	}

	// another platform's finalizers go: on the target nothing removes them, so the object could never be deleted
	if fins := o.GetFinalizers(); len(fins) > 0 {
		var keep []any
		var gone []string
		for _, f := range fins {
			if t.foreign(t.m.FinalizerOwners(f)) {
				gone = append(gone, f)
				continue
			}
			keep = append(keep, f)
		}
		if len(gone) > 0 {
			if len(keep) == 0 {
				p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/finalizers"})
			} else {
				p.Ops = append(p.Ops, Op{Op: "replace", Path: "/metadata/finalizers", Value: keep})
			}
			p.Why = append(p.Why, fmt.Sprintf("finalizer %s removed: on %s nothing would ever remove it, so the object couldn't be deleted", strings.Join(gone, ", "), t.tgt.Name))
		}
	}

	switch {
	case gvk.Group == "storage.k8s.io" && gvk.Kind == "StorageClass":
		// invoking storageClass to rewrite its provisioner and parameters, its defined below
		t.storageClass(&p, ref, c)
	case gvk.Group == "" && gvk.Kind == "Service":
		// invoking service to carry its exposure over, its defined below
		t.service(&p, ref, o)
	case gvk.Group == "" && gvk.Kind == "ServiceAccount":
		// invoking identity to record the decision for a cloud identity, its defined below
		t.identity(ref, o)
	case gvk.Group == "external-secrets.io" && (gvk.Kind == "SecretStore" || gvk.Kind == "ClusterSecretStore"):
		t.secretStore(ref, o)
	}
	// invoking placement to rewrite the node pool and spot labels a Pod template asks for, its defined in placement.go
	t.placement(&p, ref, o)
	if len(p.Ops) > 0 || p.Delete {
		t.r.Patches = append(t.r.Patches, p)
	}
}

// storageClass rewrites a class of another platform's provisioner to the target's class of the same tier, keeping its name so claims need no change
func (t *translator) storageClass(p *Patch, ref string, c map[string]any) {
	prov, _ := c["provisioner"].(string)
	if !t.foreign(t.m.ProvisionerOwners(prov)) {
		return
	}
	params := map[string]string{}
	if pm, ok := c["parameters"].(map[string]any); ok {
		for k, v := range pm {
			params[k] = fmt.Sprint(v)
		}
	}
	// invoking tierOf to recognize the source class's kind of storage, its defined below
	tier := t.tierOf(prov, params)
	var cls *capability.StorageClass
	for i := range t.tgt.Translate.Storage {
		if t.tgt.Translate.Storage[i].Tier == tier {
			cls = &t.tgt.Translate.Storage[i].Class
		}
	}
	if cls == nil {
		what := fmt.Sprintf("%s uses provisioner %s with parameters %v, and SetDrift knows no %s class of the same kind", ref, prov, params, t.tgt.Name)
		if t.decide(Decision{Feature: "storage/" + qualifiedName(p), What: what, Options: []string{LeaveOut}}) == LeaveOut {
			p.Delete = true
			p.Why = append(p.Why, "left out, as decided: "+what)
		}
		return
	}
	p.Ops = append(p.Ops, Op{Op: "replace", Path: "/provisioner", Value: cls.Provisioner})
	newParams := map[string]any{}
	for k, v := range cls.Parameters {
		newParams[k] = v
	}
	p.Ops = append(p.Ops, Op{Op: "add", Path: "/parameters", Value: newParams})
	if ann, ok := c["metadata"].(map[string]any)["annotations"].(map[string]any); ok && ann["storageclass.kubernetes.io/is-default-class"] != nil {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape("storageclass.kubernetes.io/is-default-class")})
		p.Why = append(p.Why, fmt.Sprintf("no longer marked the default class: %s keeps its own default, and two defaults would make claims that name no class land on either", t.tgt.Name))
	}
	p.Why = append(p.Why, fmt.Sprintf("provisioner %s (%s) becomes %s with %s, the same kind of storage on %s; the class keeps its name, so claims need no change", prov, tier, cls.Provisioner, formatParams(cls.Parameters), t.tgt.Name))
	if strings.Contains(fmt.Sprint(cls.Parameters), "<") {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: fill in the values in angle brackets in the overlay's kustomization.yaml before applying", ref))
	}
}

// tierOf recognizes a class's kind of storage from any platform's tiers, by provisioner and parameter, empty when none matches
func (t *translator) tierOf(prov string, params map[string]string) string {
	for _, pl := range t.m.Platforms {
		for _, st := range pl.Translate.Storage {
			if !slices.Contains(st.Provisioners, prov) {
				continue
			}
			// invoking param to read the tier's parameter without regard to case, its defined below
			v, ok := param(params, st.Parameter)
			switch {
			case st.Parameter == "":
				return st.Tier
			case ok && slices.ContainsFunc(st.Values, func(x string) bool { return strings.EqualFold(x, v) }):
				return st.Tier
			case !ok && st.Default:
				return st.Tier
			}
		}
	}
	return ""
}

// param reads a StorageClass parameter without regard to case, Azure's skuName is also written skuname and storageaccounttype
func param(params map[string]string, name string) (string, bool) {
	for k, v := range params {
		if strings.EqualFold(k, name) || (strings.EqualFold(name, "skuName") && strings.EqualFold(k, "storageaccounttype")) {
			return v, true
		}
	}
	return "", false
}

// service carries a load balancer's exposure, internal or external, over to the target, written explicitly, since each platform defaults differently and an internal load balancer must never become public
func (t *translator) service(p *Patch, ref string, o *unstructured.Unstructured) {
	typ, _, _ := unstructured.NestedString(o.Object, "spec", "type")
	cls, _, _ := unstructured.NestedString(o.Object, "spec", "loadBalancerClass")
	if typ != "LoadBalancer" && cls == "" {
		return
	}
	ann := o.GetAnnotations()
	// invoking exposure to read whether the source load balancer is internal or external, its defined below
	exp, source := t.exposure(cls, ann)
	if cls != "" && t.foreign(t.m.LoadBalancerClassOwners(cls)) {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/spec/loadBalancerClass"})
		p.Why = append(p.Why, fmt.Sprintf("load balancer class %s removed: %s gives a Service its load balancer without one", cls, t.tgt.Name))
	}
	// every other platform's load balancer annotation goes, the target reads none of them
	for _, k := range slices.Sorted(maps.Keys(ann)) {
		if t.foreign(t.m.ServiceAnnotationOwners(k)) {
			p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape(k)})
		}
	}
	want := t.tgt.Translate.LoadBalancer.External
	if exp == "internal" {
		want = t.tgt.Translate.LoadBalancer.Internal
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if len(ann) == 0 && !hasAddedAnnotations(p) {
			p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations", Value: map[string]any{}})
		}
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations/" + escape(k), Value: want[k]})
	}
	if len(p.Ops) > 0 {
		p.Why = append(p.Why, fmt.Sprintf("%s load balancer (%s), written explicitly for %s", exp, source, t.tgt.Name))
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s is an %s load balancer on %s; its address will be new, so point DNS and clients at it", ref, exp, t.tgt.Name))
	}
}

// hasAddedAnnotations tells if a patch already creates the annotations map
func hasAddedAnnotations(p *Patch) bool {
	return slices.ContainsFunc(p.Ops, func(o Op) bool { return o.Op == "add" && o.Path == "/metadata/annotations" })
}

// exposure reads a Service's exposure on the platform that owns its class or annotations, else on the source platform, and says what it went by
func (t *translator) exposure(cls string, ann map[string]string) (exposure, source string) {
	for _, pl := range t.m.Platforms {
		lb := pl.Translate.LoadBalancer
		if v, ok := ann[lb.Annotation]; ok {
			switch {
			case slices.ContainsFunc(lb.InternalValues, func(x string) bool { return strings.EqualFold(x, v) }):
				return "internal", lb.Annotation + ": " + v
			case slices.ContainsFunc(lb.ExternalValues, func(x string) bool { return strings.EqualFold(x, v) }):
				return "external", lb.Annotation + ": " + v
			}
		}
	}
	for _, pl := range t.m.Platforms {
		if d, ok := pl.Translate.LoadBalancer.ClassDefaults[cls]; ok {
			return d, "the default of " + pl.Name + "'s class " + cls
		}
	}
	if src := t.m.For(t.from); src != nil && src.Translate.LoadBalancer.Default != "" {
		return src.Translate.LoadBalancer.Default, src.Name + "'s default"
	}
	// with no platform to go by, internal is the safe reading: a public load balancer is a choice to make, not to fall into
	return "internal", "no platform to go by, so the safe reading"
}

// identity records the decision for a ServiceAccount carrying another platform's cloud identity
func (t *translator) identity(ref string, o *unstructured.Unstructured) {
	for _, k := range slices.Sorted(maps.Keys(o.GetAnnotations())) {
		owners := t.m.ServiceAccountAnnotationOwners(k)
		if !t.foreign(owners) {
			continue
		}
		src, dst := clouds(t.m, owners), clouds(t.m, []platform.Distribution{t.to})
		what := fmt.Sprintf("%s uses the %s identity %s (%s), and its Pods call %s services with it. keep-source federates the %s cluster to that identity, so the same calls work unchanged. Moving to %s services and a %s identity is a change to the app, outside SetDrift", ref, src, o.GetAnnotations()[k], k, src, t.tgt.Name, dst, dst)
		if t.decide(Decision{Feature: "identity/" + qualified(o), What: what, Options: []string{KeepSource, LeaveOut}, Default: KeepSource}) == KeepSource {
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s keeps its %s identity: in %s IAM, trust the %s cluster's OIDC issuer for it", ref, src, src, t.tgt.Name))
		}
	}
}

// secretStore records the decision for an External Secrets store of another platform's secret manager
func (t *translator) secretStore(ref string, o *unstructured.Unstructured) {
	prov, _, _ := unstructured.NestedMap(o.Object, "spec", "provider")
	for _, k := range slices.Sorted(maps.Keys(prov)) {
		owners := t.m.SecretStoreProviderOwners(k)
		if !t.foreign(owners) {
			continue
		}
		src, dst := clouds(t.m, owners), clouds(t.m, []platform.Distribution{t.to})
		what := fmt.Sprintf("%s reads secrets from %s's secret manager (provider %s). keep-source keeps reading it, federating the %s cluster to it. equivalent means moving the secrets to %s's secret manager yourself (SetDrift never copies secret values) and pointing the store there", ref, src, k, t.tgt.Name, dst)
		t.decide(Decision{Feature: "secret-store/" + qualified(o), What: what, Options: []string{KeepSource, Equivalent}, Default: KeepSource})
	}
}

// clouds writes the clouds of platforms for people, like AWS, for wording about their services
func clouds(m *capability.Map, ds []platform.Distribution) string {
	var out []string
	for _, d := range ds {
		c := d.Name()
		if p := m.For(d); p != nil && p.Cloud != "" {
			c = p.Cloud
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return strings.Join(out, " and ")
}

// names writes platforms for people, like EKS and AKS
func names(m *capability.Map, ds []platform.Distribution) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name()
		if p := m.For(d); p != nil {
			out[i] = p.Name
		}
	}
	return strings.Join(out, " and ")
}

// qualified names an object namespace/name, or name for a cluster-scoped one
func qualified(o *unstructured.Unstructured) string {
	if o.GetNamespace() == "" {
		return o.GetName()
	}
	return o.GetNamespace() + "/" + o.GetName()
}

// qualifiedName is qualified for a patch's object
func qualifiedName(p *Patch) string {
	if p.Namespace == "" {
		return p.Name
	}
	return p.Namespace + "/" + p.Name
}

// escape writes a key for a JSON pointer, ~ as ~0 and / as ~1
func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

// formatParams writes parameters as k=v, sorted
func formatParams(p map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(p)) {
		out = append(out, k+"="+p[k])
	}
	return strings.Join(out, ", ")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
