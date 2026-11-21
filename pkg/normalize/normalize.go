// Package normalize reduces objects to what a person or tool declared:
// server-set metadata, status, the API server's defaults and what
// controllers wrote are removed, so an export applies cleanly to another
// cluster and compares with Git without noise.
package normalize

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// metadata fields the API server sets
var serverMetadata = []string{
	"managedFields",
	"uid",
	"resourceVersion",
	"generation",
	"creationTimestamp",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"selfLink",
	// child objects already removed, this accounts for missing parents during ownership checks
	"ownerReferences",
}

// rule removes server-generated fields of one kind. Rules run before the
// generic metadata stripping, so they can still read managedFields and status.
type rule func(o *resource.Object)

// rules are the per-kind rules, for server-set values beyond the generic
// metadata.
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
}

var secretGK = schema.GroupKind{Kind: "Secret"}

// Markers GitOps tools and Helm stamp on what they apply. Users don't write
// them: the tool adds them on every apply. Provenance has read them by now
// (the snapshot keeps them as its evidence), so they are removed from the
// manifest. Kept in Git, they would make every managed object differ from
// its source in a drift comparison, and mislead whichever tool applies the
// export next (Helm annotations on an object Argo CD applies).
//
// app.kubernetes.io/instance stays: Argo CD's label tracking uses it, but
// charts set it too, as part of their selectors.
var (
	toolAnnotations = []string{
		"argocd.argoproj.io/tracking-id",
		"meta.helm.sh/release-name",
		"meta.helm.sh/release-namespace",
		// a CI/CD pipeline's (see provenance.SourceAnnotations): source-revision
		// changes with every deploy, and differs between clusters
		"setdrift.io/source-repo",
		"setdrift.io/source-path",
		"setdrift.io/source-branch",
		"setdrift.io/source-tag",
		"setdrift.io/source-revision",
		"setdrift.io/deployed-by",
		// the Helm post-renderer's (helmsource.ChartRepo), read into the inventory
		"setdrift.io/chart-repo",
	}
	toolLabels = []string{
		"kustomize.toolkit.fluxcd.io/name",
		"kustomize.toolkit.fluxcd.io/namespace",
		"helm.toolkit.fluxcd.io/name",
		"helm.toolkit.fluxcd.io/namespace",
	}
)

// defaultArgoInstanceLabel is provenance.DefaultArgoInstanceLabel, the one
// tool label that stays.
const defaultArgoInstanceLabel = "app.kubernetes.io/instance"

// Normalizer is the normalize stage. It strips server-owned state in order:
// fields only controllers own, per-kind defaults, then status and
// server-set metadata. It implements resource.Stage.
type Normalizer struct {
	// ControllerManagers are the field managers whose fields are removed when
	// no other manager owns them; nil means Kubernetes' own components.
	ControllerManagers map[string]bool
	// ArgoInstanceLabel is the label Argo CD's label tracking uses, if not app.kubernetes.io/instance;
	// a custom key is Argo CD's alone, so it is removed like the other tool markers
	ArgoInstanceLabel string
	// KeepSourceDefaults keeps values the source cluster's own defaults set
	// that another cluster may not share: the storage class a claim got from
	// the default class. The claim then asks for the same class by name
	// everywhere, and a target without it needs one of that name.
	KeepSourceDefaults bool
}

// Name implements resource.Stage.
func (Normalizer) Name() string { return "normalize" }

// ReplicasFromHPA is the code of the finding on a workload whose replicas
// were left out because a HorizontalPodAutoscaler sets them.
const ReplicasFromHPA = "REPLICAS_FROM_HPA"

// Process implements resource.Stage.
func (n Normalizer) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	controllers := n.ControllerManagers
	if controllers == nil {
		controllers = defaultControllerManagers
	}
	scaled := hpaTargets(objs)
	for _, o := range objs {
		// prune controller fields
		pruneControllerFields(o, controllers)
		// apply rules for the objects kind
		if gk := o.GroupVersionKind().GroupKind(); gk == pvcGK && n.KeepSourceDefaults {
			pvcDefaultsKeeping(o, true)
		} else {
			for _, r := range rules[gk] {
				r(o)
			}
		}
		if gvk := o.GroupVersionKind(); scaled[scaleTarget{gvk.GroupKind(), o.GetNamespace(), o.GetName()}] {
			// The HorizontalPodAutoscaler sets the replicas; written by
			// it through the scale subresource, they are owned by no one.
			if _, ok, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas"); ok {
				unstructured.RemoveNestedField(o.Object, "spec", "replicas")
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, ReplicasFromHPA,
					"replicas left out: a HorizontalPodAutoscaler scales this workload")
			}
		}
		// apply default rules for pods
		podSpecDefaults(o)
		// remove status
		unstructured.RemoveNestedField(o.Object, "status")
		// remove server metadata, keeping the UID for the snapshot
		if uid := string(o.GetUID()); uid != "" {
			o.SourceUID = uid
		}
		for _, f := range serverMetadata {
			unstructured.RemoveNestedField(o.Object, "metadata", f)
		}
		// generateName is a leftover once the object has a name
		if o.GetName() != "" {
			unstructured.RemoveNestedField(o.Object, "metadata", "generateName")
		}
		// remove tool markers from the object's own metadata, never from Pod templates, where a selector may use them
		n.removeToolMarkers(o)
		// remove annotations, labels, and finalizers when left empty
		for _, f := range []string{"annotations", "labels", "finalizers"} {
			dropEmpty(o.Object, "metadata", f)
		}
	}
	return objs, nil, nil
}

// jobGeneratedLabels are added by the API server to every Job that does not
// set spec.manualSelector.
var jobGeneratedLabels = []string{
	"controller-uid",
	"job-name",
	"batch.kubernetes.io/controller-uid",
	"batch.kubernetes.io/job-name",
}

// jobGeneratedSelector removes the selector and labels the API server
// generates for a Job.
func jobGeneratedSelector(o *resource.Object) {
	if manual, _, _ := unstructured.NestedBool(o.Object, "spec", "manualSelector"); manual {
		return
	}
	unstructured.RemoveNestedField(o.Object, "spec", "selector")
	for _, path := range [][]string{
		{"metadata", "labels"},
		{"spec", "template", "metadata", "labels"},
	} {
		labels, found, err := unstructured.NestedStringMap(o.Object, path...)
		if !found || err != nil {
			continue
		}
		for _, k := range jobGeneratedLabels {
			delete(labels, k)
		}
		if len(labels) == 0 {
			unstructured.RemoveNestedField(o.Object, path...)
		} else {
			// cannot fail: path was just read as a string map
			_ = unstructured.SetNestedStringMap(o.Object, labels, path...)
		}
	}
}

// removeToolMarkers removes toolAnnotations and toolLabels from o's own
// metadata, and the Helm managed-by label.
func (n Normalizer) removeToolMarkers(o *resource.Object) {
	if ann := o.GetAnnotations(); len(ann) > 0 {
		for _, a := range toolAnnotations {
			delete(ann, a)
		}
		o.SetAnnotations(ann)
	}
	labels := o.GetLabels()
	if len(labels) == 0 {
		return
	}
	for _, l := range toolLabels {
		delete(labels, l)
	}
	if labels["app.kubernetes.io/managed-by"] == "Helm" {
		delete(labels, "app.kubernetes.io/managed-by")
	}
	if n.ArgoInstanceLabel != "" && n.ArgoInstanceLabel != defaultArgoInstanceLabel {
		delete(labels, n.ArgoInstanceLabel)
	}
	o.SetLabels(labels)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
