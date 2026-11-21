// tests the lifecycle filter, what it drops, what it keeps and the findings it attaches

package lifecycle

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// obj builds an Object of the given type and name in namespace shop, with the given annotations
func obj(apiVersion, kind, name string, ann map[string]string) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetAnnotations(ann)
	return resource.New(u)
}

// withCondition gives the object one true status condition of the given type
func withCondition(o *resource.Object, typ string) *resource.Object {
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": typ, "status": "True"}}, "status", "conditions")
	return o
}

// codes gives back the set of finding codes attached to the object
func codes(o *resource.Object) map[string]bool {
	out := map[string]bool{}
	for _, f := range o.Findings {
		out[f.Code] = true
	}
	return out
}

// check that finished Jobs are kept and marked ran once with their outcome, hook Workflows are dropped, and running Jobs and hook ConfigMaps get their warnings
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
	// finished Jobs stay for the snapshot, marked as ran once with their outcome, the hook Workflow goes
	if len(kept) != 5 || kept["running"] == nil || kept["hook-config"] == nil || kept["migrate"] != nil {
		t.Fatalf("kept %v, want the Jobs and hook-config", kept)
	}
	for name, result := range map[string]string{"done": "Complete", "succeeding": "Complete", "failing": "Failed"} {
		o := kept[name]
		if o == nil || !codes(o)[RanOnce] || codes(o)["RUN_ONCE_EXPORTED"] {
			t.Errorf("%s: findings %v, want RAN_ONCE only", name, codes(o))
			continue
		}
		if msg := o.Findings[0].Message; !strings.HasPrefix(msg, "finished Job ("+result+")") {
			t.Errorf("%s: %q, want its result %s", name, msg, result)
		}
	}
	if codes(kept["running"])[RanOnce] {
		t.Error("a running Job marked as ran once")
	}
	if !codes(kept["running"])["RUN_ONCE_EXPORTED"] {
		t.Error("running Job not flagged RUN_ONCE_EXPORTED")
	}
	if !codes(kept["hook-config"])["HOOK_RESOURCE"] {
		t.Error("hook ConfigMap not flagged HOOK_RESOURCE")
	}
}

// check that only bare Pods with restartPolicy Never or OnFailure are flagged as running once
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

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
