// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package normalize

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

func parse(t *testing.T, s string) *resource.Object {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(s), &u.Object); err != nil {
		t.Fatal(err)
	}
	return resource.New(u)
}

func run(t *testing.T, o *resource.Object) map[string]any {
	t.Helper()
	if _, _, err := (Normalizer{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	return o.Object
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPruneControllerFields(t *testing.T) {
	o := parse(t, `{
	  "apiVersion": "v1", "kind": "ConfigMap",
	  "metadata": {
	    "name": "x", "namespace": "shop",
	    "annotations": {"team": "a", "controller/stamp": "1", "shared": "y"},
	    "finalizers": ["example.com/keep", "kubernetes.io/pvc-protection"],
	    "managedFields": [
	      {"manager": "kubectl", "operation": "Update", "fieldsType": "FieldsV1",
	       "fieldsV1": {"f:metadata": {"f:annotations": {"f:team": {}, "f:shared": {}}, "f:finalizers": {"v:\"example.com/keep\"": {}}}, "f:data": {}}},
	      {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1",
	       "fieldsV1": {"f:metadata": {"f:annotations": {"f:controller/stamp": {}, "f:shared": {}}, "f:finalizers": {"v:\"kubernetes.io/pvc-protection\"": {}}}, "f:data": {"f:k": {}}}}
	    ]
	  },
	  "data": {"k": "v"}
	}`)
	got := run(t, o)
	want := `{"apiVersion":"v1","data":{"k":"v"},"kind":"ConfigMap","metadata":{"annotations":{"shared":"y","team":"a"},"finalizers":["example.com/keep"],"name":"x","namespace":"shop"}}`
	// data.k stays: kubectl owns data as a whole (a prefix of data.k).
	if jsonOf(got) != want {
		t.Errorf("got  %s\nwant %s", jsonOf(got), want)
	}
}

func TestPruneUnreadableManagedFields(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap",
	  "metadata": {"name": "x", "annotations": {"a": "1"}, "managedFields": [
	    {"manager": "kube-controller-manager", "fieldsType": "FieldsV1", "fieldsV1": {"f:metadata": {"f:annotations": {"f:a": {}}}}},
	    {"manager": "kubectl", "fieldsType": "FieldsV1", "fieldsV1": {"not a path": {}}}]}}`)
	if got := run(t, o); jsonOf(got["metadata"]) != `{"annotations":{"a":"1"},"name":"x"}` {
		t.Errorf("an unreadable entry must stop pruning: %s", jsonOf(got))
	}
}

func TestDefaultPullPolicy(t *testing.T) {
	for image, want := range map[string]string{
		"nginx":                            "Always",
		"nginx:latest":                     "Always",
		"nginx:1.27":                       "IfNotPresent",
		"localhost:5000/app":               "Always",
		"localhost:5000/app:2":             "IfNotPresent",
		"ghcr.io/x/app@sha256:abc":         "IfNotPresent",
		"111122223333.dkr.ecr.aws/app:1.0": "IfNotPresent",
	} {
		if got := defaultPullPolicy(image); got != want {
			t.Errorf("%s: got %s, want %s", image, got, want)
		}
	}
}

// Values that differ from the default are kept.
func TestNonDefaultsKept(t *testing.T) {
	o := parse(t, `{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "x"},
	  "spec": {"revisionHistoryLimit": 3, "progressDeadlineSeconds": 600,
	    "strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 1, "maxUnavailable": 0}},
	    "template": {"spec": {"dnsPolicy": "None", "terminationGracePeriodSeconds": 60,
	      "containers": [{"name": "a", "image": "app:1", "imagePullPolicy": "Always",
	        "readinessProbe": {"periodSeconds": 5, "timeoutSeconds": 1}}]}}}}`)
	got := jsonOf(run(t, o)["spec"])
	want := `{"revisionHistoryLimit":3,"strategy":{"rollingUpdate":{"maxSurge":1,"maxUnavailable":0},"type":"RollingUpdate"},"template":{"spec":{"containers":[{"image":"app:1","imagePullPolicy":"Always","name":"a","readinessProbe":{"periodSeconds":5}}],"dnsPolicy":"None","terminationGracePeriodSeconds":60}}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestToolMarkersRemoved(t *testing.T) {
	o := parse(t, `{"apiVersion": "apps/v1", "kind": "Deployment",
	  "metadata": {"name": "web", "namespace": "shop",
	    "annotations": {"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/web", "meta.helm.sh/release-name": "web",
	      "meta.helm.sh/release-namespace": "shop", "team": "payments"},
	    "labels": {"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "web", "argocd.argoproj.io/instance": "web",
	      "kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system", "app": "web"}},
	  "spec": {"selector": {"matchLabels": {"app.kubernetes.io/managed-by": "Helm"}},
	    "template": {"metadata": {"labels": {"app.kubernetes.io/managed-by": "Helm"}}}}}`)
	if _, _, err := (Normalizer{ArgoInstanceLabel: "argocd.argoproj.io/instance"}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	if got, want := jsonOf(o.Object["metadata"]), `{"annotations":{"team":"payments"},"labels":{"app":"web","app.kubernetes.io/instance":"web"},"name":"web","namespace":"shop"}`; got != want {
		t.Errorf("metadata\n got %s\nwant %s", got, want)
	}
	// The Pod template must still match the selector.
	if got := jsonOf(o.Object["spec"]); got != `{"selector":{"matchLabels":{"app.kubernetes.io/managed-by":"Helm"}},"template":{"metadata":{"labels":{"app.kubernetes.io/managed-by":"Helm"}}}}` {
		t.Errorf("spec changed: %s", got)
	}
}

func TestManagedByOtherToolKept(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "labels": {"app.kubernetes.io/managed-by": "team-scripts"}}}`)
	if got := jsonOf(run(t, o)["metadata"]); got != `{"labels":{"app.kubernetes.io/managed-by":"team-scripts"},"name":"x"}` {
		t.Errorf("a non-Helm managed-by is the user's: %s", got)
	}
}

// Without managedFields nothing proves the class was filled in, so it stays.
func TestStorageClassKeptWithoutEvidence(t *testing.T) {
	for name, mf := range map[string]string{
		"no managedFields": ``,
		"unreadable entry": `, "managedFields": [{"manager": "x", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"not a path": {}}}]`,
		"owned by creator": `, "managedFields": [{"manager": "helm", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:storageClassName": {}}}}]`,
	} {
		o := parse(t, `{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "x"`+mf+`}, "spec": {"storageClassName": "gp3"}}`)
		if got := jsonOf(run(t, o)["spec"]); got != `{"storageClassName":"gp3"}` {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestJobCompletionsDefault(t *testing.T) {
	for in, want := range map[string]string{
		`{"completions": 1, "parallelism": 1}`: `{}`,
		`{"completions": 1}`:                   `{}`,
		`{"completions": 1, "parallelism": 5}`: `{"completions":1,"parallelism":5}`,
		`{"completions": 3, "parallelism": 1}`: `{"completions":3}`,
	} {
		o := parse(t, `{"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": "x"}, "spec": `+in+`}`)
		spec, _ := run(t, o)["spec"]
		if spec == nil {
			spec = map[string]any{}
		}
		if got := jsonOf(spec); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

// A list item only a controller wrote is removed whole, not field by field.
func TestControllerListItemRemovedWhole(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "managedFields": [
	  {"manager": "kubectl", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:env": {"k:{\"name\":\"A\"}": {".": {}, "f:name": {}, "f:value": {}}}}}},
	  {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:env": {
	    "k:{\"name\":\"B\"}": {".": {}, "f:name": {}, "f:value": {}},
	    "k:{\"name\":\"A\"}": {"f:valueFrom": {}}}}}}]},
	  "spec": {"env": [{"name": "A", "value": "1", "valueFrom": "x"}, {"name": "B", "value": "2"}]}}`)
	// B goes whole; in A, which kubectl owns, only the controller's valueFrom goes.
	if got := jsonOf(run(t, o)["spec"]); got != `{"env":[{"name":"A","value":"1"}]}` {
		t.Errorf("got %s", got)
	}
}

// A key field a controller owns alone is never removed from an item a user keeps.
func TestKeyFieldKept(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "managedFields": [
	  {"manager": "kubectl", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:ports": {"k:{\"port\":80,\"protocol\":\"TCP\"}": {"f:name": {}}}}}},
	  {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:ports": {"k:{\"port\":80,\"protocol\":\"TCP\"}": {"f:port": {}, "f:protocol": {}}}}}}]},
	  "spec": {"ports": [{"name": "http", "port": 80, "protocol": "TCP"}]}}`)
	if got := jsonOf(run(t, o)["spec"]); got != `{"ports":[{"name":"http","port":80,"protocol":"TCP"}]}` {
		t.Errorf("got %s", got)
	}
}

// An object only cluster components wrote is kept whole, with a finding.
func TestControllerCreatedKeptWhole(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "c", "namespace": "shop", "managedFields": [
	  {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1",
	   "fieldsV1": {"f:spec": {"f:accessModes": {}, "f:resources": {"f:requests": {".": {}, "f:storage": {}}}}}}]},
	  "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}}`)
	if got := jsonOf(run(t, o)["spec"]); got != `{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}` {
		t.Errorf("got %s", got)
	}
	if len(o.Findings) != 1 || o.Findings[0].Code != "CONTROLLER_CREATED" {
		t.Errorf("findings: %v", o.Findings)
	}
}

func TestKeepSourceDefaults(t *testing.T) {
	claim := `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"shop",
	  "managedFields":[{"manager":"kubectl-client-side-apply","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1",
	    "fieldsV1":{"f:spec":{"f:accessModes":{},"f:resources":{"f:requests":{".":{},"f:storage":{}}}}}}]},
	  "spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}},"storageClassName":"gp3"}}`
	dropped := parse(t, claim)
	run(t, dropped)
	if _, ok, _ := unstructured.NestedString(dropped.Object, "spec", "storageClassName"); ok {
		t.Error("default class kept without the option")
	}
	kept := parse(t, claim)
	if _, _, err := (Normalizer{KeepSourceDefaults: true}).Process(context.Background(), []*resource.Object{kept}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := unstructured.NestedString(kept.Object, "spec", "storageClassName"); c != "gp3" {
		t.Errorf("class %q, want gp3", c)
	}
	if len(kept.Findings) != 1 || kept.Findings[0].Code != "STORAGE_CLASS_KEPT" {
		t.Errorf("findings %v", kept.Findings)
	}
}

func TestRestoreFindings(t *testing.T) {
	// A Pod bound to a node: nodeName and topology labels have no manager.
	pod := parse(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"shop",
	  "labels":{"app":"p","topology.kubernetes.io/zone":"us-east-2a","topology.kubernetes.io/region":"us-east-2"},
	  "managedFields":[{"manager":"kubectl","operation":"Apply","apiVersion":"v1","fieldsType":"FieldsV1",
	    "fieldsV1":{"f:metadata":{"f:labels":{"f:app":{}}},"f:spec":{"f:containers":{}}}}]},
	  "spec":{"nodeName":"i-0123","containers":[{"name":"c","image":"busybox"}]}}`)
	run(t, pod)
	if _, ok, _ := unstructured.NestedString(pod.Object, "spec", "nodeName"); ok {
		t.Error("nodeName kept")
	}
	if l := pod.GetLabels(); len(l) != 1 || l["app"] != "p" {
		t.Errorf("labels %v", l)
	}
	// nodeName someone set: kept.
	pinned := parse(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"shop",
	  "managedFields":[{"manager":"kubectl","operation":"Apply","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:spec":{"f:nodeName":{}}}}]},
	  "spec":{"nodeName":"i-0123","containers":[{"name":"c","image":"busybox"}]}}`)
	run(t, pinned)
	if n, _, _ := unstructured.NestedString(pinned.Object, "spec", "nodeName"); n != "i-0123" {
		t.Error("nodeName someone set was dropped")
	}

	// A webhook whose caBundle another component injected.
	webhook := parse(t, `{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingWebhookConfiguration","metadata":{"name":"v",
	  "managedFields":[
	    {"manager":"helm","operation":"Update","apiVersion":"admissionregistration.k8s.io/v1","fieldsType":"FieldsV1","fieldsV1":{"f:webhooks":{"k:{\"name\":\"a\"}":{"f:clientConfig":{"f:service":{}}}}}},
	    {"manager":"external-secrets","operation":"Update","apiVersion":"admissionregistration.k8s.io/v1","fieldsType":"FieldsV1","fieldsV1":{"f:webhooks":{"k:{\"name\":\"a\"}":{"f:clientConfig":{"f:caBundle":{}}}}}}]},
	  "webhooks":[{"name":"a","clientConfig":{"caBundle":"Q0E=","service":{"name":"s","namespace":"n"}}}]}`)
	run(t, webhook)
	if ws := webhook.Object["webhooks"].([]any); ws[0].(map[string]any)["clientConfig"].(map[string]any)["caBundle"] != nil {
		t.Error("injected caBundle kept")
	}
	own := parse(t, `{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingWebhookConfiguration","metadata":{"name":"v",
	  "managedFields":[{"manager":"kubectl","operation":"Apply","apiVersion":"admissionregistration.k8s.io/v1","fieldsType":"FieldsV1","fieldsV1":{"f:webhooks":{"k:{\"name\":\"a\"}":{"f:clientConfig":{"f:caBundle":{},"f:service":{}}}}}}]},
	  "webhooks":[{"name":"a","clientConfig":{"caBundle":"Q0E=","service":{"name":"s","namespace":"n"}}}]}`)
	run(t, own)
	if ws := own.Object["webhooks"].([]any); ws[0].(map[string]any)["clientConfig"].(map[string]any)["caBundle"] == nil {
		t.Error("caBundle set with the webhook dropped")
	}

	// replicas of a workload an HPA scales.
	deploy := parse(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"shop"},"spec":{"replicas":4}}`)
	other := parse(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":4}}`)
	hpa := parse(t, `{"apiVersion":"autoscaling/v2","kind":"HorizontalPodAutoscaler","metadata":{"name":"api","namespace":"shop"},
	  "spec":{"scaleTargetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"api"},"maxReplicas":5}}`)
	if _, _, err := (Normalizer{}).Process(context.Background(), []*resource.Object{deploy, other, hpa}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(deploy.Object, "spec", "replicas"); ok || len(deploy.Findings) != 1 {
		t.Errorf("HPA target replicas kept: %v", deploy.Object["spec"])
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(other.Object, "spec", "replicas"); !ok {
		t.Error("replicas of a workload without an HPA dropped")
	}

	// Defaults the API server adds.
	pvc := parse(t, `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"d","namespace":"shop","finalizers":["kubernetes.io/pvc-protection","example.com/keep"]},"spec":{}}`)
	sts := resource.New(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": "db", "namespace": "shop"},
		"spec": map[string]any{"updateStrategy": map[string]any{"type": "RollingUpdate",
			"rollingUpdate": map[string]any{"partition": int64(0), "maxUnavailable": int64(1)}}}}})
	pc := parse(t, `{"apiVersion":"scheduling.k8s.io/v1","kind":"PriorityClass","metadata":{"name":"high"},"value":10,"preemptionPolicy":"PreemptLowerPriority"}`)
	cj := parse(t, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"r","namespace":"shop"},"spec":{"schedule":"* * * * *","jobTemplate":{"metadata":{},"spec":{}}}}`)
	for _, o := range []*resource.Object{pvc, sts, pc, cj} {
		run(t, o)
	}
	if f := pvc.GetFinalizers(); len(f) != 1 || f[0] != "example.com/keep" {
		t.Errorf("pvc finalizers %v", f)
	}
	if _, ok := sts.Object["spec"].(map[string]any)["updateStrategy"]; ok {
		t.Error("StatefulSet default updateStrategy kept")
	}
	if _, ok := pc.Object["preemptionPolicy"]; ok {
		t.Error("default preemptionPolicy kept")
	}
	if _, ok, _ := unstructured.NestedMap(cj.Object, "spec", "jobTemplate", "metadata"); ok {
		t.Error("empty jobTemplate.metadata kept")
	}
}

// A CI/CD pipeline's source annotations and the Helm post-renderer's chart
// repository are stamped at every deploy: provenance and the inventory have
// read them, and the manifest drops them.
func TestPipelineAnnotationsRemoved(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "annotations": {
	  "setdrift.io/source-repo": "https://github.com/acme/shop", "setdrift.io/source-path": "deploy", "setdrift.io/source-branch": "main", "setdrift.io/source-tag": "v1.4.2",
	  "setdrift.io/source-revision": "3f2c1a9", "setdrift.io/deployed-by": "https://github.com/acme/shop/actions/runs/1",
	  "setdrift.io/chart-repo": "https://charts.example.com", "team": "payments"}}}`)
	if got := jsonOf(run(t, o)["metadata"]); got != `{"annotations":{"team":"payments"},"name":"x"}` {
		t.Errorf("metadata: %s", got)
	}
}
