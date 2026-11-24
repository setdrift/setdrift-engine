// tests the normalize stage, that server-set fields, defaults, controller fields and tool markers go while what a user set stays

package normalize

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// parse turns a json object into a resource.Object for a test, failing the test when the json is bad
func parse(t *testing.T, s string) *resource.Object {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(s), &u.Object); err != nil {
		t.Fatal(err)
	}
	return resource.New(u)
}

// run normalizes one object with the default Normalizer and gives back its fields
func run(t *testing.T, o *resource.Object) map[string]any {
	t.Helper()
	if _, _, err := (Normalizer{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	return o.Object
}

// jsonOf gives back v as json, to compare with the expected output as one string
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// check that fields only a controller owns are removed, while ones a user owns or shares stay
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
	// data.k stays, kubectl owns all of data, a prefix of data.k
	if jsonOf(got) != want {
		t.Errorf("got  %s\nwant %s", jsonOf(got), want)
	}
}

// check that an unreadable managedFields entry stops all pruning
func TestPruneUnreadableManagedFields(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap",
	  "metadata": {"name": "x", "annotations": {"a": "1"}, "managedFields": [
	    {"manager": "kube-controller-manager", "fieldsType": "FieldsV1", "fieldsV1": {"f:metadata": {"f:annotations": {"f:a": {}}}}},
	    {"manager": "kubectl", "fieldsType": "FieldsV1", "fieldsV1": {"not a path": {}}}]}}`)
	if got := run(t, o); jsonOf(got["metadata"]) != `{"annotations":{"a":"1"},"name":"x"}` {
		t.Errorf("an unreadable entry must stop pruning: %s", jsonOf(got))
	}
}

// check that the default pull policy is Always for no tag or latest, and IfNotPresent for a tag or digest, with registry ports not taken for tags
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

// check that values that differ from the default are kept
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

// check that Argo CD, Helm and Flux markers are removed from an object's own metadata but not from its selector or pod template
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
	// the pod template must still match the selector
	if got := jsonOf(o.Object["spec"]); got != `{"selector":{"matchLabels":{"app.kubernetes.io/managed-by":"Helm"}},"template":{"metadata":{"labels":{"app.kubernetes.io/managed-by":"Helm"}}}}` {
		t.Errorf("spec changed: %s", got)
	}
}

// check that a managed-by label naming a tool other than Helm is kept
func TestManagedByOtherToolKept(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "labels": {"app.kubernetes.io/managed-by": "team-scripts"}}}`)
	if got := jsonOf(run(t, o)["metadata"]); got != `{"labels":{"app.kubernetes.io/managed-by":"team-scripts"},"name":"x"}` {
		t.Errorf("a non-Helm managed-by is the user's: %s", got)
	}
}

// check that a storage class stays when nothing proves the default class filled it in, no managedFields, an unreadable entry, or one owning it
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

// check that completions 1 is removed only when parallelism is unset or 1
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

// check that a list item only a controller wrote is removed entirely, not field by field
func TestControllerListItemRemovedWhole(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "managedFields": [
	  {"manager": "kubectl", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:env": {"k:{\"name\":\"A\"}": {".": {}, "f:name": {}, "f:value": {}}}}}},
	  {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:env": {
	    "k:{\"name\":\"B\"}": {".": {}, "f:name": {}, "f:value": {}},
	    "k:{\"name\":\"A\"}": {"f:valueFrom": {}}}}}}]},
	  "spec": {"env": [{"name": "A", "value": "1", "valueFrom": "x"}, {"name": "B", "value": "2"}]}}`)
	// B goes entirely, in A which kubectl owns only the controller's valueFrom goes
	if got := jsonOf(run(t, o)["spec"]); got != `{"env":[{"name":"A","value":"1"}]}` {
		t.Errorf("got %s", got)
	}
}

// check that a key field a controller owns alone is never removed from an item a user keeps
func TestKeyFieldKept(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "managedFields": [
	  {"manager": "kubectl", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:ports": {"k:{\"port\":80,\"protocol\":\"TCP\"}": {"f:name": {}}}}}},
	  {"manager": "kube-controller-manager", "operation": "Update", "fieldsType": "FieldsV1", "fieldsV1": {"f:spec": {"f:ports": {"k:{\"port\":80,\"protocol\":\"TCP\"}": {"f:port": {}, "f:protocol": {}}}}}}]},
	  "spec": {"ports": [{"name": "http", "port": 80, "protocol": "TCP"}]}}`)
	if got := jsonOf(run(t, o)["spec"]); got != `{"ports":[{"name":"http","port":80,"protocol":"TCP"}]}` {
		t.Errorf("got %s", got)
	}
}

// check that an object only cluster components wrote is kept as is, with a finding
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

// check that a default storage class is dropped normally and kept with a finding when KeepSourceDefaults is set
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

// check that what would break a restore elsewhere is removed, node bindings, injected CA bundles, autoscaled replicas and API server defaults, and what a user set stays
func TestRestoreFindings(t *testing.T) {
	// a Pod bound to a node, nodeName and the topology labels have no manager
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
	// a nodeName someone set is kept
	pinned := parse(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"shop",
	  "managedFields":[{"manager":"kubectl","operation":"Apply","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:spec":{"f:nodeName":{}}}}]},
	  "spec":{"nodeName":"i-0123","containers":[{"name":"c","image":"busybox"}]}}`)
	run(t, pinned)
	if n, _, _ := unstructured.NestedString(pinned.Object, "spec", "nodeName"); n != "i-0123" {
		t.Error("nodeName someone set was dropped")
	}

	// a webhook whose caBundle another component injected
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

	// the replicas of a workload an HPA scales
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

	// defaults the API server adds
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

// check that a CI/CD pipeline's source annotations and the Helm post-renderer's chart repo are removed, theyre stamped at every deploy and provenance and the inventory have read them
func TestPipelineAnnotationsRemoved(t *testing.T) {
	o := parse(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x", "annotations": {
	  "setdrift.io/source-repo": "https://github.com/acme/shop", "setdrift.io/source-path": "deploy", "setdrift.io/source-branch": "main", "setdrift.io/source-tag": "v1.4.2",
	  "setdrift.io/source-revision": "3f2c1a9", "setdrift.io/deployed-by": "https://github.com/acme/shop/actions/runs/1",
	  "setdrift.io/chart-repo": "https://charts.example.com", "team": "payments"}}}`)
	if got := jsonOf(run(t, o)["metadata"]); got != `{"annotations":{"team":"payments"},"name":"x"}` {
		t.Errorf("metadata: %s", got)
	}
}

// check that a single-stack Service keeps an explicit IPv6 family, while the usual IPv4 default is dropped with its policy
func TestServiceIPFamilies(t *testing.T) {
	for fam, keep := range map[string]bool{"IPv4": false, "IPv6": true} {
		o := parse(t, `{"apiVersion": "v1", "kind": "Service", "metadata": {"name": "x"},
		  "spec": {"ipFamilyPolicy": "SingleStack", "ipFamilies": ["`+fam+`"], "ports": [{"port": 80}]}}`)
		spec := run(t, o)["spec"].(map[string]any)
		if _, kept := spec["ipFamilies"]; kept != keep {
			t.Errorf("%s: ipFamilies kept %v, want %v: %v", fam, kept, keep, spec)
		}
	}
}

// check that the field managers are kept for the snapshot, oldest first, with their names, operations, subresources and times only, and managedFields leaves the manifest
func TestFieldManagersKept(t *testing.T) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetNamespace("shop")
	u.SetName("web")
	late, early := metav1.NewTime(time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)), metav1.NewTime(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, Time: &late, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{}}`)}},
		{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply, Time: &early},
	})
	o := resource.New(u)
	if _, _, err := (Normalizer{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	if len(o.GetManagedFields()) != 0 {
		t.Errorf("managedFields left in the manifest")
	}
	var got []string
	for _, m := range o.FieldManagers {
		got = append(got, m.Manager+" "+m.Operation+" "+m.GetTime().AsTime().Format(time.RFC3339))
	}
	if want := []string{"helm Apply 2026-11-20T09:00:00Z", "kubectl-edit Update 2026-11-24T06:00:00Z"}; !slices.Equal(got, want) {
		t.Errorf("field managers %v, want %v", got, want)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
