// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package collect reads objects from a source, a live cluster or a
// directory of manifests, for the rest of the pipeline.
package collect

import (
	"context"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Collector reads objects from a source.
type Collector interface {
	Collect(ctx context.Context) (*Result, error)
}

// Result is the output of a collection.
type Result struct {
	// everything collected including Partial, pipeline hands this to redact
	Objects []*resource.Object
	// problems with the collection itself e.g. LIST_FAILED e.g. forbidden for a type
	Findings []*snapshotv1alpha1.Finding
	// server k8s version, blank for directories
	KubernetesVersion string
	// Scope is what was read: the whole cluster, or which namespaces, and
	// which the credentials could not read.
	Scope *snapshotv1alpha1.ReadScope
}

// warn builds the finding directly, adds WARNING (not error resulting in termination)
func (r *Result) warn(code, msg string) {
	r.Findings = append(r.Findings, &snapshotv1alpha1.Finding{
		Severity: snapshotv1alpha1.Severity_SEVERITY_WARNING,
		Code:     code,
		Message:  msg,
	})
}
