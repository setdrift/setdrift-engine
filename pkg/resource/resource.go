// Package resource defines the object that flows through the pipeline, and
// the Stage interface each pipeline step implements.
package resource

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Object is a Kubernetes object plus the metadata SetDrift attaches to it as it
// moves through the pipeline.
type Object struct {
	*unstructured.Unstructured

	// partial is true for object that will be dropped before the snapshot is built
	Partial bool

	Provenance *snapshotv1alpha1.Provenance
	Redactions []*snapshotv1alpha1.Redaction
	Findings   []*snapshotv1alpha1.Finding

	// OwnerStatus is what the collector found out about a controller owner
	// missing from the collected objects. The ownership stage reads it.
	OwnerStatus OwnerStatus

	// LastApplied is the parsed last-applied-configuration annotation,
	// which the redactor removes. Kept only where a later stage needs it
	// (Services, for node ports); never written to a snapshot or output.
	LastApplied map[string]any

	// HelmRelease is the chart metadata of a Helm release record (a Secret
	// of type helm.sh/release.v1), read by the redactor before it replaces
	// the record's data. Only non-secret chart metadata: no values, no
	// manifests.
	HelmRelease *snapshotv1alpha1.HelmRelease

	// SourceUID is the object's UID in the cluster, kept by the normalizer
	// when it removes metadata.uid from the manifest, for the snapshot's
	// source_uid.
	SourceUID string
}

// OwnerStatus describes a controller owner that is not in the collected
// objects.
type OwnerStatus int

const (
	// OwnerUnknown means not checked, e.g. for manifests read from files.
	OwnerUnknown OwnerStatus = iota
	// OwnerDeleted means the owner was deleted, or replaced by a new object
	// of the same name. The object is garbage the garbage collector is
	// deleting.
	OwnerDeleted
	// OwnerNotCollected means the owner's type was not collected (skipped,
	// cluster-scoped while namespaces were requested, not readable, or
	// failed to list).
	OwnerNotCollected
)

// New wraps a manifest in an Object. Collectors create Objects; every later
// stage receives them.
func New(u *unstructured.Unstructured) *Object {
	return &Object{Unstructured: u}
}

// AddFinding attaches a finding to the object, for an object that stays in
// the snapshot.
func (o *Object) AddFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) {
	o.Findings = append(o.Findings, NewFinding(sev, code, format, args...))
}

// Ref names the object for messages and as its identity in a snapshot:
// [group/]Kind [namespace/]name, e.g. apps/Deployment shop/web or
// ClusterRole system:aggregate-to-shop.
func (o *Object) Ref() string {
	gvk := o.GroupVersionKind()
	gk := gvk.Kind
	if gvk.Group != "" {
		gk = gvk.Group + "/" + gvk.Kind
	}
	if ns := o.GetNamespace(); ns != "" {
		return gk + " " + ns + "/" + o.GetName()
	}
	return gk + " " + o.GetName()
}

// Stage is a step of the pipeline. It may change objects in place, and
// returns the objects to pass to the next step, with findings about the
// whole run.
type Stage interface {
	Name() string
	Process(ctx context.Context, objs []*Object) ([]*Object, []*snapshotv1alpha1.Finding, error)
}

// NewFinding builds a finding about the run as a whole, or about objects the
// stage drops before the snapshot is built.
func NewFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) *snapshotv1alpha1.Finding {
	return &snapshotv1alpha1.Finding{Severity: sev, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ClaimTemplatePrefixes returns, for every StatefulSet volume claim template
// in objs, the name prefix of the claims it creates ("<namespace>/<template>-
// <statefulset>-", the claim's name without its ordinal), mapped to the
// StatefulSet's name.
func ClaimTemplatePrefixes(objs []*Object) map[string]string {
	out := map[string]string{}
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		if gvk.Group != "apps" || gvk.Kind != "StatefulSet" {
			continue
		}
		templates, _, _ := unstructured.NestedSlice(o.Object, "spec", "volumeClaimTemplates")
		for _, t := range templates {
			tm, _ := t.(map[string]any)
			if name, _, _ := unstructured.NestedString(tm, "metadata", "name"); name != "" {
				out[o.GetNamespace()+"/"+name+"-"+o.GetName()+"-"] = o.GetName()
			}
		}
	}
	return out
}

type captureTimeKey struct{}

// WithCaptureTime returns a context carrying the time of the capture, so
// stages can tell how old a timestamp in an object is. A context value
// rather than a Stage parameter: it is the run's, set once, and only the
// stages that judge ages read it.
func WithCaptureTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, captureTimeKey{}, t)
}

// CaptureTime returns the time WithCaptureTime set, if any.
func CaptureTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(captureTimeKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
