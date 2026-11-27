// Package translate writes an export's set for another platform, the generic set with its platform-specific settings rewritten for the target, like a storage class's provisioner, a load balancer's exposure or a Pod's node pool label
// the result is a kustomize overlay at the target's platform directory, like platform/gcp/gke, so the generic set itself is never changed
// what has no 1:1 equivalent is a decision for the user, kept in translate-decisions.yaml beside the overlay, with a safe default where there is one and none where there isnt
// were migrating, not modernizing, so an object keeps its kind, its names and its intent, only what the target reads differently is rewritten
package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/netpolicy"
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
	// Require has setdrift verify fail on a target that doesnt enforce the export's NetworkPolicies
	Require = "require"
	// AcceptUnenforced lets verify pass on a target that doesnt enforce them, for an emergency, said loudly every run
	AcceptUnenforced = "accept-unenforced"
)

// NetworkPolicyFeature is the decision for the export's NetworkPolicies, when the source enforced them
const NetworkPolicyFeature = "network-policy-enforcement"

// choices are every choice a decision can take
var choices = []string{KeepSource, Equivalent, LeaveOut, Require, AcceptUnenforced}

// Source is what a translation needs to know about the export besides its generic set
type Source struct {
	// Region is the source cluster's, for the AWS SDK's region in a kept AWS identity, empty when unknown
	Region string
	// NetworkPolicy is the engine that enforced NetworkPolicies on the source, netpolicy.None, or empty when the export doesnt say
	NetworkPolicy string
	// Held are the export's cloud-resource operator objects, from cloud-resources/, which arent in the generic set
	Held []*unstructured.Unstructured
}

// CloudResource is one cloud-resource operator object and what the translation does with it, for RESTORE.md
type CloudResource struct {
	// Ref is its kind and namespace/name, Operator its operator, Cloud the cloud its resource is on
	Ref, Operator, Cloud string
	// Retain is the operator's setting that makes deleting the object leave the resource in place
	Retain string
	// Choice is the decision taken, empty when the operator is the target's own and the export's RESTORE.md step moves it
	Choice string
	// Equivalent names the objects written for it with equivalent
	Equivalent []string
}

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
	// Basis fingerprints what the decision was made on, the source object as exported, so a later translation knows when it changed under the choice
	Basis string `json:"basis,omitempty"`
	// Previous is the user's choice before the source changed under it, set while the translation waits for them to choose again
	Previous string `json:"previous,omitempty"`
}

// Subject names what a decision is about for people, like ServiceAccount shop/orders, from its feature
func (d Decision) Subject() string {
	kind, rest, _ := strings.Cut(d.Feature, "/")
	switch kind {
	case "identity":
		return "ServiceAccount " + rest
	case "secret-store":
		return "the secret store " + rest
	case "storage":
		return "StorageClass " + rest
	case "ingress":
		return "Ingress " + rest
	case "object", "cloud-resource":
		k, name, _ := strings.Cut(rest, "/")
		return k + " " + name
	case NetworkPolicyFeature:
		return "the NetworkPolicies"
	}
	return d.Feature
}

// Describe says what a choice does in words, with the value the file holds, like to leave out ServiceAccount shop/orders (leave-out)
func (d Decision) Describe(choice string) string {
	s := d.Subject()
	var words string
	switch choice {
	case LeaveOut:
		words = "to leave out " + s
	case KeepSource:
		words = "to keep " + s + " using the source cloud"
	case Equivalent:
		words = "to use the target's equivalent of " + s
	case Require:
		words = "to require the target to enforce " + s
	case AcceptUnenforced:
		words = "to accept " + s + " not being enforced"
	default:
		words = choice + " for " + s
	}
	return words + " (" + choice + ")"
}

// Waiting tells if the user must choose again, the source changed under their earlier choice
func (d Decision) Waiting() bool { return d.Previous != "" && d.Choose == "" }

// Fingerprint gives back a short fingerprint of what a decision is made on, an object, or for NetworkPolicies the engine and the policies, the same for the same content
func Fingerprint(v any) string {
	// the error is dropped: marshalling plain maps, strings and slices can't fail
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
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
	// m is the capability map the translation was made with
	m        *capability.Map
	From, To platform.Distribution
	// Dir is where the translation goes under the cluster's directory, translations/<code>, and Code its name, like te2g for EKS to GKE
	Dir  string
	Code string
	// ID is the translation's id, a hash of what its overlay applies, set when its written
	ID        string
	Patches   []Patch
	Decisions []Decision
	// Notes are what the user should know, like a placement that was relaxed
	Notes []string
	// Stale are the features of earlier decisions with nothing left to decide in the export, like an object deleted since, theyre dropped
	Stale []string
	// Identities are the cloud identities kept, keep-source, for the RESTORE.md steps that federate the target to them
	Identities []Identity
	// Lost are what the translation can't bring over and RESTORE.md lists loudly, like a disk's zone redundancy
	Lost []Lost
	// Added are objects the translation writes into the overlay, the target's equivalents chosen for cloud resources
	Added []*unstructured.Unstructured
	// CloudResources are the export's cloud-resource operator objects and what happens to each
	CloudResources []CloudResource
	// NetworkPolicies counts the export's NetworkPolicies, NetworkPolicyEngine is what enforced them on the source, empty when unknown
	NetworkPolicies     int
	NetworkPolicyEngine string
	// Images are the images in another cloud's private registry, for RESTORE.md's step that copies them to the target's
	Images []Image
}

// Lost is something a translation can't bring over, with why and what to do instead, for RESTORE.md's list
type Lost struct {
	What, Why, ToDo string
}

// NetworkPolicyChoice gives back what the decisions say about unenforced NetworkPolicies, Require when nothing does
func NetworkPolicyChoice(ds []Decision) string {
	for _, d := range ds {
		if d.Feature == NetworkPolicyFeature && d.choice() != "" && !d.Waiting() {
			return d.choice()
		}
	}
	return Require
}

// Undecided gives back the decisions with neither a choice nor a default, which stop a translation
func (r *Result) Undecided() []Decision {
	var out []Decision
	for _, d := range r.Decisions {
		if d.choice() == "" || d.Waiting() {
			out = append(out, d)
		}
	}
	return out
}

// Translate works out the translation of the generic set's objects from one platform to another
// from may be Unknown, for an export of a cluster on no known cloud, then only markers owned by a platform other than the target are rewritten
// earlier holds the decisions from a previous run, their choices carry over by feature
// src says what else the export knows, the source's region, its network policy engine and its held-back cloud-resource objects
func Translate(objs []*unstructured.Unstructured, from, to platform.Distribution, m *capability.Map, earlier []Decision, src Source) (*Result, error) {
	tgt := m.For(to)
	if tgt == nil {
		return nil, fmt.Errorf("no capability map for %s: translate knows %s", to, m.Names("and"))
	}
	if from == to {
		return nil, fmt.Errorf("this export is already for %s", tgt.Name)
	}
	r := &Result{m: m, From: from, To: to, Dir: m.TranslationDir(from, to), Code: m.TranslationCode(from, to)}
	chosen := map[string]Decision{}
	for _, d := range earlier {
		chosen[d.Feature] = d
	}
	t := &translator{m: m, from: from, to: to, tgt: tgt, chosen: chosen, r: r, region: src.Region, kept: map[string]Identity{}, claimMin: claimSizes(objs)}
	for _, o := range objs {
		// invoking object to work out one object's patch and decisions, its defined below
		t.object(o)
	}
	// the held-back cloud-resource objects get their decisions too, theyre not in the generic set so nothing is patched
	for _, o := range src.Held {
		// invoking cloudResource to decide what happens to one, its defined below
		t.cloudResource(o, false)
	}
	// invoking networkPolicies for the decision on the export's NetworkPolicies, its defined below
	t.networkPolicies(objs, src.NetworkPolicy)
	// invoking imagesToCopy for the images the target can't pull as they are, its defined in images.go
	r.Images = imagesToCopy(m, objs, to)
	// a second pass, now that every kept identity is known, gives the Pods using one what its cloud's webhook would have
	// and takes out what an earlier translation injected where its no longer needed, like back on the identity's own cloud
	for _, o := range objs {
		base, ok := podSpecPath(o.GetKind())
		if !ok {
			continue
		}
		sa, _, _ := unstructured.NestedString(o.Object, append(append([]string{}, base...), "serviceAccountName")...)
		if sa == "" {
			sa = "default"
		}
		id, kept := t.kept[o.GetNamespace()+"/"+sa]
		// invoking injected to read what an earlier translation added, its defined in identity.go
		names, already := injected(o)
		switch {
		case already && kept:
			// it carries the token already, from translating to an earlier cloud, and still needs it
		case already:
			// invoking removeFederation to take it out, its defined in identity.go
			t.removeFederation(&r.Patches[t.patchOf(o)], o, names)
		case kept:
			// invoking injectFederation to add the token and environment, its defined in identity.go
			t.injectFederation(&r.Patches[t.patchOf(o)], o, id)
		}
	}
	r.Patches = slices.DeleteFunc(r.Patches, func(p Patch) bool { return len(p.Ops) == 0 && !p.Delete })
	for _, id := range t.kept {
		r.Identities = append(r.Identities, id)
	}
	sort.Slice(r.Identities, func(i, j int) bool {
		return r.Identities[i].Namespace+"/"+r.Identities[i].ServiceAccount < r.Identities[j].Namespace+"/"+r.Identities[j].ServiceAccount
	})
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

// claimSizes gives back the smallest size each StorageClass's claims ask for, in GiB rounded up, from claims and StatefulSets' claim templates
func claimSizes(objs []*unstructured.Unstructured) map[string]int {
	out := map[string]int{}
	note := func(spec map[string]any) {
		cls, _ := spec["storageClassName"].(string)
		size, _, _ := unstructured.NestedString(spec, "resources", "requests", "storage")
		q, err := resource.ParseQuantity(size)
		if cls == "" || err != nil {
			return
		}
		gi := int((q.Value() + (1 << 30) - 1) >> 30)
		if cur, ok := out[cls]; !ok || gi < cur {
			out[cls] = gi
		}
	}
	for _, o := range objs {
		switch o.GetKind() {
		case "PersistentVolumeClaim":
			if spec, ok, _ := unstructured.NestedMap(o.Object, "spec"); ok {
				note(spec)
			}
		case "StatefulSet":
			tpls, _, _ := unstructured.NestedSlice(o.Object, "spec", "volumeClaimTemplates")
			for _, tpl := range tpls {
				if spec, ok := tpl.(map[string]any)["spec"].(map[string]any); ok {
					note(spec)
				}
			}
		}
	}
	return out
}

// patchKey orders patches by kind, namespace and name
func patchKey(p Patch) string { return p.Kind + "/" + p.Namespace + "/" + p.Name }

// translator holds one translation's state
type translator struct {
	m        *capability.Map
	from, to platform.Distribution
	tgt      *capability.Platform
	chosen   map[string]Decision
	r        *Result
	region   string
	// kept are the cloud identities kept, by namespace/serviceaccount
	kept map[string]Identity
	// claimMin is the smallest size, in GiB rounded up, a claim of each StorageClass asks for, by class name
	claimMin map[string]int
}

// capMap gives back the capability map the translation was made with, the embedded one for a Result made another way, like a test's
// the embedded map is checked by its tests before a release, so loading it doesnt fail in a built binary
func (r *Result) capMap() *capability.Map {
	if r.m == nil {
		m, err := capability.Load()
		if err != nil {
			panic(err)
		}
		r.m = m
	}
	return r.m
}

// patchOf gives back the index of the object's patch, starting one when it has none yet
func (t *translator) patchOf(o *unstructured.Unstructured) int {
	gvk := o.GroupVersionKind()
	for i, p := range t.r.Patches {
		if p.Group == gvk.Group && p.Kind == gvk.Kind && p.Namespace == o.GetNamespace() && p.Name == o.GetName() {
			return i
		}
	}
	t.r.Patches = append(t.r.Patches, Patch{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: o.GetNamespace(), Name: o.GetName()})
	return len(t.r.Patches) - 1
}

// decide records a decision and gives back what to act on, the earlier choice when there was one, else the default
// a decision made on a source object that has changed since doesnt carry over: the user's own choice waits for them to choose again, a default is taken again and said
func (t *translator) decide(d Decision) string {
	if prev, ok := t.chosen[d.Feature]; ok {
		changed := prev.Basis != "" && d.Basis != "" && prev.Basis != d.Basis
		switch {
		case changed && prev.Choose != "":
			d.Previous = prev.Choose
		case changed:
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s has changed on the source since it was last translated. The default still applies, %s, so check that still fits", d.Subject(), d.Describe(d.Default)))
		default:
			d.Choose = prev.Choose
			// still waiting from an earlier run, the user hasnt chosen again yet
			if prev.Choose == "" {
				d.Previous = prev.Previous
			}
		}
	}
	t.r.Decisions = append(t.r.Decisions, d)
	if d.Waiting() {
		return ""
	}
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

	// an object of an operator that creates cloud resources, in the generic set of an export from before they were held back
	if op, _ := t.m.CloudResourceOperator(gvk.Group); op != nil {
		// invoking cloudResource to decide what happens to it, its defined below
		t.cloudResource(o, true)
		return
	}
	// an object whose kind belongs to another platform, like an EC2NodeClass, has nothing to run it here
	if owners := t.m.GroupOwners(gvk.Group); t.foreign(owners) {
		what := fmt.Sprintf("%s (%s) is a %s object: on %s nothing serves or acts on it", ref, gvk.Group, names(t.m, owners), t.tgt.Name)
		switch t.decide(Decision{Feature: "object/" + gvk.Kind + "/" + qualified(o), What: what, Options: []string{KeepSource, LeaveOut}, Basis: Fingerprint(o.Object)}) {
		case KeepSource, LeaveOut:
			p.Delete = true
			p.Why = append(p.Why, "left out of the translation, as decided: "+what)
		}
		t.r.Patches = append(t.r.Patches, p)
		return
	}

	// a platform's own objects, like EKS's VPC CNI settings, nothing reads them on another platform
	if own := t.ownObject(o); own != nil {
		p.Delete = true
		why := fmt.Sprintf("left out: %s, which nothing reads on %s", own.What, t.tgt.Name)
		if own.Then != "" {
			why += ". " + own.Then
		}
		p.Why = append(p.Why, why)
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

	// invoking the kind's handler, if translate has one for it, theyre registered in handlers below
	if h := handlers[gvk.Group+"/"+gvk.Kind]; h != nil {
		h(t, &p, ref, o)
	}
	// invoking placement to rewrite the node pool and spot labels a Pod template asks for, its defined in placement.go
	t.placement(&p, ref, o)
	if len(p.Ops) > 0 || p.Delete {
		t.r.Patches = append(t.r.Patches, p)
	}
}

// handler translates one kind of object, writing its patch, notes and decisions
type handler func(t *translator, p *Patch, ref string, o *unstructured.Unstructured)

// handlers are the kinds translate rewrites, by group/kind, a new kind is a handler added here, with what it reads in the platform's YAML
// placement, which rewrites any Pod template, and the rules for every object, like finalizers, run for each object besides
var handlers = map[string]handler{
	"storage.k8s.io/StorageClass": func(t *translator, p *Patch, ref string, o *unstructured.Unstructured) {
		t.storageClass(p, ref, o.Object)
	},
	"/Service":                                       (*translator).service,
	"/ServiceAccount":                                func(t *translator, _ *Patch, ref string, o *unstructured.Unstructured) { t.identity(ref, o) },
	"external-secrets.io/SecretStore":                func(t *translator, _ *Patch, ref string, o *unstructured.Unstructured) { t.secretStore(ref, o) },
	"external-secrets.io/ClusterSecretStore":         func(t *translator, _ *Patch, ref string, o *unstructured.Unstructured) { t.secretStore(ref, o) },
	"networking.k8s.io/Ingress":                      (*translator).ingress,
	"networking.k8s.io/IngressClass":                 (*translator).ingressClass,
	"snapshot.storage.k8s.io/VolumeSnapshotClass":    (*translator).snapshotClass,
	"secrets-store.csi.x-k8s.io/SecretProviderClass": (*translator).secretProviderClass,
}

// ownObject gives back the platform's own object o is, when its another platform's than the target's, nil when it isnt one
func (t *translator) ownObject(o *unstructured.Unstructured) *capability.OwnObject {
	for i := range t.m.Platforms {
		pl := &t.m.Platforms[i]
		if pl.Distribution == t.to {
			continue
		}
		for j := range pl.Translate.LeaveOut {
			own := &pl.Translate.LeaveOut[j]
			if o.GroupVersionKind().Group == own.Group && o.GetKind() == own.Kind && o.GetNamespace() == own.Namespace && o.GetName() == own.Name {
				return own
			}
		}
	}
	return nil
}

// cloudResource decides what happens to an object of an operator that creates cloud resources, like an ACK Bucket
// on the target's own cloud its operator runs there, and the export's RESTORE.md moves it one controller at a time, so theres nothing to decide
// on another cloud theres no safe default: keep-source leaves it with the source cluster, leave-out drops it, and where SetDrift knows the target's equivalent, equivalent writes it
// inSet is true for an object in the generic set, of an export from before they were held back, its taken out of the translation unless its the target's own
func (t *translator) cloudResource(o *unstructured.Unstructured, inSet bool) {
	gvk := o.GroupVersionKind()
	op, owner := t.m.CloudResourceOperator(gvk.Group)
	ref := gvk.Kind + " " + qualified(o)
	cr := CloudResource{Ref: ref, Operator: op.Name, Cloud: t.m.CloudName(owner.Distribution), Retain: op.Retain.String()}
	if owner.Distribution == t.to {
		t.r.CloudResources = append(t.r.CloudResources, cr)
		return
	}
	// invoking bucketEquivalent for the target's equivalent where SetDrift knows one, its defined in buckets.go
	eq, eqNotes := bucketEquivalent(t.m, o, t.to)
	options := []string{KeepSource, LeaveOut}
	offer := ""
	if eq != nil {
		options = []string{KeepSource, Equivalent, LeaveOut}
		offer = fmt.Sprintf("; equivalent writes %s's own, a new, empty %s with the same name, versioning and public access", t.tgt.Name, strings.ToLower(eq[len(eq)-1].GetKind()))
	}
	what := fmt.Sprintf("%s (%s) manages a real %s resource through %s, which doesn't run on %s. keep-source leaves it with the source cluster, whose operator keeps managing it; leave-out drops it%s",
		ref, gvk.Group, cr.Cloud, op.Name, t.tgt.Name, offer)
	c := t.decide(Decision{Feature: "cloud-resource/" + gvk.Kind + "/" + qualified(o), What: what, Options: options, Basis: Fingerprint(o.Object)})
	cr.Choice = c
	if inSet && c != "" {
		p := Patch{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: o.GetNamespace(), Name: o.GetName(), Delete: true}
		p.Why = append(p.Why, fmt.Sprintf("left out of the translation, as decided (%s): it manages a real %s resource through %s", c, owner.Cloud, op.Name))
		t.r.Patches = append(t.r.Patches, p)
	}
	if c == Equivalent {
		t.r.Added = append(t.r.Added, eq...)
		for _, a := range eq {
			cr.Equivalent = append(cr.Equivalent, a.GetKind()+" "+qualified(a))
		}
		for _, n := range eqNotes {
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s's equivalent (%s): %s", ref, strings.Join(cr.Equivalent, ", "), n))
		}
	}
	t.r.CloudResources = append(t.r.CloudResources, cr)
}

// networkPolicies records the decision on the export's NetworkPolicies when the source enforced them
// translate can't see the target, so setdrift verify checks it there, and fails when the target doesnt enforce them unless the decision is accept-unenforced
func (t *translator) networkPolicies(objs []*unstructured.Unstructured, engine string) {
	n := 0
	for _, o := range objs {
		if o.GroupVersionKind().Group == "networking.k8s.io" && o.GetKind() == "NetworkPolicy" {
			n++
		}
	}
	if n == 0 {
		return
	}
	t.r.NetworkPolicies = n
	// checking if the export doesnt say whether the source enforced them, if so theres no decision, only a note
	if engine == netpolicy.Unknown {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("the export doesn't record whether the source enforced its %s (export again with this version to record it). Make sure %s enforces them: %s",
			text.Plural(n, "NetworkPolicy", "NetworkPolicies"), t.tgt.Name, t.tgt.Translate.NetworkPolicy.HowToEnforce))
		return
	}
	if !netpolicy.Enforced(engine) {
		return
	}
	t.r.NetworkPolicyEngine = engine
	what := fmt.Sprintf("%s, enforced on the source by %s. On a %s cluster that doesn't enforce them, they apply and do nothing. require has setdrift verify fail on such a cluster; accept-unenforced lets it pass, for an emergency, and verify says so every run",
		text.Plural(n, "NetworkPolicy", "NetworkPolicies"), engine, t.tgt.Name)
	// the decision is made on the engine and the policies themselves, a policy added or changed since is a new question
	var policies []any
	for _, o := range objs {
		if o.GroupVersionKind().Group == "networking.k8s.io" && o.GetKind() == "NetworkPolicy" {
			policies = append(policies, o.Object)
		}
	}
	t.decide(Decision{Feature: NetworkPolicyFeature, What: what, Options: []string{Require, AcceptUnenforced}, Default: Require, Basis: Fingerprint([]any{engine, policies})})
}

// storageClass rewrites a class of another platform's provisioner to the target's class of the same tier, keeping its name so claims need no change
// the source's IOPS and throughput carry over where both classes set them, within the target's limits, and what can't come over is said, a class can't always map 1:1 or both ways
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
	ann, _ := c["metadata"].(map[string]any)["annotations"].(map[string]any)
	// invoking sourceClass to read what an earlier translation recorded of the original class, its defined below
	if orig, ok := sourceClass(ann); ok {
		// checking if its going back to the original's platform, if so the original is restored exactly and the record goes
		if platform.Distribution(orig.Platform) == t.to {
			p.Ops = append(p.Ops, Op{Op: "replace", Path: "/provisioner", Value: orig.Provisioner})
			restored := map[string]any{}
			for k, v := range orig.Parameters {
				restored[k] = v
			}
			p.Ops = append(p.Ops, Op{Op: "add", Path: "/parameters", Value: restored})
			p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape(capability.SourceClassAnnotation)})
			p.Why = append(p.Why, fmt.Sprintf("the original %s class an earlier translation recorded is restored exactly, provisioner %s with %s", t.tgt.Name, orig.Provisioner, formatParams(orig.Parameters)))
			return
		}
		// going on to another platform, the original is the truest source, not the translation of it
		prov, params = orig.Provisioner, orig.Parameters
	} else {
		// recording the original, so translating back restores it exactly
		// the error is dropped: marshalling plain maps, strings and slices can't fail
		rec, _ := json.Marshal(classRecord{Platform: string(t.from), Provisioner: prov, Parameters: params})
		if ann == nil {
			p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations", Value: map[string]any{capability.SourceClassAnnotation: string(rec)}})
		} else {
			p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations/" + escape(capability.SourceClassAnnotation), Value: string(rec)})
		}
	}
	// invoking tierOf to recognize the source class's kind of storage, its defined below
	tier, src := t.tierOf(prov, params)
	tt := t.targetTier(tier)
	if tt == nil {
		what := fmt.Sprintf("%s uses provisioner %s with parameters %v, and SetDrift knows no %s class of the same kind", ref, prov, params, t.tgt.Name)
		if t.decide(Decision{Feature: "storage/" + qualifiedName(p), What: what, Options: []string{LeaveOut}, Basis: Fingerprint(c)}) == LeaveOut {
			p.Delete = true
			p.Why = append(p.Why, "left out, as decided: "+what)
		}
		return
	}
	var why []string
	// the source's IOPS and throughput, where its class sets them
	iops, thr, perGi := perfOf(src, params)
	// checking if the IOPS are above the target tier's, if so the tier it promotes to is written, like Ultra Disk above Premium SSD v2's 80,000
	if iops > 0 && tt.PromoteAbove > 0 && iops > tt.PromoteAbove {
		if up := t.targetTier(tt.Promote); up != nil {
			why = append(why, fmt.Sprintf("%d IOPS is above %d, the most the %s class takes, so it becomes the %s class", iops, tt.PromoteAbove, tt.Tier, up.Tier))
			tt = up
		}
	}
	// checking if a claim of the class asks for less than the target class gives, if so its fallback is written, like gp3 for claims under st1's 125 GiB
	if tt.MinSizeGi > 0 && tt.Fallback != "" {
		if smallest, ok := t.claimMin[qualifiedName(p)]; ok && smallest < tt.MinSizeGi {
			if fb := t.targetTier(tt.Fallback); fb != nil {
				why = append(why, fmt.Sprintf("a claim of this class asks for %d GiB, under the %d GiB the %s class starts at, so it becomes the %s class", smallest, tt.MinSizeGi, tt.Tier, fb.Tier))
				tt = fb
			}
		}
	}
	newParams := map[string]any{}
	for k, v := range tt.Class.Parameters {
		newParams[k] = v
	}
	// invoking carry to set the IOPS and throughput on the target's class, or say why they couldn't, its defined below
	why = append(why, t.carry(ref, tt, newParams, iops, thr, perGi)...)
	// the CSI parameters every driver reads, like the filesystem type, carry over as they are, anything else of the source's is said, not dropped silently
	var dropped []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		switch {
		case strings.HasPrefix(k, "csi.storage.k8s.io/"):
			newParams[k] = params[k]
		case !sourceKnows(src, k):
			dropped = append(dropped, k+"="+params[k])
		}
	}
	if len(dropped) > 0 {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s's settings %s don't carry over to %s's class: set what the workload needs by hand, like encryption with a key of the target cloud's", ref, strings.Join(dropped, ", "), t.tgt.Name))
	}
	p.Ops = append(p.Ops, Op{Op: "replace", Path: "/provisioner", Value: tt.Class.Provisioner})
	p.Ops = append(p.Ops, Op{Op: "add", Path: "/parameters", Value: newParams})
	if ann != nil && ann["storageclass.kubernetes.io/is-default-class"] != nil {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape("storageclass.kubernetes.io/is-default-class")})
		p.Why = append(p.Why, fmt.Sprintf("no longer marked the default class: %s keeps its own default, and two defaults would make claims that name no class land on either", t.tgt.Name))
	}
	shown := map[string]string{}
	for k, v := range newParams {
		shown[k] = fmt.Sprint(v)
	}
	p.Why = append(p.Why, fmt.Sprintf("provisioner %s (%s) becomes %s with %s, the same kind of storage on %s; the class keeps its name, so claims need no change", prov, tier, tt.Class.Provisioner, formatParams(shown), t.tgt.Name))
	p.Why = append(p.Why, why...)
	// checking if the source keeps its disks in several zones, if so its said loudly, the target's class doesnt
	if v, ok := zoneValue(src, params); ok {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("ZONE REDUNDANCY LOST: %s keeps its disks in several zones (%s), and %s's class keeps each disk in one zone, so a zone outage takes its volumes down. Replicate the data across zones yourself, like a database's replicas, or use the shared-file class for shared data", ref, v, t.tgt.Name))
		t.r.Lost = append(t.r.Lost, Lost{What: ref + "'s zone redundancy", Why: fmt.Sprintf("%s disks are kept in several zones, %s's in one", v, t.tgt.Name), ToDo: "Replicate the data across zones, like a database's replicas, or use shared files"})
	}
	if strings.Contains(fmt.Sprint(tt.Class.Parameters), "<") {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: fill in the values in angle brackets in the overlay's kustomization.yaml before applying", ref))
	}
}

// classRecord is what SourceClassAnnotation holds, a StorageClass as it was before its first translation
type classRecord struct {
	Platform    string            `json:"platform"`
	Provisioner string            `json:"provisioner"`
	Parameters  map[string]string `json:"parameters,omitempty"`
}

// sourceClass reads the original class an earlier translation recorded, ok false when theres none or it doesnt parse
func sourceClass(ann map[string]any) (classRecord, bool) {
	v, _ := ann[capability.SourceClassAnnotation].(string)
	var rec classRecord
	if v == "" || json.Unmarshal([]byte(v), &rec) != nil || rec.Provisioner == "" {
		return classRecord{}, false
	}
	if rec.Parameters == nil {
		rec.Parameters = map[string]string{}
	}
	return rec, true
}

// zoneValue gives back the source class's parameter value when it keeps disks in several zones
func zoneValue(src *capability.StorageTier, params map[string]string) (string, bool) {
	if src == nil {
		return "", false
	}
	v, ok := param(params, src.Parameter)
	return v, ok && slices.ContainsFunc(src.ZoneRedundant, func(z string) bool { return strings.EqualFold(z, v) })
}

// sourceKnows tells if a parameter is one the source's tier reads, its tier parameter, its shape or its performance settings, which the translation accounts for
func sourceKnows(src *capability.StorageTier, k string) bool {
	if src == nil {
		return false
	}
	known := append([]string{src.Parameter}, src.Shape...)
	if pf := src.Performance; pf != nil {
		known = append(known, pf.IOPS, pf.Throughput, pf.IOPSPerGi)
	}
	// Azure's skuName is also written skuname and storageaccounttype
	if strings.EqualFold(src.Parameter, "skuName") {
		known = append(known, "storageaccounttype")
	}
	return slices.ContainsFunc(known, func(x string) bool { return x != "" && strings.EqualFold(x, k) })
}

// targetTier gives back the target's tier by name, nil when it has none
func (t *translator) targetTier(name string) *capability.StorageTier {
	for i := range t.tgt.Translate.Storage {
		if t.tgt.Translate.Storage[i].Tier == name {
			return &t.tgt.Translate.Storage[i]
		}
	}
	return nil
}

// perfOf reads a source class's IOPS, throughput and IOPS per GiB, zero for each it doesnt set
func perfOf(src *capability.StorageTier, params map[string]string) (iops, thr, perGi int) {
	if src == nil || src.Performance == nil {
		return 0, 0, 0
	}
	num := func(name string) int {
		if name == "" {
			return 0
		}
		v, ok := param(params, name)
		if !ok {
			return 0
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	}
	return num(src.Performance.IOPS), num(src.Performance.Throughput), num(src.Performance.IOPSPerGi)
}

// carry sets the source's IOPS and throughput on the target's class parameters, capped at its limits, and gives back what it says about them
// a target class without performance settings, or a value set per GiB, cant take them, and thats a note to set them by hand
func (t *translator) carry(ref string, tt *capability.StorageTier, params map[string]any, iops, thr, perGi int) []string {
	var why []string
	if perGi > 0 {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s sets %d IOPS per GiB, which %s's class doesn't read: set its IOPS by hand for the size its claims ask for", ref, perGi, t.tgt.Name))
	}
	if iops == 0 && thr == 0 {
		return nil
	}
	pf := tt.Performance
	if pf == nil {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s sets %s, which %s's %s class doesn't take from a StorageClass: set them by hand if the workload needs them", ref, perfText(iops, thr), t.tgt.Name, tt.Tier))
		return nil
	}
	set := func(name string, v, maxV, base int, what string) {
		// checking if theres nothing to carry, or the value is what both classes give anyway, if so the target's class stays as it is
		if v == 0 || (base > 0 && v == base) {
			return
		}
		if name == "" {
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s sets %s %d, which %s's %s class sets by its IOPS instead", ref, what, v, t.tgt.Name, tt.Tier))
			return
		}
		if maxV > 0 && v > maxV {
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s sets %s %d, above %d, the most %s's %s class takes, so it's capped at %d", ref, what, v, maxV, t.tgt.Name, tt.Tier, maxV))
			v = maxV
		}
		params[name] = strconv.Itoa(v)
	}
	set(pf.IOPS, iops, pf.MaxIOPS, pf.BaseIOPS, "IOPS")
	set(pf.Throughput, thr, pf.MaxThroughput, pf.BaseThroughput, "throughput")
	why = append(why, fmt.Sprintf("%s carried over", perfText(iops, thr)))
	return why
}

// perfText writes IOPS and throughput for people
func perfText(iops, thr int) string {
	var parts []string
	if iops > 0 {
		parts = append(parts, fmt.Sprintf("%d IOPS", iops))
	}
	if thr > 0 {
		parts = append(parts, fmt.Sprintf("throughput %d", thr))
	}
	return strings.Join(parts, " and ")
}

// tierOf recognizes a class's kind of storage from any platform's tiers, by provisioner and parameter, with the tier it matched, empty and nil when none matches
func (t *translator) tierOf(prov string, params map[string]string) (string, *capability.StorageTier) {
	for i := range t.m.Platforms {
		for j := range t.m.Platforms[i].Translate.Storage {
			st := &t.m.Platforms[i].Translate.Storage[j]
			if !slices.Contains(st.Provisioners, prov) {
				continue
			}
			// invoking param to read the tier's parameter without regard to case, its defined below
			v, ok := param(params, st.Parameter)
			switch {
			case st.Parameter == "":
				return st.Tier, st
			case ok && slices.ContainsFunc(st.Values, func(x string) bool { return strings.EqualFold(x, v) }):
				return st.Tier, st
			case !ok && st.Default:
				return st.Tier, st
			}
		}
	}
	return "", nil
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
	want := t.tgt.Translate.LoadBalancer.Class
	switch {
	// the target names its own class, like EKS Auto Mode's eks.amazonaws.com/nlb, so a Service without one or with another platform's gets it
	case want != "" && cls == "":
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/spec/loadBalancerClass", Value: want})
		p.Why = append(p.Why, fmt.Sprintf("load balancer class %s added: %s's load balancer controller serves it", want, t.tgt.Name))
	case want != "" && t.foreign(t.m.LoadBalancerClassOwners(cls)):
		p.Ops = append(p.Ops, Op{Op: "replace", Path: "/spec/loadBalancerClass", Value: want})
		p.Why = append(p.Why, fmt.Sprintf("load balancer class %s becomes %s, %s's", cls, want, t.tgt.Name))
	case cls != "" && t.foreign(t.m.LoadBalancerClassOwners(cls)):
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/spec/loadBalancerClass"})
		p.Why = append(p.Why, fmt.Sprintf("load balancer class %s removed: %s gives a Service its load balancer without one", cls, t.tgt.Name))
	}
	// every other platform's load balancer annotation goes, the target reads none of them
	for _, k := range slices.Sorted(maps.Keys(ann)) {
		if t.foreign(t.m.ServiceAnnotationOwners(k)) {
			p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape(k)})
		}
	}
	wantAnn := t.tgt.Translate.LoadBalancer.External
	if exp == "internal" {
		wantAnn = t.tgt.Translate.LoadBalancer.Internal
	}
	for _, k := range slices.Sorted(maps.Keys(wantAnn)) {
		if len(ann) == 0 && !hasAddedAnnotations(p) {
			p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations", Value: map[string]any{}})
		}
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations/" + escape(k), Value: wantAnn[k]})
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
		if t.decide(Decision{Feature: "identity/" + qualified(o), What: what, Options: []string{KeepSource, LeaveOut}, Default: KeepSource, Basis: Fingerprint(o.Object)}) == KeepSource {
			// invoking keptIdentity to read the identity, its defined in identity.go
			if id, ok := keptIdentity(t.m, o); ok {
				t.kept[o.GetNamespace()+"/"+o.GetName()] = id
			}
			t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s keeps its %s identity: in %s, trust the %s cluster's OIDC issuer for it, as RESTORE.md shows", ref, src, src, t.tgt.Name))
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
		t.decide(Decision{Feature: "secret-store/" + qualified(o), What: what, Options: []string{KeepSource, Equivalent}, Default: KeepSource, Basis: Fingerprint(o.Object)})
	}
}

// clouds writes the clouds of platforms for people, like AWS, for wording about their services
func clouds(m *capability.Map, ds []platform.Distribution) string {
	var out []string
	for _, d := range ds {
		if c := m.CloudName(d); !slices.Contains(out, c) {
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
