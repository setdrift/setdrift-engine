// Package capability knows what belongs to each cloud's Kubernetes, the names and markers only that platform acts on, like a load balancer class, a storage provisioner or an identity annotation
// its the capability map of v0.3.0, as data, one YAML file per platform under platforms/
// Misfits uses it to find the objects that name something the target platform doesnt have, so they would apply cleanly there and not work
package capability

import (
	"embed"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/pkg/platform"
	"sigs.k8s.io/yaml"
)

//go:embed platforms/*.yaml
var files embed.FS

// Platform is one platform's markers, read from its YAML file
type Platform struct {
	// Distribution is the platform, like eks, matching platform.Distribution
	Distribution platform.Distribution `json:"distribution"`
	// Name is how its written for people, like EKS
	Name string `json:"name"`
	// Cloud is the cloud the platform runs on, like AWS, for wording about its services, identities and secret manager
	Cloud     string    `json:"cloud,omitempty"`
	Markers   Markers   `json:"markers"`
	Translate Translate `json:"translate"`
}

// Translate holds how a translation reads this platform's settings and writes them for it
type Translate struct {
	// Dir is where a translation for this platform goes in an export, like platform/gcp/gke
	Dir          string        `json:"dir"`
	Storage      []StorageTier `json:"storage,omitempty"`
	LoadBalancer LoadBalancer  `json:"loadBalancer"`
	Ingress      Ingress       `json:"ingress"`
	// SnapshotDriver is the VolumeSnapshotClass driver of the platform's block storage
	SnapshotDriver string          `json:"snapshotDriver,omitempty"`
	Nodes          NodeTranslation `json:"nodes"`
}

// StorageTier is one kind of storage on a platform, how a StorageClass for it is recognized and how one is written
// tiers are matched by capability, the same tier name on two platforms means the same kind of disk or share
type StorageTier struct {
	// Tier names the kind of storage, hdd, general-ssd, premium-ssd, extreme or shared-file
	Tier string `json:"tier"`
	// Provisioners and the Parameter with one of its Values recognize a class of this tier, matched without regard to case
	Provisioners []string `json:"provisioners"`
	Parameter    string   `json:"parameter,omitempty"`
	Values       []string `json:"values,omitempty"`
	// Default means a class of one of these provisioners without the parameter is this tier, as the driver's default
	Default bool `json:"default,omitempty"`
	// Class is what a StorageClass of this tier is written as on this platform
	Class StorageClass `json:"class"`
}

// StorageClass is a provisioner with its parameters
type StorageClass struct {
	Provisioner string            `json:"provisioner"`
	Parameters  map[string]string `json:"parameters,omitempty"`
}

// LoadBalancer says how a Service's exposure, internal or external, reads and is written on a platform
type LoadBalancer struct {
	// Annotation holds the exposure, with InternalValues or ExternalValues
	Annotation     string   `json:"annotation"`
	InternalValues []string `json:"internalValues,omitempty"`
	ExternalValues []string `json:"externalValues,omitempty"`
	// ClassDefaults is the exposure a load balancer class gives without the annotation, internal or external
	ClassDefaults map[string]string `json:"classDefaults,omitempty"`
	// Default is the exposure with neither, internal or external
	Default string `json:"default"`
	// Internal and External are the annotations a translation writes for each
	Internal map[string]string `json:"internal,omitempty"`
	External map[string]string `json:"external,omitempty"`
}

// Ingress says how an Ingress's controller and exposure read and are written on a platform
type Ingress struct {
	// ClassName is the ingressClassName a translation writes, unless ClassAnnotation says the platform reads the class from the kubernetes.io/ingress.class annotation, with InternalClass or ExternalClass
	ClassName       string `json:"className,omitempty"`
	ClassAnnotation bool   `json:"classAnnotation,omitempty"`
	InternalClass   string `json:"internalClass,omitempty"`
	ExternalClass   string `json:"externalClass,omitempty"`
	// ExposureAnnotation holds the exposure with InternalValues or ExternalValues, Default is the exposure without it
	ExposureAnnotation string   `json:"exposureAnnotation,omitempty"`
	InternalValues     []string `json:"internalValues,omitempty"`
	ExternalValues     []string `json:"externalValues,omitempty"`
	Default            string   `json:"default"`
	// InternalUnsupported means the platform's controller offers no internal Ingress, so an internal one is a decision
	InternalUnsupported bool `json:"internalUnsupported,omitempty"`
	// Internal and External are the annotations a translation writes for each
	Internal map[string]string `json:"internal,omitempty"`
	External map[string]string `json:"external,omitempty"`
}

// IngressClassOwners gives back the platforms an ingress class name belongs to, as the kubernetes.io/ingress.class annotation or ingressClassName names it
func (m *Map) IngressClassOwners(c string) []platform.Distribution {
	return m.owners(c, func(k Markers) []string { return k.IngressClassAnnotations }, false)
}

// IngressControllerOwners gives back the platforms an IngressClass controller belongs to
func (m *Map) IngressControllerOwners(c string) []platform.Distribution {
	return m.owners(c, func(k Markers) []string { return k.IngressControllers }, false)
}

// IngressAnnotationOwners gives back the platforms an Ingress annotation belongs to, by prefix
func (m *Map) IngressAnnotationOwners(a string) []platform.Distribution {
	return m.owners(a, func(k Markers) []string { return k.IngressAnnotationPrefixes }, true)
}

// NodeTranslation says which node labels name a pool and which mark spot capacity on a platform
type NodeTranslation struct {
	// Pools are the label keys that name a node pool, the first is the one a translation writes
	Pools []string `json:"pools,omitempty"`
	// Spot are the labels that mark spot capacity, the first is the one a translation writes
	Spot []SpotLabel `json:"spot,omitempty"`
}

// SpotLabel is a label marking spot capacity, Taint means the platform taints spot nodes with the same key and value, so Pods need a toleration
type SpotLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Taint bool   `json:"taint,omitempty"`
}

// For gives back the platform's entry, nil when the map has none
func (m *Map) For(d platform.Distribution) *Platform {
	for i := range m.Platforms {
		if m.Platforms[i].Distribution == d {
			return &m.Platforms[i]
		}
	}
	return nil
}

// Markers are the names and markers only this platform acts on
// a name in more than one platform's file, like Karpenter's on EKS and AKS, belongs to each of them
type Markers struct {
	FinalizerPrefixes            []string `json:"finalizerPrefixes,omitempty"`
	LoadBalancerClasses          []string `json:"loadBalancerClasses,omitempty"`
	ServiceAnnotationPrefixes    []string `json:"serviceAnnotationPrefixes,omitempty"`
	IngressControllers           []string `json:"ingressControllers,omitempty"`
	IngressAnnotationPrefixes    []string `json:"ingressAnnotationPrefixes,omitempty"`
	IngressClassAnnotations      []string `json:"ingressClassAnnotations,omitempty"`
	GatewayControllerPrefixes    []string `json:"gatewayControllerPrefixes,omitempty"`
	StorageProvisioners          []string `json:"storageProvisioners,omitempty"`
	ServiceAccountAnnotations    []string `json:"serviceAccountAnnotations,omitempty"`
	PodLabels                    []string `json:"podLabels,omitempty"`
	PodAnnotations               []string `json:"podAnnotations,omitempty"`
	NodeLabelPrefixes            []string `json:"nodeLabelPrefixes,omitempty"`
	SecretStoreProviders         []string `json:"secretStoreProviders,omitempty"`
	SecretProviderClassProviders []string `json:"secretProviderClassProviders,omitempty"`
	APIGroupSuffixes             []string `json:"apiGroupSuffixes,omitempty"`
}

// Map is every platform's markers
type Map struct {
	Platforms []Platform
}

// Load reads the platforms embedded in the binary
// its an error only when a file doesnt parse, which a test catches before a release
func Load() (*Map, error) {
	// invoking files.ReadDir to list the embedded platform files
	entries, err := files.ReadDir("platforms")
	if err != nil {
		return nil, err
	}
	m := &Map{}
	for _, e := range entries {
		// invoking files.ReadFile to read one platform's file
		b, err := files.ReadFile(path.Join("platforms", e.Name()))
		if err != nil {
			return nil, err
		}
		var p Platform
		// invoking yaml.UnmarshalStrict so a misspelled key fails instead of being ignored
		if err := yaml.UnmarshalStrict(b, &p); err != nil {
			return nil, fmt.Errorf("capability map %s: %w", e.Name(), err)
		}
		m.Platforms = append(m.Platforms, p)
	}
	return m, nil
}

// name gives back how a distribution is written for people, its own Name when the map has it
func (m *Map) name(d platform.Distribution) string {
	for _, p := range m.Platforms {
		if p.Distribution == d {
			return p.Name
		}
	}
	// checking if the platform package knows the name, if not the bare value is used
	if n := d.Name(); n != "" {
		return n
	}
	return string(d)
}

// owners gives back the platforms whose markers hold a value, picked by pick from each platform's markers, matched exactly or by prefix
func (m *Map) owners(value string, pick func(Markers) []string, prefix bool) []platform.Distribution {
	var out []platform.Distribution
	for _, p := range m.Platforms {
		// checking each of the platform's markers of this kind against the value
		for _, v := range pick(p.Markers) {
			if value == v || (prefix && strings.HasPrefix(value, v)) {
				out = append(out, p.Distribution)
				break
			}
		}
	}
	return out
}

// groupOwners gives back the platforms an API group belongs to, matching the group itself or a suffix after a dot
func (m *Map) groupOwners(group string) []platform.Distribution {
	if group == "" {
		return nil
	}
	var out []platform.Distribution
	for _, p := range m.Platforms {
		for _, s := range p.Markers.APIGroupSuffixes {
			if group == s || strings.HasSuffix(group, "."+s) {
				out = append(out, p.Distribution)
				break
			}
		}
	}
	return out
}

// Misfit is an object that names something the target platform doesnt have, so it would apply there and not work
type Misfit struct {
	// Ref names the object as verify does, like apps/v1 Deployment sd-apps/web
	Ref string
	// Reasons say what it names, whose it is and what happens on the target, one line each
	Reasons []string
}

// Object is the part of an object Misfits reads, an unstructured object's content with its kind and group
type Object struct {
	Ref     string
	Group   string
	Kind    string
	Content map[string]any
}

// Misfits checks objects against the target platform and gives back those that name another platform's markers
// target is the distribution the objects would be applied to, with Unknown nothing is checked, since no platform can be told apart
func (m *Map) Misfits(objs []Object, target platform.Distribution) []Misfit {
	if target == platform.Unknown {
		return nil
	}
	var out []Misfit
	for _, o := range objs {
		// invoking reasons to list what the object names that the target lacks, its defined below
		if rs := m.reasons(o, target); len(rs) > 0 {
			out = append(out, Misfit{Ref: o.Ref, Reasons: rs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// reasons lists what one object names that belongs to other platforms and not the target
func (m *Map) reasons(o Object, target platform.Distribution) []string {
	var rs []string
	// foreign adds a reason when the value belongs to platforms that dont include the target
	foreign := func(what, value string, owners []platform.Distribution, effect string) {
		if len(owners) == 0 || slices.Contains(owners, target) {
			return
		}
		names := make([]string, len(owners))
		for i, d := range owners {
			names[i] = m.name(d)
		}
		rs = append(rs, fmt.Sprintf("%s %s belongs to %s, and on %s %s", what, value, strings.Join(names, " and "), m.name(target), effect))
	}
	c := o.Content

	// a finalizer another platform's controller would remove, on the target nothing does, so deleting the object hangs
	for _, f := range stringsAt(c, "metadata", "finalizers") {
		foreign("finalizer", f, m.owners(f, func(k Markers) []string { return k.FinalizerPrefixes }, true), "nothing removes it, so deleting the object hangs")
	}
	// the object's own kind, like a Karpenter NodePool or an ACK Bucket
	foreign("its API group", o.Group, m.groupOwners(o.Group), "nothing serves or acts on it")

	switch {
	case o.Group == "" && o.Kind == "Service":
		cls, _ := str(c, "spec", "loadBalancerClass")
		foreign("load balancer class", cls, m.owners(cls, func(k Markers) []string { return k.LoadBalancerClasses }, false), "no controller gives the Service an address")
		for _, a := range keys(c, "metadata", "annotations") {
			foreign("annotation", a, m.owners(a, func(k Markers) []string { return k.ServiceAnnotationPrefixes }, true), "nothing reads it, so the load balancer it describes (internal or public, its subnet, its certificate) isn't what you get")
		}
	case o.Group == "networking.k8s.io" && o.Kind == "IngressClass":
		ctl, _ := str(c, "spec", "controller")
		foreign("ingress controller", ctl, m.owners(ctl, func(k Markers) []string { return k.IngressControllers }, false), "no controller serves this class")
	case o.Group == "networking.k8s.io" && o.Kind == "Ingress":
		ann := annotations(c)
		if cls := ann["kubernetes.io/ingress.class"]; cls != "" {
			foreign("ingress class", cls, m.owners(cls, func(k Markers) []string { return k.IngressClassAnnotations }, false), "no controller serves this Ingress")
		}
		for _, a := range sortedKeys(ann) {
			foreign("annotation", a, m.owners(a, func(k Markers) []string { return k.IngressAnnotationPrefixes }, true), "nothing reads it")
		}
	case o.Group == "gateway.networking.k8s.io" && o.Kind == "GatewayClass":
		ctl, _ := str(c, "spec", "controllerName")
		foreign("gateway controller", ctl, m.owners(ctl, func(k Markers) []string { return k.GatewayControllerPrefixes }, true), "no controller serves this class")
	case o.Group == "storage.k8s.io" && o.Kind == "StorageClass":
		p, _ := str(c, "provisioner")
		foreign("provisioner", p, m.owners(p, func(k Markers) []string { return k.StorageProvisioners }, false), "claims for this class wait forever")
	case o.Group == "snapshot.storage.k8s.io" && o.Kind == "VolumeSnapshotClass":
		d, _ := str(c, "driver")
		foreign("snapshot driver", d, m.owners(d, func(k Markers) []string { return k.StorageProvisioners }, false), "no snapshot can be taken")
	case o.Group == "" && o.Kind == "ServiceAccount":
		for _, a := range keys(c, "metadata", "annotations") {
			foreign("identity annotation", a, m.owners(a, func(k Markers) []string { return k.ServiceAccountAnnotations }, false), "nothing gives its Pods those cloud credentials")
		}
	case o.Group == "external-secrets.io" && (o.Kind == "SecretStore" || o.Kind == "ClusterSecretStore"):
		for _, p := range keys(c, "spec", "provider") {
			foreign("secret store provider", p, m.owners(p, func(k Markers) []string { return k.SecretStoreProviders }, false), "it can't authenticate as the source cluster did")
		}
	case o.Group == "secrets-store.csi.x-k8s.io" && o.Kind == "SecretProviderClass":
		p, _ := str(c, "spec", "provider")
		foreign("secret provider", p, m.owners(p, func(k Markers) []string { return k.SecretProviderClassProviders }, false), "no provider for it is installed")
	}

	// a Pod template, in a Pod, a workload or a Job, may pin cloud node labels, a cloud identity label or a cloud volume annotation
	// invoking podSpecAndMeta to find it, its defined below
	if spec, meta := podSpecAndMeta(o); spec != nil {
		for _, l := range keys(meta, "labels") {
			foreign("Pod label", l, m.owners(l, func(k Markers) []string { return k.PodLabels }, false), "nothing acts on it")
		}
		for _, a := range keys(meta, "annotations") {
			foreign("Pod annotation", a, m.owners(a, func(k Markers) []string { return k.PodAnnotations }, false), "nothing acts on it")
		}
		// invoking nodeKeys to list the node labels and taints the Pods ask for, its defined below
		for _, k := range nodeKeys(spec) {
			foreign("node label", k, m.owners(k, func(k Markers) []string { return k.NodeLabelPrefixes }, true), "no node carries it, so its Pods may never schedule, or lose the placement asked for")
		}
	}
	return slices.Compact(rs)
}

// podSpecAndMeta finds the Pod spec and Pod metadata in an object, the object's own for a Pod, the template's for workloads, a CronJob's job template's
func podSpecAndMeta(o Object) (spec, meta map[string]any) {
	c := o.Content
	switch o.Kind {
	case "Pod":
		spec, _ = c["spec"].(map[string]any)
		meta, _ = c["metadata"].(map[string]any)
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "ReplicationController":
		spec = mapAt(c, "spec", "template", "spec")
		meta = mapAt(c, "spec", "template", "metadata")
	case "CronJob":
		spec = mapAt(c, "spec", "jobTemplate", "spec", "template", "spec")
		meta = mapAt(c, "spec", "jobTemplate", "spec", "template", "metadata")
	}
	return spec, meta
}

// nodeKeys lists the node label keys a Pod spec selects on or tolerates, from its nodeSelector, its node affinity and its tolerations, sorted
func nodeKeys(spec map[string]any) []string {
	set := map[string]bool{}
	for _, k := range keys(spec, "nodeSelector") {
		set[k] = true
	}
	// each node selector term of required and preferred node affinity, its match expressions name label keys
	terms, _ := anyAt(spec, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
	if prefs, ok := anyAt(spec, "affinity", "nodeAffinity", "preferredDuringSchedulingIgnoredDuringExecution").([]any); ok {
		for _, p := range prefs {
			if pm, ok := p.(map[string]any); ok {
				terms = append(terms, pm["preference"])
			}
		}
	}
	for _, t := range terms {
		tm, _ := t.(map[string]any)
		exprs, _ := tm["matchExpressions"].([]any)
		for _, e := range exprs {
			if em, ok := e.(map[string]any); ok {
				if k, ok := em["key"].(string); ok {
					set[k] = true
				}
			}
		}
	}
	tols, _ := spec["tolerations"].([]any)
	for _, t := range tols {
		if tm, ok := t.(map[string]any); ok {
			if k, ok := tm["key"].(string); ok && k != "" {
				set[k] = true
			}
		}
	}
	return sortedKeys(set)
}

// anyAt walks nested maps by keys and gives back what is there, nil when a step is missing
func anyAt(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = cm[p]
	}
	return cur
}

// mapAt is anyAt for a map
func mapAt(m map[string]any, path ...string) map[string]any {
	v, _ := anyAt(m, path...).(map[string]any)
	return v
}

// str is anyAt for a string
func str(m map[string]any, path ...string) (string, bool) {
	v, ok := anyAt(m, path...).(string)
	return v, ok
}

// keys gives back the sorted keys of the map at path
func keys(m map[string]any, path ...string) []string {
	return sortedKeys(mapAt(m, path...))
}

// stringsAt gives back the strings in the list at path
func stringsAt(m map[string]any, path ...string) []string {
	l, _ := anyAt(m, path...).([]any)
	var out []string
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// annotations gives back an object's annotations as strings
func annotations(c map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range mapAt(c, "metadata", "annotations") {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// sortedKeys gives back a map's keys in order
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// the owners of each kind of marker, for translate

// GroupOwners gives back the platforms an API group belongs to
func (m *Map) GroupOwners(group string) []platform.Distribution { return m.groupOwners(group) }

// ProvisionerOwners gives back the platforms a storage provisioner belongs to
func (m *Map) ProvisionerOwners(p string) []platform.Distribution {
	return m.owners(p, func(k Markers) []string { return k.StorageProvisioners }, false)
}

// LoadBalancerClassOwners gives back the platforms a load balancer class belongs to
func (m *Map) LoadBalancerClassOwners(c string) []platform.Distribution {
	return m.owners(c, func(k Markers) []string { return k.LoadBalancerClasses }, false)
}

// ServiceAnnotationOwners gives back the platforms a Service annotation belongs to, by prefix
func (m *Map) ServiceAnnotationOwners(a string) []platform.Distribution {
	return m.owners(a, func(k Markers) []string { return k.ServiceAnnotationPrefixes }, true)
}

// ServiceAccountAnnotationOwners gives back the platforms a ServiceAccount's identity annotation belongs to
func (m *Map) ServiceAccountAnnotationOwners(a string) []platform.Distribution {
	return m.owners(a, func(k Markers) []string { return k.ServiceAccountAnnotations }, false)
}

// SecretStoreProviderOwners gives back the platforms an External Secrets provider belongs to
func (m *Map) SecretStoreProviderOwners(p string) []platform.Distribution {
	return m.owners(p, func(k Markers) []string { return k.SecretStoreProviders }, false)
}

// FinalizerOwners gives back the platforms a finalizer belongs to, by prefix
func (m *Map) FinalizerOwners(f string) []platform.Distribution {
	return m.owners(f, func(k Markers) []string { return k.FinalizerPrefixes }, true)
}

// SecretProviderClassOwners gives back the platforms a Secrets Store CSI provider belongs to
func (m *Map) SecretProviderClassOwners(p string) []platform.Distribution {
	return m.owners(p, func(k Markers) []string { return k.SecretProviderClassProviders }, false)
}

// NodeLabelOwners gives back the platforms a node label key belongs to, by prefix
func (m *Map) NodeLabelOwners(k string) []platform.Distribution {
	return m.owners(k, func(k Markers) []string { return k.NodeLabelPrefixes }, true)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
