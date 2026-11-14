// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

func obj(apiVersion, kind, name string, ann map[string]string) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetAnnotations(ann)
	return resource.New(u)
}

func withCondition(o *resource.Object, typ string) *resource.Object {
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": typ, "status": "True"}}, "status", "conditions")
	return o
}

func codes(o *resource.Object) map[string]bool {
	out := map[string]bool{}
	for _, f := range o.Findings {
		out[f.Code] = true
	}
	return out
}

func TestFilter(t *testing.T) {
	hook := map[string]string{"argocd.argoproj.io/hook": "PreSync"}
	objs := []*resource.Object{
		withCondition(obj("batch/v1", "Job", "done", nil), "Complete"),
		withCondition(obj("batch/v1", "Job", "succeeding", nil), "SuccessCriteriaMet"),
		withCondition(obj("batch/v1", "Job", "failing", nil), "FailureTarget"),
		obj("batch/v1", "Job", "running", nil),
		obj("argoproj.io/v1alpha1", "Workflow", "migrate", hook),
		obj("v1", "ConfigMap", "hook-config", hook),
	}
	out, _, err := Filter{}.Process(context.Background(), objs)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]*resource.Object{}
	for _, o := range out {
		kept[o.GetName()] = o
	}
	if len(kept) != 2 || kept["running"] == nil || kept["hook-config"] == nil {
		t.Fatalf("kept %v, want running and hook-config", kept)
	}
	if !codes(kept["running"])["RUN_ONCE_EXPORTED"] {
		t.Error("running Job not flagged RUN_ONCE_EXPORTED")
	}
	if !codes(kept["hook-config"])["HOOK_RESOURCE"] {
		t.Error("hook ConfigMap not flagged HOOK_RESOURCE")
	}
}

func TestRunOncePods(t *testing.T) {
	for policy, want := range map[string]bool{"Never": true, "OnFailure": true, "Always": false, "": false} {
		o := obj("v1", "Pod", "p", nil)
		if policy != "" {
			_ = unstructured.SetNestedField(o.Object, policy, "spec", "restartPolicy")
		}
		_, _, _ = Filter{}.Process(context.Background(), []*resource.Object{o})
		if got := codes(o)["RUN_ONCE_EXPORTED"]; got != want {
			t.Errorf("restartPolicy %q: RUN_ONCE_EXPORTED = %v, want %v", policy, got, want)
		}
	}
}
