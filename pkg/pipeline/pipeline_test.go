// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

type fixed struct{ res *collect.Result }

func (f fixed) Collect(context.Context) (*collect.Result, error) { return f.res, nil }

func configMap(name string, partial bool) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetNamespace("shop")
	u.SetName(name)
	o := resource.New(u)
	o.Partial = partial
	return o
}

// TestRunLeavesMetaAlone: Run must not change the caller's Meta.
func TestRunLeavesMetaAlone(t *testing.T) {
	cluster := &snapshotv1alpha1.Cluster{Name: "prod"}
	findings := make([]*snapshotv1alpha1.Finding, 1, 10) // spare capacity
	findings[0] = &snapshotv1alpha1.Finding{Code: "CALLER"}
	meta := snapshot.Meta{Cluster: cluster, Findings: findings}

	res := &collect.Result{KubernetesVersion: "v1.35.0", Objects: []*resource.Object{configMap("a", false)},
		Findings: []*snapshotv1alpha1.Finding{{Code: "COLLECT"}}}
	snap, err := pipeline.New(fixed{res}).Run(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if cluster.KubernetesVersion != "" {
		t.Errorf("caller's Cluster changed: %v", cluster)
	}
	if snap.Cluster.KubernetesVersion != "v1.35.0" {
		t.Errorf("snapshot Kubernetes version %q", snap.Cluster.KubernetesVersion)
	}
	if extra := findings[:2][1]; extra != nil {
		t.Errorf("Run wrote into the caller's Findings backing array: %v", extra)
	}
}

// TestPartialWithoutOwnership: a clear error instead of Build's.
func TestPartialWithoutOwnership(t *testing.T) {
	p := pipeline.New(fixed{&collect.Result{Objects: []*resource.Object{configMap("a", true)}}})
	p.Ownership = nil
	_, err := p.Run(context.Background(), snapshot.Meta{})
	if err == nil || !strings.Contains(err.Error(), "Ownership stage is required") {
		t.Errorf("error %v, want one naming the Ownership stage", err)
	}
}
