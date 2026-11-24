// Package normalize reduces objects to what a person or tool declared, so an export applies cleanly to another cluster and compares with Git without noise
// it removes server-set metadata, status, the API server's defaults and what controllers wrote
// the Normalizer stage, its per-kind rules table and the tool markers it strips live here, the defaults in defaults.go and the controller field pruning in managedfields.go
package normalize

import (
	"cmp"
	"context"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// serverMetadata lists the metadata fields the API server sets, Process removes each of them
var serverMetadata = []string{
	"managedFields",
	"uid",
	"resourceVersion",
	"generation",
	"creationTimestamp",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"selfLink",
	// the child objects are already removed, this accounts for the missing parents during ownership checks
	"ownerReferences",
}

// rule removes the server-generated fields of one kind
// rules run before the generic metadata stripping, so they can still read managedFields and status
type rule func(o *resource.Object)

// rules maps each kind to its rules, for server-set values beyond the generic metadata
// the GroupKind vars and the rule functions are defined in defaults.go, except jobGeneratedSelector which is defined below
var rules = map[schema.GroupKind][]rule{
	jobGK:         {jobGeneratedSelector, jobDefaults},
	cronJobGK:     {cronJobDefaults},
	serviceGK:     {serviceDefaults},
	namespaceGK:   {namespaceDefaults},
	pvcGK:         {pvcDefaults},
	deploymentGK:  {replicasDefault, deploymentDefaults},
	replicaSetGK:  {replicasDefault},
	statefulSetGK: {replicasDefault, statefulSetDefaults},
	daemonSetGK:   {daemonSetDefaults},
	secretGK:      {secretDefaults},
	netpolGK:      {networkPolicyDefaults},
	storageGK:     {storageClassDefaults},
	podGK:         {podDefaults},
	pvGK:          {pvDefaults},
	priorityGK:    {priorityClassDefaults},
	mutatingGK:    {injectedCABundles},
	validatingGK:  {injectedCABundles},
	apiServiceGK:  {injectedCABundles},
	crdGK:         {injectedCABundles},
	argoAppGK:     {argoOperation},
}

// argoAppGK is the GroupKind of an Argo CD Application, for looking up its rules above
var argoAppGK = schema.GroupKind{Group: "argoproj.io", Kind: "Application"}

// argoOperation removes an Argo CD Application's operation, which the application controller writes while it syncs and removes once done
// an export taken during a sync captures it, and it would show in verify and diff as a difference a moment later, like status
func argoOperation(o *resource.Object) {
	unstructured.RemoveNestedField(o.Object, "operation")
}

// secretGK is the GroupKind of a Secret, for looking up its rules above
var secretGK = schema.GroupKind{Kind: "Secret"}

// toolAnnotations and toolLabels are the markers GitOps tools and Helm stamp on what they apply, users dont write them, the tool adds them on every apply
// provenance has read them by now, the snapshot keeps them as its evidence, so they are removed from the manifest
// kept in Git they would make every managed object differ from its source in a drift comparison, and mislead whichever tool applies the export next, like Helm annotations on an object Argo CD applies
// app.kubernetes.io/instance stays, Argo CD's label tracking uses it but charts set it too as part of their selectors
var (
	toolAnnotations = []string{
		"argocd.argoproj.io/tracking-id",
		"meta.helm.sh/release-name",
		"meta.helm.sh/release-namespace",
		// a CI/CD pipeline's, see provenance.SourceAnnotations in pkg/provenance/provenance.go
		// source-revision changes with every deploy and differs between clusters
		"setdrift.io/source-repo",
		"setdrift.io/source-path",
		"setdrift.io/source-branch",
		"setdrift.io/source-tag",
		"setdrift.io/source-revision",
		"setdrift.io/deployed-by",
		// the Helm post-renderer's, helmsource.ChartRepo in pkg/helmsource/helmsource.go, already read into the inventory
		"setdrift.io/chart-repo",
	}
	toolLabels = []string{
		"kustomize.toolkit.fluxcd.io/name",
		"kustomize.toolkit.fluxcd.io/namespace",
		"helm.toolkit.fluxcd.io/name",
		"helm.toolkit.fluxcd.io/namespace",
	}
)

// defaultArgoInstanceLabel is the same value as provenance.DefaultArgoInstanceLabel in pkg/provenance/provenance.go, the one tool label that stays
const defaultArgoInstanceLabel = "app.kubernetes.io/instance"

// Normalizer is the normalize stage, it strips server-owned state in order, fields only controllers own, per-kind defaults, then status and server-set metadata
// it implements resource.Stage, defined in pkg/resource/resource.go
type Normalizer struct {
	// ControllerManagers are the field managers whose fields are removed when no other manager owns them, nil means Kubernetes' own components
	ControllerManagers map[string]bool
	// ArgoInstanceLabel is the label Argo CD's label tracking uses, if not app.kubernetes.io/instance
	// a custom key is Argo CD's alone, so it is removed like the other tool markers
	ArgoInstanceLabel string
	// KeepSourceDefaults keeps values the source cluster's own defaults set that another cluster may not share, like the storage class a claim got from the default class
	// the claim then asks for the same class by name everywhere, and a target without it needs one of that name
	KeepSourceDefaults bool
}

// Name gives back the stage's name for resource.Stage
func (Normalizer) Name() string { return "normalize" }

// ReplicasFromHPA is the code of the finding on a workload whose replicas were left out because a HorizontalPodAutoscaler sets them
const ReplicasFromHPA = "REPLICAS_FROM_HPA"

// Process takes the collected objects and strips the server-owned state from each in place, for resource.Stage
// it gives back the same objects, any findings are added to the objects themselves
func (n Normalizer) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	controllers := n.ControllerManagers
	// checking if no controller managers were given, if so Kubernetes' own components are used, defaultControllerManagers is defined in managedfields.go
	if controllers == nil {
		controllers = defaultControllerManagers
	}
	// invoking hpaTargets to find the workloads a HorizontalPodAutoscaler scales, its defined in defaults.go
	scaled := hpaTargets(objs)
	// going through each object to normalize it
	for _, o := range objs {
		// invoking pruneControllerFields to remove the fields only controllers own, its defined in managedfields.go
		pruneControllerFields(o, controllers)
		// checking if its a claim and source defaults are kept, if so pvcDefaultsKeeping keeps its storage class, its defined in defaults.go
		// otherwise the kind's rules from the table above run
		if gk := o.GroupVersionKind().GroupKind(); gk == pvcGK && n.KeepSourceDefaults {
			pvcDefaultsKeeping(o, true)
		} else {
			// running each rule for the object's kind
			for _, r := range rules[gk] {
				r(o)
			}
		}
		// checking if a HorizontalPodAutoscaler scales this workload
		if gvk := o.GroupVersionKind(); scaled[scaleTarget{gvk.GroupKind(), o.GetNamespace(), o.GetName()}] {
			// the HorizontalPodAutoscaler sets the replicas, written by it through the scale subresource they are owned by no one
			// checking if replicas are set, if so they are dropped with a finding saying why
			if _, ok, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas"); ok {
				unstructured.RemoveNestedField(o.Object, "spec", "replicas")
				// invoking AddFinding to note the replicas were left out, its defined in pkg/resource/resource.go
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, ReplicasFromHPA,
					"replicas left out: a HorizontalPodAutoscaler scales this workload")
			}
		}
		// invoking podSpecDefaults to drop the defaults in the pod spec or pod template, its defined in defaults.go
		podSpecDefaults(o)
		// removing status
		unstructured.RemoveNestedField(o.Object, "status")
		// checking if the object has a uid, if so its kept for the snapshot before the server metadata goes
		if uid := string(o.GetUID()); uid != "" {
			o.SourceUID = uid
		}
		// invoking fieldManagers to keep who wrote the object and when, before managedFields goes, its defined below
		o.FieldManagers = fieldManagers(o)
		// removing each server metadata field
		for _, f := range serverMetadata {
			unstructured.RemoveNestedField(o.Object, "metadata", f)
		}
		// checking if the object has a name, if so generateName serves no purpose anymore
		if o.GetName() != "" {
			unstructured.RemoveNestedField(o.Object, "metadata", "generateName")
		}
		// invoking removeToolMarkers to remove the tool markers from the object's own metadata, never from pod templates where a selector may use them, its defined below
		n.removeToolMarkers(o)
		// invoking dropEmpty on annotations, labels and finalizers to remove whichever were left empty, its defined in defaults.go
		for _, f := range []string{"annotations", "labels", "finalizers"} {
			dropEmpty(o.Object, "metadata", f)
		}
	}
	return objs, nil, nil
}

// jobGeneratedLabels are the labels the API server adds to every Job that doesnt set spec.manualSelector
var jobGeneratedLabels = []string{
	"controller-uid",
	"job-name",
	"batch.kubernetes.io/controller-uid",
	"batch.kubernetes.io/job-name",
}

// jobGeneratedSelector takes a Job and removes the selector and labels the API server generated for it, so the export doesnt carry another cluster's uids
func jobGeneratedSelector(o *resource.Object) {
	// checking if the Job sets its own selector, if so the selector and labels are the user's and stay
	if manual, _, _ := unstructured.NestedBool(o.Object, "spec", "manualSelector"); manual {
		return
	}
	unstructured.RemoveNestedField(o.Object, "spec", "selector")
	// going through the Job's own labels and its pod template's labels
	for _, path := range [][]string{
		{"metadata", "labels"},
		{"spec", "template", "metadata", "labels"},
	} {
		labels, found, err := unstructured.NestedStringMap(o.Object, path...)
		// checking if the labels are missing or unreadable, if so theres nothing to remove there
		if !found || err != nil {
			continue
		}
		// deleting each generated label
		for _, k := range jobGeneratedLabels {
			delete(labels, k)
		}
		// checking if no labels are left, if so the map goes, otherwise the rest is written back
		if len(labels) == 0 {
			unstructured.RemoveNestedField(o.Object, path...)
		} else {
			// this cant fail, path was read as a string map right above
			_ = unstructured.SetNestedStringMap(o.Object, labels, path...)
		}
	}
}

// removeToolMarkers takes an object and removes toolAnnotations and toolLabels from its own metadata, and the Helm managed-by label
func (n Normalizer) removeToolMarkers(o *resource.Object) {
	// checking if the object has annotations, if so the tool annotations are deleted from them
	if ann := o.GetAnnotations(); len(ann) > 0 {
		// deleting each tool annotation
		for _, a := range toolAnnotations {
			delete(ann, a)
		}
		o.SetAnnotations(ann)
	}
	labels := o.GetLabels()
	// checking if the object has no labels, if so theres nothing more to remove
	if len(labels) == 0 {
		return
	}
	// deleting each tool label
	for _, l := range toolLabels {
		delete(labels, l)
	}
	// checking if Helm is the one named in managed-by, another tool's value stays
	if labels["app.kubernetes.io/managed-by"] == "Helm" {
		delete(labels, "app.kubernetes.io/managed-by")
	}
	// checking if Argo CD uses a custom instance label, if so its Argo CD's alone and goes too
	if n.ArgoInstanceLabel != "" && n.ArgoInstanceLabel != defaultArgoInstanceLabel {
		delete(labels, n.ArgoInstanceLabel)
	}
	o.SetLabels(labels)
}

// fieldManagers takes an object and gives back its writers from managedFields, one per manager, operation and subresource, oldest first
// only the names, operations, subresources and times are kept, never the fields they own, which can be larger than the object
func fieldManagers(o *resource.Object) []*snapshotv1alpha1.FieldManager {
	var out []*snapshotv1alpha1.FieldManager
	// going through each managedFields entry
	for _, m := range o.GetManagedFields() {
		fm := &snapshotv1alpha1.FieldManager{Manager: m.Manager, Operation: string(m.Operation), Subresource: m.Subresource}
		// checking if the API server recorded a time, if so its kept
		if m.Time != nil {
			fm.Time = timestamppb.New(m.Time.UTC())
		}
		out = append(out, fm)
	}
	// sorting by time, then by manager, so the same object always gives the same order
	slices.SortStableFunc(out, func(a, b *snapshotv1alpha1.FieldManager) int {
		return cmp.Or(a.GetTime().AsTime().Compare(b.GetTime().AsTime()), cmp.Compare(a.Manager, b.Manager),
			cmp.Compare(a.Operation, b.Operation), cmp.Compare(a.Subresource, b.Subresource))
	})
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
