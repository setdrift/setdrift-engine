// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// defines the object that flows through pipeline
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

// k8s object + app specific metadata
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
	// OwnerUnknown: not checked, e.g. for manifests read from files.
	OwnerUnknown OwnerStatus = iota
	// OwnerDeleted: the owner was deleted, or replaced by a new object of
	// the same name. The object is garbage the garbage collector is
	// deleting.
	OwnerDeleted
	// OwnerNotCollected: the owner's type was not collected (skipped,
	// cluster-scoped while namespaces were requested, not readable, or
	// failed to list).
	OwnerNotCollected
)

// used to wrap manifest in an Object struct, only used by Collectors, everything else gets Objects
func New(u *unstructured.Unstructured) *Object {
	return &Object{Unstructured: u}
}

// attaches a finding to the object, used if object wont be dropped from the snapshot
func (o *Object) AddFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) {
	o.Findings = append(o.Findings, NewFinding(sev, code, format, args...))
}

// constructs reference used in error messages,e.g. apps/Deployment shop/web or ClusterRole system:aggregate-to-shop
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

// stage is a step of the pipeline that modifies objects in place, it returns the object to be passed to the next pipeline step
type Stage interface {
	Name() string
	Process(ctx context.Context, objs []*Object) ([]*Object, []*snapshotv1alpha1.Finding, error)
}

// cluster wide finding, a finding for a resource that will be dropped before the Snapshot is produced
func NewFinding(sev snapshotv1alpha1.Severity, code, format string, args ...any) *snapshotv1alpha1.Finding {
	return &snapshotv1alpha1.Finding{Severity: sev, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Plural formats n with the singular or plural form of a noun, for finding
// messages: Plural(1, "Pod", "Pods") is "1 Pod".
func Plural(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// Verb returns "was" for n == 1 and "were" otherwise.
func Verb(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

type captureTimeKey struct{}

// WithCaptureTime returns a context carrying the time of the capture, so
// stages can tell how old a timestamp in an object is.
func WithCaptureTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, captureTimeKey{}, t)
}

// CaptureTime returns the time WithCaptureTime set, if any.
func CaptureTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(captureTimeKey{}).(time.Time)
	return t, ok && !t.IsZero()
}
