// tests the hygiene report, terminated Pods by cause and deletions stuck on finalizers

package hygiene

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// obj builds an Object of the given type, name and uid in namespace shop, with owner as its controller when given
func obj(apiVersion, kind, name, uid string, owner *metav1.OwnerReference) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetUID(types.UID(uid))
	if owner != nil {
		yes := true
		owner.Controller = &yes
		u.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	return resource.New(u)
}

// pod builds a Pod in the given phase, with owner as its controller when given
func pod(name, phase string, owner *metav1.OwnerReference) *resource.Object {
	o := obj("v1", "Pod", name, name, owner)
	_ = unstructured.SetNestedField(o.Object, phase, "status", "phase")
	return o
}

// finished marks a Job Complete
func finished(o *resource.Object) *resource.Object {
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Complete", "status": "True"}}, "status", "conditions")
	return o
}

// check that terminated Pods are counted by what keeps them, CronJob history, Jobs with no TTL, no owner or a workload, and the ones their Job will remove are counted apart
func TestTerminatedPodsByCause(t *testing.T) {
	cronJobJob := obj("batch/v1", "Job", "nightly-1", "j1", &metav1.OwnerReference{Kind: "CronJob", Name: "nightly", UID: "cj"})
	// CronJob children are metadata-only
	cronJobJob.Partial = true
	noTTL := finished(obj("batch/v1", "Job", "migrate", "j2", nil))
	withTTL := finished(obj("batch/v1", "Job", "reindex", "j3", nil))
	_ = unstructured.SetNestedField(withTTL.Object, int64(60), "spec", "ttlSecondsAfterFinished")
	running := obj("batch/v1", "Job", "backfill", "j4", nil)

	objs := []*resource.Object{cronJobJob, noTTL, withTTL, running,
		pod("a", "Succeeded", &metav1.OwnerReference{Kind: "Job", UID: "j1"}),
		pod("b", "Failed", &metav1.OwnerReference{Kind: "Job", UID: "j2"}),
		pod("c", "Succeeded", &metav1.OwnerReference{Kind: "Job", UID: "j3"}),
		pod("d", "Failed", &metav1.OwnerReference{Kind: "Job", UID: "j4"}),
		pod("e", "Failed", &metav1.OwnerReference{Kind: "ReplicaSet", UID: "rs"}),
		pod("f", "Succeeded", nil),
		pod("g", "Running", nil),
	}
	_, findings, err := Reporter{}.Process(context.Background(), objs)
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, f := range findings {
		if f.Code == "TERMINATED_PODS_TO_CLEANUP" {
			msg = f.Message
		}
	}
	for _, want := range []string{
		"4 terminated Pods", "(3 Succeeded, 3 Failed)", "1 kept by CronJob history limits",
		"1 kept by finished Jobs with no ttlSecondsAfterFinished", "1 with no owner",
		"1 from ReplicaSets", "2 Pods more will be removed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// check that only a deletion waiting on finalizers for over an hour is reported stuck, and nothing is reported without a capture time
func TestStuckDeletions(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	deleting := func(name string, ago time.Duration, finalizers ...string) *resource.Object {
		o := obj("v1", "ConfigMap", name, name, nil)
		ts := metav1.NewTime(now.Add(-ago))
		o.SetDeletionTimestamp(&ts)
		o.SetFinalizers(finalizers)
		return o
	}
	objs := []*resource.Object{
		deleting("stuck", 72*time.Hour, "example.com/cleanup"),
		deleting("recent", time.Minute, "example.com/cleanup"),
		deleting("no-finalizers", 72*time.Hour),
	}
	_, findings, _ := Reporter{}.Process(resource.WithCaptureTime(context.Background(), now), objs)
	var stuck []string
	for _, f := range findings {
		if f.Code == "DELETION_STUCK" {
			stuck = append(stuck, f.Message)
		}
	}
	if len(stuck) != 1 || !strings.Contains(stuck[0], "shop/stuck has been deleting for 3 days") {
		t.Errorf("DELETION_STUCK = %q", stuck)
	}
	// without a capture time theres nothing to compare with
	if _, findings, _ := (Reporter{}).Process(context.Background(), objs); len(findings) != 0 {
		t.Errorf("findings without a capture time: %v", findings)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
