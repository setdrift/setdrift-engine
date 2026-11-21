// Package collect reads objects from a source, a live cluster or a directory of manifests, for the rest of the pipeline
// the cluster reader is in cluster.go, the directory reader in files.go, the access checks in access.go
package collect

import (
	"context"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Collector is anything that reads objects from a source, ClusterCollector and FileSource both are one
type Collector interface {
	Collect(ctx context.Context) (*Result, error)
}

// Result is what a collection gives back, the objects plus anything that went wrong reading them
type Result struct {
	// everything collected, Partial objects included, the pipeline hands this to redact
	Objects []*resource.Object
	// problems with the collection itself, like LIST_FAILED when a type is forbidden
	Findings []*snapshotv1alpha1.Finding
	// the server's kubernetes version, empty for directories
	KubernetesVersion string
	// Scope is what was read, the whole cluster or which namespaces, and which ones the credentials couldnt read
	Scope *snapshotv1alpha1.ReadScope
}

// warn adds a warning finding with the code and message, a warning and not a terminal error so the collection carries on
func (r *Result) warn(code, msg string) {
	r.Findings = append(r.Findings, &snapshotv1alpha1.Finding{
		Severity: snapshotv1alpha1.Severity_SEVERITY_WARNING,
		Code:     code,
		Message:  msg,
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
