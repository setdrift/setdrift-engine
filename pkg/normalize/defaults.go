// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// The defaults the API server and admission set (pkg/apis/*/defaults.go in
// Kubernetes' source), removed where they equal the default.

package normalize

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

var (
	podGK         = schema.GroupKind{Kind: "Pod"}
	pvGK          = schema.GroupKind{Kind: "PersistentVolume"}
	priorityGK    = schema.GroupKind{Group: "scheduling.k8s.io", Kind: "PriorityClass"}
	mutatingGK    = schema.GroupKind{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}
	validatingGK  = schema.GroupKind{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}
	apiServiceGK  = schema.GroupKind{Group: "apiregistration.k8s.io", Kind: "APIService"}
	crdGK         = schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}
	serviceGK     = schema.GroupKind{Kind: "Service"}
	namespaceGK   = schema.GroupKind{Kind: "Namespace"}
	pvcGK         = schema.GroupKind{Kind: "PersistentVolumeClaim"}
	deploymentGK  = schema.GroupKind{Group: "apps", Kind: "Deployment"}
	replicaSetGK  = schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}
	statefulSetGK = schema.GroupKind{Group: "apps", Kind: "StatefulSet"}
	daemonSetGK   = schema.GroupKind{Group: "apps", Kind: "DaemonSet"}
	jobGK         = schema.GroupKind{Group: "batch", Kind: "Job"}
	cronJobGK     = schema.GroupKind{Group: "batch", Kind: "CronJob"}
	netpolGK      = schema.GroupKind{Group: "networking.k8s.io", Kind: "NetworkPolicy"}
	storageGK     = schema.GroupKind{Group: "storage.k8s.io", Kind: "StorageClass"}
)

// podSpecPath is where each workload kind keeps its pod spec
var podSpecPath = map[schema.GroupKind][]string{
	podGK:         {"spec"},
	deploymentGK:  {"spec", "template", "spec"},
	replicaSetGK:  {"spec", "template", "spec"},
	statefulSetGK: {"spec", "template", "spec"},
	daemonSetGK:   {"spec", "template", "spec"},
	jobGK:         {"spec", "template", "spec"},
	cronJobGK:     {"spec", "jobTemplate", "spec", "template", "spec"},
}

// dropIf removes the field at path if it equals def
func dropIf(m map[string]any, def any, path ...string) {
	v, found, err := unstructured.NestedFieldNoCopy(m, path...)
	if !found || err != nil || !equal(v, def) {
		return
	}
	unstructured.RemoveNestedField(m, path...)
}

// equal compares unstructured values, treating every integer type the same
func equal(a, b any) bool {
	if ai, ok := toInt(a); ok {
		bi, ok := toInt(b)
		return ok && ai == bi
	}
	return reflect.DeepEqual(a, b)
}

// determine if value is an integer
func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

// remove empty fields
func dropEmpty(m map[string]any, path ...string) {
	v, found, _ := unstructured.NestedFieldNoCopy(m, path...)
	if found && empty(v) {
		unstructured.RemoveNestedField(m, path...)
	}
}

// extracts nested fields
func items(m map[string]any, path ...string) []map[string]any {
	list, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	s, _ := list.([]any)
	out := make([]map[string]any, 0, len(s))
	for _, item := range s {
		if im, ok := item.(map[string]any); ok {
			out = append(out, im)
		}
	}
	return out
}

func podSpecDefaults(o *resource.Object) {
	gk := o.GroupVersionKind().GroupKind()
	path, ok := podSpecPath[gk]
	if !ok {
		return
	}
	if len(path) > 1 {
		// handle timestamp values
		meta := append(slices.Clone(path[:len(path)-1]), "metadata")
		dropIf(o.Object, nil, append(meta, "creationTimestamp")...)
		// kubectl rollout restart's timestamp
		unstructured.RemoveNestedField(o.Object, append(meta, "annotations", restartedAtAnnotation)...)
		dropEmpty(o.Object, append(meta, "annotations")...)
		dropEmpty(o.Object, meta...)
	}
	v, found, _ := unstructured.NestedFieldNoCopy(o.Object, path...)
	spec, ok := v.(map[string]any)
	if !found || !ok {
		return
	}
	// hard coded defaults injected by cluster
	dropIf(spec, "ClusterFirst", "dnsPolicy")
	dropIf(spec, "default-scheduler", "schedulerName")
	dropIf(spec, int64(30), "terminationGracePeriodSeconds")
	dropIf(spec, "PreemptLowerPriority", "preemptionPolicy")
	dropIf(spec, true, "enableServiceLinks")
	dropEmpty(spec, "securityContext")
	// Jobs must set restartPolicy (Never or OnFailure), defaults to Always
	if gk != jobGK && gk != cronJobGK {
		dropIf(spec, "Always", "restartPolicy")
	}
	// serviceAccount is the deprecated copy of serviceAccountName
	if sa, ok := spec["serviceAccount"]; ok && equal(sa, spec["serviceAccountName"]) {
		delete(spec, "serviceAccount")
	}
	// set by the priority admission plugin from priorityClassName
	if _, ok := spec["priorityClassName"]; ok {
		delete(spec, "priority")
	} else {
		dropIf(spec, int64(0), "priority")
	}
	for _, list := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, c := range items(spec, list) {
			containerDefaults(c)
		}
	}
	for _, vol := range items(spec, "volumes") {
		for _, src := range []string{"configMap", "secret"} {
			dropIf(vol, int64(0o644), src, "defaultMode")
		}
		dropIf(vol, int64(0o644), "projected", "defaultMode")
		dropIf(vol, "", "hostPath", "type")
		dropIf(vol, int64(0o644), "downwardAPI", "defaultMode")
	}
	if gk == podGK {
		podOnlyDefaults(spec)
	}
}

// replicasDefault removes replicas: 1, the default for Deployments, ReplicaSets and StatefulSets
func replicasDefault(o *resource.Object) {
	dropIf(o.Object, int64(1), "spec", "replicas")
}

// container level defaults
func containerDefaults(c map[string]any) {
	dropIf(c, "/dev/termination-log", "terminationMessagePath")
	dropIf(c, "File", "terminationMessagePolicy")
	if image, ok := c["image"].(string); ok {
		dropIf(c, defaultPullPolicy(image), "imagePullPolicy")
	}
	dropEmpty(c, "resources")
	for _, p := range items(c, "ports") {
		dropIf(p, "TCP", "protocol")
	}
	for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
		pm, ok := c[probe].(map[string]any)
		if !ok {
			continue
		}
		dropIf(pm, int64(1), "timeoutSeconds")
		dropIf(pm, int64(10), "periodSeconds")
		dropIf(pm, int64(1), "successThreshold")
		dropIf(pm, int64(3), "failureThreshold")
		dropIf(pm, "HTTP", "httpGet", "scheme")
	}
}

// defaultPullPolicy is Always for an image with no tag or the latest tag, IfNotPresent otherwise
func defaultPullPolicy(image string) string {
	if strings.Contains(image, "@") {
		return "IfNotPresent"
	}
	name := image[strings.LastIndex(image, "/")+1:]
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
	}
	if tag == "" || tag == "latest" {
		return "Always"
	}
	return "IfNotPresent"
}

// podOnlyDefaults handles what admission adds to pods themselves
func podOnlyDefaults(spec map[string]any) {
	// the ServiceAccount admission plugin sets the default service account on pods that name none
	dropIf(spec, "default", "serviceAccountName")
	var token string
	if vols, ok := spec["volumes"].([]any); ok {
		var kept []any
		for _, v := range vols {
			vm, _ := v.(map[string]any)
			name, _ := vm["name"].(string)
			//kube-api-access-* default mounted by every container
			if strings.HasPrefix(name, "kube-api-access-") {
				token = name
				continue
			}
			kept = append(kept, v)
		}
		setOrDrop(spec, "volumes", kept)
	}
	if token != "" {
		for _, list := range []string{"containers", "initContainers"} {
			for _, c := range items(spec, list) {
				mounts, ok := c["volumeMounts"].([]any)
				if !ok {
					continue
				}
				var kept []any
				for _, m := range mounts {
					if mm, _ := m.(map[string]any); mm["name"] != token {
						kept = append(kept, m)
					}
				}
				setOrDrop(c, "volumeMounts", kept)
			}
		}
	}
	if tols, ok := spec["tolerations"].([]any); ok {
		var kept []any
		for _, t := range tols {
			if !admissionToleration(t) {
				kept = append(kept, t)
			}
		}
		setOrDrop(spec, "tolerations", kept)
	}
}

func admissionToleration(t any) bool {
	m, ok := t.(map[string]any)
	if !ok || m["operator"] != "Exists" || m["effect"] != "NoExecute" || !equal(m["tolerationSeconds"], int64(300)) {
		return false
	}
	k := m["key"]
	return len(m) == 4 && (k == "node.kubernetes.io/not-ready" || k == "node.kubernetes.io/unreachable")
}

func setOrDrop(m map[string]any, key string, list []any) {
	if len(list) == 0 {
		delete(m, key)
	} else {
		m[key] = list
	}
}

// service defaults
func serviceDefaults(o *resource.Object) {
	spec, ok := o.Object["spec"].(map[string]any)
	if !ok {
		return
	}
	// Before the protocol default below is removed, it is part of each port's key in managedFields.
	nodePorts(o, spec)
	// Allocated from the cluster's Service CIDR, "None" (headless) is a choice
	if spec["clusterIP"] != "None" {
		delete(spec, "clusterIP")
		delete(spec, "clusterIPs")
	}
	dropIf(spec, "SingleStack", "ipFamilyPolicy")
	if spec["ipFamilyPolicy"] == nil {
		delete(spec, "ipFamilies")
	}
	dropIf(spec, "None", "sessionAffinity")
	dropIf(spec, "Cluster", "internalTrafficPolicy")
	dropIf(spec, "Cluster", "externalTrafficPolicy")
	dropIf(spec, true, "allocateLoadBalancerNodePorts")
	dropIf(spec, "ClusterIP", "type")
	for _, p := range items(spec, "ports") {
		dropIf(p, "TCP", "protocol")
		if equal(p["targetPort"], p["port"]) {
			delete(p, "targetPort")
		}
	}
}

// namespace defaults
func namespaceDefaults(o *resource.Object) {
	dropIf(o.Object, []any{"kubernetes"}, "spec", "finalizers")
	dropEmpty(o.Object, "spec")
	// Set by the API server on every Namespace
	if labels := o.GetLabels(); labels["kubernetes.io/metadata.name"] == o.GetName() {
		delete(labels, "kubernetes.io/metadata.name")
		setLabels(o, labels)
	}
}

// pvc defaults
func pvcDefaults(o *resource.Object) { pvcDefaultsKeeping(o, false) }

// pvcDefaultsKeeping is pvcDefaults; keepClass keeps a storage class the
// cluster's default filled in (Normalizer.KeepSourceDefaults).
func pvcDefaultsKeeping(o *resource.Object, keepClass bool) {
	dropIf(o.Object, "Filesystem", "spec", "volumeMode")
	// a claim that names no class gets the cluster's default class from the DefaultStorageClass admission plugin
	if keepClass && admissionSet(o, "spec", "storageClassName") {
		class, _, _ := unstructured.NestedString(o.Object, "spec", "storageClassName")
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "STORAGE_CLASS_KEPT",
			"storage class %s was filled in from the cluster's default class; kept (--keep-source-defaults), so a new cluster needs a class named %s", class, class)
	} else if admissionSet(o, "spec", "storageClassName") {
		class, _, _ := unstructured.NestedString(o.Object, "spec", "storageClassName")
		unstructured.RemoveNestedField(o.Object, "spec", "storageClassName")
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "STORAGE_CLASS_DROPPED",
			"storage class %s was filled in from the cluster's default class, not set in the manifest, so it was left out "+
				"and a new cluster's default class applies; set storageClassName in the manifest to keep %s", class, class)
	}
	dropFinalizer(o, "kubernetes.io/pvc-protection")
	ann := o.GetAnnotations()
	// Dynamically provisioned: the volume name was chosen by the binder
	if ann["volume.kubernetes.io/storage-provisioner"] != "" || ann["volume.beta.kubernetes.io/storage-provisioner"] != "" {
		unstructured.RemoveNestedField(o.Object, "spec", "volumeName")
	}
	for _, a := range []string{
		"pv.kubernetes.io/bind-completed",
		"pv.kubernetes.io/bound-by-controller",
		"volume.kubernetes.io/storage-provisioner",
		"volume.beta.kubernetes.io/storage-provisioner",
		"volume.kubernetes.io/selected-node",
	} {
		delete(ann, a)
	}
	o.SetAnnotations(ann)
}

// admissionSet reports whether the field at path is set but owned by no field manager.
// The API server records managedFields before mutating admission runs, so a value an
// admission plugin fills in belongs to no one, while one a client sent belongs to that
// client. Without managedFields (an export from files) nothing can be proven, so false.
func admissionSet(o *resource.Object, path ...string) bool {
	if _, found, _ := unstructured.NestedFieldNoCopy(o.Object, path...); !found {
		return false
	}
	entries := o.GetManagedFields()
	if len(entries) == 0 {
		return false
	}
	parts := make([]any, len(path))
	for i, p := range path {
		parts[i] = p
	}
	fp, err := fieldpath.MakePath(parts...)
	if err != nil {
		return false
	}
	for _, e := range entries {
		s, err := kube.FieldSet(e)
		if err != nil || s == nil || s.Has(fp) {
			return false // an unreadable entry might own the field
		}
	}
	return true
}

// podDefaults drops what binding a Pod to a node writes: spec.nodeName
// (the scheduler's binding) and the node's topology labels (the
// PodTopologyLabels admission plugin). Neither is recorded in managedFields.
// Exported, the Pod would be bound to a node a new cluster does not have,
// and the pod garbage collector deletes it at once.
func podDefaults(o *resource.Object) {
	if admissionSet(o, "spec", "nodeName") {
		unstructured.RemoveNestedField(o.Object, "spec", "nodeName")
	}
	for _, l := range []string{"topology.kubernetes.io/zone", "topology.kubernetes.io/region"} {
		if admissionSet(o, "metadata", "labels", l) {
			unstructured.RemoveNestedField(o.Object, "metadata", "labels", l)
		}
	}
}

// pvDefaults drops the protection finalizer the API server adds.
func pvDefaults(o *resource.Object) { dropFinalizer(o, "kubernetes.io/pv-protection") }

func priorityClassDefaults(o *resource.Object) {
	dropIf(o.Object, "PreemptLowerPriority", "preemptionPolicy")
}

// dropFinalizer removes a finalizer the API server adds to every object of
// a kind (StorageObjectInUseProtection admission).
func dropFinalizer(o *resource.Object, name string) {
	fin := o.GetFinalizers()
	var kept []string
	for _, f := range fin {
		if f != name {
			kept = append(kept, f)
		}
	}
	if len(kept) != len(fin) {
		o.SetFinalizers(kept)
	}
}

// injectedCABundles drops the CA bundles of webhooks, APIServices and CRD
// conversion webhooks when a component other than the one that wrote the
// object injected them (cert-manager's cainjector, an operator's own cert
// controller): the bundle is the source cluster's CA, the injector writes
// a new one in a new cluster, and an export carrying the old one conflicts
// with it on apply. The evidence is a field manager that owns a caBundle
// but no service or URL it belongs with.
func injectedCABundles(o *resource.Object) {
	injected := false
	for _, e := range o.GetManagedFields() {
		s, err := kube.FieldSet(e)
		if err != nil || s == nil {
			continue
		}
		if ownsField(s, "caBundle") && !ownsField(s, "service") && !ownsField(s, "url") {
			injected = true
		}
	}
	if !injected {
		return
	}
	for _, w := range items(o.Object, "webhooks") {
		unstructured.RemoveNestedField(w, "clientConfig", "caBundle")
	}
	unstructured.RemoveNestedField(o.Object, "spec", "caBundle")
	unstructured.RemoveNestedField(o.Object, "spec", "conversion", "webhook", "clientConfig", "caBundle")
}

// ownsField reports whether s holds a field named name, at any depth.
func ownsField(s *fieldpath.Set, name string) bool {
	found := false
	s.Iterate(func(p fieldpath.Path) {
		for _, pe := range p {
			if pe.FieldName != nil && *pe.FieldName == name {
				found = true
			}
		}
	})
	return found
}

// scaleTarget is the object a HorizontalPodAutoscaler scales.
type scaleTarget struct {
	gk        schema.GroupKind
	namespace string
	name      string
}

// hpaTargets returns each HorizontalPodAutoscaler's target.
func hpaTargets(objs []*resource.Object) map[scaleTarget]bool {
	out := map[scaleTarget]bool{}
	for _, o := range objs {
		if gvk := o.GroupVersionKind(); gvk.Group != "autoscaling" || gvk.Kind != "HorizontalPodAutoscaler" {
			continue
		}
		ref, _, _ := unstructured.NestedStringMap(o.Object, "spec", "scaleTargetRef")
		out[scaleTarget{schema.GroupKind{Group: kube.Group(ref["apiVersion"]), Kind: ref["kind"]}, o.GetNamespace(), ref["name"]}] = true
	}
	return out
}

// deployment defaults
func deploymentDefaults(o *resource.Object) {
	dropIf(o.Object, int64(600), "spec", "progressDeadlineSeconds")
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	dropIf(o.Object, map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"},
	}, "spec", "strategy")
	dropAnnotation(o, "deployment.kubernetes.io/revision")
}

// statefulset defaults
func statefulSetDefaults(o *resource.Object) {
	dropIf(o.Object, "OrderedReady", "spec", "podManagementPolicy")
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	for _, def := range []any{
		map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(0)}},
		// 1.37 also defaults maxUnavailable
		map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(0), "maxUnavailable": int64(1)}},
		map[string]any{"type": "RollingUpdate"},
	} {
		dropIf(o.Object, def, "spec", "updateStrategy")
	}
	dropIf(o.Object, map[string]any{"whenDeleted": "Retain", "whenScaled": "Retain"}, "spec", "persistentVolumeClaimRetentionPolicy")
	for _, t := range items(o.Object, "spec", "volumeClaimTemplates") {
		dropIf(t, "Filesystem", "spec", "volumeMode")
		delete(t, "status")
	}
}

// daemonset defaults
func daemonSetDefaults(o *resource.Object) {
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	dropIf(o.Object, map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"maxSurge": int64(0), "maxUnavailable": int64(1)},
	}, "spec", "updateStrategy")
	dropAnnotation(o, "deprecated.daemonset.template.generation")
}

// job spec defaults
func jobSpecDefaults(spec map[string]any) {
	dropIf(spec, int64(6), "backoffLimit")
	dropIf(spec, "NonIndexed", "completionMode")
	// completions defaults to 1 only when parallelism is unset too; with parallelism set and
	// completions unset the Job is a work queue, so completions: 1 beside it was chosen
	if p, ok := spec["parallelism"]; !ok || equal(p, int64(1)) {
		dropIf(spec, int64(1), "completions")
	}
	dropIf(spec, int64(1), "parallelism")
	dropIf(spec, false, "suspend")
	dropIf(spec, false, "manualSelector")
	if _, ok := spec["podFailurePolicy"]; !ok {
		dropIf(spec, "TerminatingOrFailed", "podReplacementPolicy")
	}
}

// job defaults, uses jobspecdefaults function
func jobDefaults(o *resource.Object) {
	if spec, ok := o.Object["spec"].(map[string]any); ok {
		jobSpecDefaults(spec)
	}
}

// cronjob defaults, uses jobspecdefaults function
func cronJobDefaults(o *resource.Object) {
	dropIf(o.Object, "Allow", "spec", "concurrencyPolicy")
	dropIf(o.Object, int64(1), "spec", "failedJobsHistoryLimit")
	dropIf(o.Object, int64(3), "spec", "successfulJobsHistoryLimit")
	dropIf(o.Object, false, "spec", "suspend")
	// the API server fills in an empty metadata on the Job template
	dropEmpty(o.Object, "spec", "jobTemplate", "metadata")
	if v, found, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "jobTemplate", "spec"); found {
		if spec, ok := v.(map[string]any); ok {
			jobSpecDefaults(spec)
		}
	}
}

// network policy defaults
func networkPolicyDefaults(o *resource.Object) {
	spec, ok := o.Object["spec"].(map[string]any)
	if !ok {
		return
	}
	// with no policyTypes the API server sets Ingress, and Egress when there are egress rules
	def := []any{"Ingress"}
	if egress, _ := spec["egress"].([]any); len(egress) > 0 {
		def = append(def, "Egress")
	}
	dropIf(spec, def, "policyTypes")
	for _, dir := range []string{"ingress", "egress"} {
		for _, r := range items(spec, dir) {
			for _, p := range items(r, "ports") {
				dropIf(p, "TCP", "protocol")
			}
		}
	}
}

// storage class defaults
func storageClassDefaults(o *resource.Object) {
	dropIf(o.Object, "Delete", "reclaimPolicy")
	dropIf(o.Object, "Immediate", "volumeBindingMode")
}

func dropAnnotation(o *resource.Object, key string) {
	ann := o.GetAnnotations()
	if _, ok := ann[key]; !ok {
		return
	}
	delete(ann, key)
	o.SetAnnotations(ann)
}

func setLabels(o *resource.Object, labels map[string]string) {
	if len(labels) == 0 {
		unstructured.RemoveNestedField(o.Object, "metadata", "labels")
		return
	}
	o.SetLabels(labels)
}

// removes cluster assigned nodePorts, keeps if user defined
func nodePorts(o *resource.Object, spec map[string]any) {
	applied := fieldpath.NewSet()
	for _, e := range o.GetManagedFields() {
		if e.Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		if s, err := kube.FieldSet(e); err == nil && s != nil {
			applied = applied.Union(s)
		}
	}
	lastSpec, _, _ := unstructured.NestedMap(o.LastApplied, "spec")

	var dropped []string
	for _, p := range items(spec, "ports") {
		np, ok := p["nodePort"]
		if !ok {
			continue
		}
		protocol, _ := p["protocol"].(string)
		if protocol == "" {
			protocol = "TCP"
		}
		path, err := fieldpath.MakePath("spec", "ports", fieldpath.KeyByFields("port", p["port"], "protocol", protocol), "nodePort")
		if err != nil || applied.Has(path) || lastAppliedPortSets(lastSpec, p["port"], protocol) {
			continue // a port that makes no path can't be judged: kept
		}
		delete(p, "nodePort")
		dropped = append(dropped, fmt.Sprint(np))
	}
	if hc, ok := spec["healthCheckNodePort"]; ok {
		_, inLast := lastSpec["healthCheckNodePort"]
		if !inLast && !applied.Has(fieldpath.MakePathOrDie("spec", "healthCheckNodePort")) { // constant path: cannot panic
			delete(spec, "healthCheckNodePort")
			dropped = append(dropped, fmt.Sprintf("%v (health check)", hc))
		}
	}
	if len(dropped) > 0 {
		what, verb, they, them := "node ports", "were", "they", "them"
		if len(dropped) == 1 {
			what, verb, they, them = "node port", "was", "it", "it"
		}
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "NODE_PORT_DROPPED",
			"%s %s %s assigned by the cluster, not set in the applied manifest, so %s %s left out and a new cluster assigns its own; "+
				"set %s in the manifest if something outside the cluster depends on %s",
			what, strings.Join(dropped, ", "), verb, they, verb, them, them)
	}
}

// reports whether the last-applied spec sets a nodePort for the port with this number and protocol
func lastAppliedPortSets(lastSpec map[string]any, port any, protocol string) bool {
	for _, p := range items(lastSpec, "ports") {
		pp, _ := p["protocol"].(string)
		if pp == "" {
			pp = "TCP"
		}
		if _, ok := p["nodePort"]; ok && equal(p["port"], port) && pp == protocol {
			return true
		}
	}
	return false
}

const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// serviceAccountUIDAnnotation is set by the token controller on a service account token secret
const serviceAccountUIDAnnotation = "kubernetes.io/service-account.uid"

// secret defaults
func secretDefaults(o *resource.Object) {
	if t, _ := o.Object["type"].(string); t == "kubernetes.io/service-account-token" {
		dropAnnotation(o, serviceAccountUIDAnnotation)
	}
}
