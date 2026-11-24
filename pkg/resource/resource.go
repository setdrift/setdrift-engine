// Package resource defines the object that flows through the pipeline, and the Stage interface each pipeline step implements
package resource

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Object is a Kubernetes object plus the metadata SetDrift attaches to it as it moves through the pipeline
type Object struct {
	*unstructured.Unstructured

	// true for an object that will be dropped before the snapshot is built
	Partial bool

	// where the object came from, and what was redacted from it and found about it on the way
	Provenance *snapshotv1alpha1.Provenance
	Redactions []*snapshotv1alpha1.Redaction
	Findings   []*snapshotv1alpha1.Finding

	// what the collector found out about a controller owner missing from the collected objects, the ownership stage reads it
	OwnerStatus OwnerStatus

	// the parsed last-applied-configuration annotation, which the redactor removes
	// its kept only where a later stage needs it, Services for their node ports, and never written to a snapshot or output
	LastApplied map[string]any

	// the chart metadata of a Helm release record, a Secret of type helm.sh/release.v1, read by the redactor before it replaces the record's data
	// only the non-secret chart metadata, no values and no manifests
	HelmRelease *snapshotv1alpha1.HelmRelease

	// the object's uid in the cluster, kept by the normalizer when it removes metadata.uid from the manifest, for the snapshot's source_uid
	SourceUID string

	// who wrote the object and when, kept by the normalizer when it removes metadata.managedFields from the manifest, for the snapshot's field_managers
	FieldManagers []*snapshotv1alpha1.FieldManager
}

// OwnerStatus says what happened to a controller owner that is not in the collected objects
type OwnerStatus int

// the OwnerStatus values
const (
	// OwnerUnknown means not checked, like for manifests read from files
	OwnerUnknown OwnerStatus = iota
	// OwnerDeleted means the owner was deleted, or replaced by a new object of the same name
	// the object is garbage the garbage collector is deleting
	OwnerDeleted
	// OwnerNotCollected means the owner's type was not collected, it was skipped, cluster-scoped while namespaces were asked for, not readable, or failed to list
	OwnerNotCollected
)

// New wraps a manifest in an Object, collectors create Objects and every later stage receives them
func New(u *unstructured.Unstructured) *Object {
	return &Object{Unstructured: u}
}

// AddFinding takes a severity, a code and a message and attaches the finding to the object, for an object that stays in the snapshot
func (o *Object) AddFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) {
	// invoking NewFinding to build the finding, its defined below
	o.Findings = append(o.Findings, NewFinding(sev, code, format, args...))
}

// Ref names the object for messages and as its identity in a snapshot, [group/]Kind [namespace/]name
// like apps/Deployment shop/web or ClusterRole system:aggregate-to-shop
func (o *Object) Ref() string {
	gvk := o.GroupVersionKind()
	gk := gvk.Kind
	// checking if the kind has a group, the core group has none and is left out
	if gvk.Group != "" {
		gk = gvk.Group + "/" + gvk.Kind
	}
	// checking if the object is namespaced, if so the namespace goes before the name
	if ns := o.GetNamespace(); ns != "" {
		return gk + " " + ns + "/" + o.GetName()
	}
	return gk + " " + o.GetName()
}

// Stage is a step of the pipeline, it may change objects in place
// Process gives back the objects to pass to the next step, with findings about the run itself
type Stage interface {
	Name() string
	Process(ctx context.Context, objs []*Object) ([]*Object, []*snapshotv1alpha1.Finding, error)
}

// NewFinding takes a severity, a code and a message and builds a finding about the run itself, or about objects the stage drops before the snapshot is built
func NewFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) *snapshotv1alpha1.Finding {
	return &snapshotv1alpha1.Finding{Severity: sev, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ClaimTemplatePrefixes takes the objects and gives back, for every StatefulSet volume claim template, the name prefix of the claims it creates mapped to the StatefulSet's name
// the prefix is "<namespace>/<template>-<statefulset>-", the claim's name without its ordinal
func ClaimTemplatePrefixes(objs []*Object) map[string]string {
	out := map[string]string{}
	// going over each object looking for StatefulSets
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		// checking if its a StatefulSet, if not theres nothing to look at
		if gvk.Group != "apps" || gvk.Kind != "StatefulSet" {
			continue
		}
		templates, _, _ := unstructured.NestedSlice(o.Object, "spec", "volumeClaimTemplates")
		// going over each volume claim template
		for _, t := range templates {
			tm, _ := t.(map[string]any)
			// checking if the template has a name, a nameless one creates no claims we can match
			if name, _, _ := unstructured.NestedString(tm, "metadata", "name"); name != "" {
				out[o.GetNamespace()+"/"+name+"-"+o.GetName()+"-"] = o.GetName()
			}
		}
	}
	return out
}

// captureTimeKey is the context key the capture time is stored under, its own type so nothing else can collide with it
type captureTimeKey struct{}

// WithCaptureTime takes a context and the time of the capture and gives back a context carrying it, so stages can tell how old a timestamp in an object is
// its a context value rather than a Stage parameter because it belongs to the run, set once, and only the stages that judge ages read it
func WithCaptureTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, captureTimeKey{}, t)
}

// CaptureTime gives back the time WithCaptureTime set, and false when none was set or it is zero
func CaptureTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(captureTimeKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
