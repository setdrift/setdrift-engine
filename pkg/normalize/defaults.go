// removes the defaults the API server and admission set, where the value equals the default, as listed in pkg/apis/*/defaults.go in Kubernetes' source
// a value the user set to something else always stays, and some defaults are only removed when managedFields prove no one sent them

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

// the GroupKind of each kind that has defaults to remove, the rules table in normalize.go and podSpecPath below are keyed by them
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

// podSpecPath is where each workload kind keeps its pod spec, so podSpecDefaults finds it
var podSpecPath = map[schema.GroupKind][]string{
	podGK:         {"spec"},
	deploymentGK:  {"spec", "template", "spec"},
	replicaSetGK:  {"spec", "template", "spec"},
	statefulSetGK: {"spec", "template", "spec"},
	daemonSetGK:   {"spec", "template", "spec"},
	jobGK:         {"spec", "template", "spec"},
	cronJobGK:     {"spec", "jobTemplate", "spec", "template", "spec"},
}

// dropIf takes a map, a default value and a path, and removes the field at path if it equals the default
func dropIf(m map[string]any, def any, path ...string) {
	v, found, err := unstructured.NestedFieldNoCopy(m, path...)
	// checking if the field is missing, unreadable or set to something else, if so it stays, equal is defined below
	if !found || err != nil || !equal(v, def) {
		return
	}
	unstructured.RemoveNestedField(m, path...)
}

// equal compares two unstructured values, treating every integer type the same, since decoded json gives int64 or float64 where the defaults here are int64
func equal(a, b any) bool {
	// invoking toInt to check if a is an integer, if so b must be the same integer, its defined below
	if ai, ok := toInt(a); ok {
		bi, ok := toInt(b)
		return ok && ai == bi
	}
	return reflect.DeepEqual(a, b)
}

// toInt tells if v is an integer, of any integer type or a float64 with no fraction, and gives it back as an int64
func toInt(v any) (int64, bool) {
	// checking which numeric type v has
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		// checking if the float has no fraction, if so it counts as an integer
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

// dropEmpty removes the field at path if its an empty map or list
func dropEmpty(m map[string]any, path ...string) {
	v, found, _ := unstructured.NestedFieldNoCopy(m, path...)
	// invoking empty to check if the field is there and empty, its defined in managedfields.go
	if found && empty(v) {
		unstructured.RemoveNestedField(m, path...)
	}
}

// items gives back the maps in the list at path, skipping anything that isnt a map, and nothing when theres no list there
// the maps are the ones in the object, so changing them changes the object
func items(m map[string]any, path ...string) []map[string]any {
	list, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	s, _ := list.([]any)
	out := make([]map[string]any, 0, len(s))
	// keeping each item that is a map
	for _, item := range s {
		// checking if the item is a map, anything else is left out
		if im, ok := item.(map[string]any); ok {
			out = append(out, im)
		}
	}
	return out
}

// podSpecDefaults takes a workload or Pod and removes the defaults in its pod spec, its containers and its volumes, and in the template's metadata
func podSpecDefaults(o *resource.Object) {
	gk := o.GroupVersionKind().GroupKind()
	path, ok := podSpecPath[gk]
	// checking if the kind has a pod spec, if not theres nothing to do
	if !ok {
		return
	}
	// checking if the pod spec sits in a template, if so the template's metadata has defaults too
	if len(path) > 1 {
		// the template's metadata sits next to its spec
		meta := append(slices.Clone(path[:len(path)-1]), "metadata")
		// invoking dropIf to remove the null creationTimestamp the API server writes in templates, its defined above
		dropIf(o.Object, nil, append(meta, "creationTimestamp")...)
		// removing the timestamp kubectl rollout restart writes
		unstructured.RemoveNestedField(o.Object, append(meta, "annotations", restartedAtAnnotation)...)
		// invoking dropEmpty to remove the annotations and the metadata if that left them empty, its defined above
		dropEmpty(o.Object, append(meta, "annotations")...)
		dropEmpty(o.Object, meta...)
	}
	v, found, _ := unstructured.NestedFieldNoCopy(o.Object, path...)
	spec, ok := v.(map[string]any)
	// checking if the pod spec is missing or not a map, if so theres nothing more to do
	if !found || !ok {
		return
	}
	// invoking dropIf and dropEmpty on the hard coded defaults the cluster fills in, theyre defined above
	dropIf(spec, "ClusterFirst", "dnsPolicy")
	dropIf(spec, "default-scheduler", "schedulerName")
	dropIf(spec, int64(30), "terminationGracePeriodSeconds")
	dropIf(spec, "PreemptLowerPriority", "preemptionPolicy")
	dropIf(spec, true, "enableServiceLinks")
	dropEmpty(spec, "securityContext")
	// checking if its not a Job or CronJob, if so restartPolicy Always is the default
	// Jobs must set restartPolicy, Never or OnFailure, so theirs always stays
	if gk != jobGK && gk != cronJobGK {
		dropIf(spec, "Always", "restartPolicy")
	}
	// checking if serviceAccount, the deprecated copy of serviceAccountName, is the same as it, if so the copy goes
	if sa, ok := spec["serviceAccount"]; ok && equal(sa, spec["serviceAccountName"]) {
		delete(spec, "serviceAccount")
	}
	// checking if priorityClassName is set, if so priority was set from it by the priority admission plugin and goes
	// otherwise priority 0 is the default
	if _, ok := spec["priorityClassName"]; ok {
		delete(spec, "priority")
	} else {
		dropIf(spec, int64(0), "priority")
	}
	// going through each kind of container list
	for _, list := range []string{"containers", "initContainers", "ephemeralContainers"} {
		// invoking containerDefaults on each container, its defined below
		for _, c := range items(spec, list) {
			containerDefaults(c)
		}
	}
	// going through each volume to remove the default file modes and the empty hostPath type
	for _, vol := range items(spec, "volumes") {
		// checking both the configMap and the secret source for the default mode
		for _, src := range []string{"configMap", "secret"} {
			dropIf(vol, int64(0o644), src, "defaultMode")
		}
		dropIf(vol, int64(0o644), "projected", "defaultMode")
		dropIf(vol, "", "hostPath", "type")
		dropIf(vol, int64(0o644), "downwardAPI", "defaultMode")
	}
	// checking if its a Pod itself, if so invoking podOnlyDefaults for what admission adds to pods, its defined below
	if gk == podGK {
		podOnlyDefaults(spec)
	}
}

// replicasDefault removes replicas set to 1, the default for Deployments, ReplicaSets and StatefulSets
func replicasDefault(o *resource.Object) {
	// invoking dropIf to remove it, its defined above
	dropIf(o.Object, int64(1), "spec", "replicas")
}

// containerDefaults takes one container and removes the defaults the API server fills in, on the container, its ports and its probes
func containerDefaults(c map[string]any) {
	// invoking dropIf on the termination message defaults, its defined above
	dropIf(c, "/dev/termination-log", "terminationMessagePath")
	dropIf(c, "File", "terminationMessagePolicy")
	// checking if the container names an image, if so invoking defaultPullPolicy for the pull policy that image gets by default, its defined below
	if image, ok := c["image"].(string); ok {
		dropIf(c, defaultPullPolicy(image), "imagePullPolicy")
	}
	dropEmpty(c, "resources")
	// removing the default TCP protocol from each port
	for _, p := range items(c, "ports") {
		dropIf(p, "TCP", "protocol")
	}
	// going through each kind of probe to remove its defaults
	for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
		pm, ok := c[probe].(map[string]any)
		// checking if the container has no such probe
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

// defaultPullPolicy takes an image and gives back the pull policy it gets by default, Always for no tag or the latest tag, IfNotPresent otherwise
func defaultPullPolicy(image string) string {
	// checking if the image is pinned by digest, if so its IfNotPresent whatever the tag
	if strings.Contains(image, "@") {
		return "IfNotPresent"
	}
	// the tag is looked for after the last slash, so a registry port isnt taken for a tag
	name := image[strings.LastIndex(image, "/")+1:]
	tag := ""
	// checking if the name has a tag
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
	}
	// checking if theres no tag or its latest
	if tag == "" || tag == "latest" {
		return "Always"
	}
	return "IfNotPresent"
}

// podOnlyDefaults takes a Pod's spec and removes what admission adds to pods themselves, the default service account, the service account token volume and its mounts, and the not-ready and unreachable tolerations
func podOnlyDefaults(spec map[string]any) {
	// the ServiceAccount admission plugin sets the default service account on pods that name none
	dropIf(spec, "default", "serviceAccountName")
	var token string
	// checking if the pod has volumes, if so the token volume is taken out and its name remembered for the mounts below
	if vols, ok := spec["volumes"].([]any); ok {
		var kept []any
		// going through each volume, keeping all but the token volume
		for _, v := range vols {
			vm, _ := v.(map[string]any)
			name, _ := vm["name"].(string)
			// checking if its the kube-api-access-* token volume admission mounts into every container
			if strings.HasPrefix(name, "kube-api-access-") {
				token = name
				continue
			}
			kept = append(kept, v)
		}
		// invoking setOrDrop to write back the volumes that are left, its defined below
		setOrDrop(spec, "volumes", kept)
	}
	// checking if a token volume was found, if so its mounts come out of the containers too
	if token != "" {
		// going through the containers and the init containers
		for _, list := range []string{"containers", "initContainers"} {
			// going through each container's mounts
			for _, c := range items(spec, list) {
				mounts, ok := c["volumeMounts"].([]any)
				// checking if the container has no mounts
				if !ok {
					continue
				}
				var kept []any
				// keeping each mount that isnt the token volume's
				for _, m := range mounts {
					// checking if the mount is for some other volume than the token, if so its kept
					if mm, _ := m.(map[string]any); mm["name"] != token {
						kept = append(kept, m)
					}
				}
				// invoking setOrDrop to write back the mounts that are left, its defined below
				setOrDrop(c, "volumeMounts", kept)
			}
		}
	}
	// checking if the pod has tolerations, if so the ones admission added come out
	if tols, ok := spec["tolerations"].([]any); ok {
		var kept []any
		// invoking admissionToleration on each toleration and keeping the ones admission didnt add, its defined below
		for _, t := range tols {
			// checking if admission didnt add this toleration, if so its kept
			if !admissionToleration(t) {
				kept = append(kept, t)
			}
		}
		// invoking setOrDrop to write back the tolerations that are left, its defined below
		setOrDrop(spec, "tolerations", kept)
	}
}

// admissionToleration tells if a toleration is one the cluster adds to a pod by itself, so its not the user's
// the DefaultTolerationSeconds admission plugin adds not-ready and unreachable with NoExecute for 300 seconds, and a pod that isnt BestEffort can also get memory-pressure with NoSchedule (seen on AKS)
// left in, a re-apply puts them back in a different order and every one compares as changed
func admissionToleration(t any) bool {
	m, ok := t.(map[string]any)
	// checking if its the memory-pressure toleration, Exists and NoSchedule with nothing else set
	if ok && len(m) == 3 && m["key"] == "node.kubernetes.io/memory-pressure" && m["operator"] == "Exists" && m["effect"] == "NoSchedule" {
		return true
	}
	// checking if its a map with the operator, effect and seconds admission uses, if not its the user's
	if !ok || m["operator"] != "Exists" || m["effect"] != "NoExecute" || !equal(m["tolerationSeconds"], int64(300)) {
		return false
	}
	k := m["key"]
	return len(m) == 4 && (k == "node.kubernetes.io/not-ready" || k == "node.kubernetes.io/unreachable")
}

// setOrDrop writes list to key in m, or removes key when the list is empty, so no empty list is left behind
func setOrDrop(m map[string]any, key string, list []any) {
	// checking if the list is empty
	if len(list) == 0 {
		delete(m, key)
	} else {
		m[key] = list
	}
}

// serviceDefaults takes a Service and removes its defaults, the cluster-assigned ips and node ports, the default policies and type, and ports targeting themselves
func serviceDefaults(o *resource.Object) {
	spec, ok := o.Object["spec"].(map[string]any)
	// checking if the Service has no spec
	if !ok {
		return
	}
	// checking if GKE gave the Service its automatic NEG annotation, if so it goes, GKE adds it to every Service on a VPC-native cluster
	// a NEG annotation someone set on purpose has another value and stays
	if ann := o.GetAnnotations(); ann["cloud.google.com/neg"] == `{"ingress":true}` {
		delete(ann, "cloud.google.com/neg")
		o.SetAnnotations(ann)
	}
	// GKE's NEG controller writes the network endpoint groups it made in cloud.google.com/neg-status, state like status, a new cluster's controller writes its own
	if ann := o.GetAnnotations(); ann["cloud.google.com/neg-status"] != "" {
		delete(ann, "cloud.google.com/neg-status")
		o.SetAnnotations(ann)
	}
	// dropping the finalizers a load balancer controller adds, Kubernetes' own and each cloud's, the target's controller adds its own
	// an exported one only hurts, on another platform nothing would remove it and the Service could never be deleted
	for _, f := range o.GetFinalizers() {
		if strings.HasPrefix(f, "service.kubernetes.io/") || strings.HasPrefix(f, "service.eks.amazonaws.com/") || strings.HasPrefix(f, "service.k8s.aws/") || strings.HasPrefix(f, "gke.networking.io/") || strings.HasPrefix(f, "networking.gke.io/") {
			// invoking dropFinalizer to remove it, its defined below
			dropFinalizer(o, f)
		}
	}
	// invoking nodePorts to remove the node ports the cluster assigned, its defined below
	// it runs before the protocol default below is removed, since the protocol is part of each port's key in managedFields
	nodePorts(o, spec)
	// checking if the clusterIP is anything but None, if so it was allocated from the cluster's Service CIDR and goes
	// None, a headless Service, is a choice and stays
	if spec["clusterIP"] != "None" {
		delete(spec, "clusterIP")
		delete(spec, "clusterIPs")
	}
	// invoking dropIf on the default ip family policy, its defined above
	dropIf(spec, "SingleStack", "ipFamilyPolicy")
	// checking if the ip family policy is gone and the families are the usual IPv4 default, if so they go too
	// an explicit IPv6 single-stack choice stays, so a restore into an IPv4 cluster doesnt silently turn the Service into IPv4
	if fams, _ := spec["ipFamilies"].([]any); spec["ipFamilyPolicy"] == nil && len(fams) == 1 && fams[0] == "IPv4" {
		delete(spec, "ipFamilies")
	}
	dropIf(spec, "None", "sessionAffinity")
	dropIf(spec, "Cluster", "internalTrafficPolicy")
	dropIf(spec, "Cluster", "externalTrafficPolicy")
	dropIf(spec, true, "allocateLoadBalancerNodePorts")
	dropIf(spec, "ClusterIP", "type")
	// going through each port to remove the default protocol and a targetPort equal to the port
	for _, p := range items(spec, "ports") {
		dropIf(p, "TCP", "protocol")
		// checking if targetPort is the same as port, thats its default
		if equal(p["targetPort"], p["port"]) {
			delete(p, "targetPort")
		}
	}
}

// namespaceDefaults takes a Namespace and removes the kubernetes finalizer and the metadata.name label the API server sets on every Namespace
func namespaceDefaults(o *resource.Object) {
	// invoking dropIf and dropEmpty to remove the default finalizer and the spec it leaves empty, theyre defined above
	dropIf(o.Object, []any{"kubernetes"}, "spec", "finalizers")
	dropEmpty(o.Object, "spec")
	// checking if the kubernetes.io/metadata.name label matches the name, if so its the API server's and goes, setLabels is defined below
	if labels := o.GetLabels(); labels["kubernetes.io/metadata.name"] == o.GetName() {
		delete(labels, "kubernetes.io/metadata.name")
		setLabels(o, labels)
	}
}

// pvcDefaults removes a claim's defaults, including a storage class the cluster's default filled in, by invoking pvcDefaultsKeeping which is defined below
func pvcDefaults(o *resource.Object) { pvcDefaultsKeeping(o, false) }

// pvcDefaultsKeeping is pvcDefaults, with keepClass to keep a storage class the cluster's default filled in, for Normalizer.KeepSourceDefaults in normalize.go
// either way a finding says what happened to that class
func pvcDefaultsKeeping(o *resource.Object, keepClass bool) {
	// invoking dropIf on the default volume mode, its defined above
	dropIf(o.Object, "Filesystem", "spec", "volumeMode")
	// a claim that names no class gets the cluster's default class from the DefaultStorageClass admission plugin
	// invoking admissionSet to check if thats where the class came from, its defined below
	// checking if its kept, if so a finding says a new cluster needs a class of that name, otherwise its removed with a finding saying so
	if keepClass && admissionSet(o, "spec", "storageClassName") {
		class, _, _ := unstructured.NestedString(o.Object, "spec", "storageClassName")
		// invoking AddFinding to note the class was kept, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "STORAGE_CLASS_KEPT",
			"storage class %s was filled in from the cluster's default class; kept (--keep-source-defaults), so a new cluster needs a class named %s", class, class)
	} else if admissionSet(o, "spec", "storageClassName") {
		class, _, _ := unstructured.NestedString(o.Object, "spec", "storageClassName")
		unstructured.RemoveNestedField(o.Object, "spec", "storageClassName")
		// invoking AddFinding to note the class was left out, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "STORAGE_CLASS_DROPPED",
			"storage class %s was filled in from the cluster's default class, not set in the manifest, so it was left out "+
				"and a new cluster's default class applies; set storageClassName in the manifest to keep %s", class, class)
	}
	// invoking dropFinalizer to remove the protection finalizer, its defined below
	dropFinalizer(o, "kubernetes.io/pvc-protection")
	ann := o.GetAnnotations()
	// checking if the claim was dynamically provisioned, if so the binder chose the volume name and it goes
	if ann["volume.kubernetes.io/storage-provisioner"] != "" || ann["volume.beta.kubernetes.io/storage-provisioner"] != "" {
		unstructured.RemoveNestedField(o.Object, "spec", "volumeName")
	}
	// deleting each annotation the binder and the provisioner write
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

// admissionSet tells if the field at path is set but owned by no field manager
// the API server records managedFields before mutating admission runs, so a value an admission plugin fills in belongs to no one, while one a client sent belongs to that client
// without managedFields, like an export from files, nothing can be proven, so its false
func admissionSet(o *resource.Object, path ...string) bool {
	// checking if the field is set at all
	if _, found, _ := unstructured.NestedFieldNoCopy(o.Object, path...); !found {
		return false
	}
	entries := o.GetManagedFields()
	// checking if the object has no managedFields, if so nothing can be proven
	if len(entries) == 0 {
		return false
	}
	parts := make([]any, len(path))
	// turning the path into the parts fieldpath.MakePath takes
	for i, p := range path {
		parts[i] = p
	}
	fp, err := fieldpath.MakePath(parts...)
	// checking if the path couldnt be made, if so nothing can be proven
	if err != nil {
		return false
	}
	// going through each managedFields entry to see if any owns the field
	for _, e := range entries {
		// invoking kube.FieldSet to read the entry's fields, its defined in internal/kube/kube.go
		s, err := kube.FieldSet(e)
		// checking if the entry owns the field, or is unreadable or empty, an unreadable entry might own the field
		if err != nil || s == nil || s.Has(fp) {
			return false
		}
	}
	return true
}

// podDefaults takes a Pod and drops what binding it to a node writes, spec.nodeName from the scheduler's binding and the node's topology labels from the PodTopologyLabels admission plugin
// neither is recorded in managedFields, and exported the Pod would be bound to a node a new cluster doesnt have, so the pod garbage collector deletes it at once
func podDefaults(o *resource.Object) {
	// invoking admissionSet to check if no one sent the node name, its defined above
	if admissionSet(o, "spec", "nodeName") {
		unstructured.RemoveNestedField(o.Object, "spec", "nodeName")
	}
	// going through each topology label, removing it when no one sent it
	for _, l := range []string{"topology.kubernetes.io/zone", "topology.kubernetes.io/region"} {
		// invoking admissionSet to check if no one sent the label, its defined above
		if admissionSet(o, "metadata", "labels", l) {
			unstructured.RemoveNestedField(o.Object, "metadata", "labels", l)
		}
	}
}

// pvDefaults drops the protection finalizer the API server adds to a PersistentVolume, by invoking dropFinalizer which is defined below
func pvDefaults(o *resource.Object) { dropFinalizer(o, "kubernetes.io/pv-protection") }

// priorityClassDefaults takes a PriorityClass and removes its default preemption policy
func priorityClassDefaults(o *resource.Object) {
	// invoking dropIf to remove it, its defined above
	dropIf(o.Object, "PreemptLowerPriority", "preemptionPolicy")
}

// dropFinalizer takes an object and removes a finalizer the API server adds to every object of its kind, from StorageObjectInUseProtection admission
func dropFinalizer(o *resource.Object, name string) {
	fin := o.GetFinalizers()
	var kept []string
	// keeping every finalizer but that one
	for _, f := range fin {
		// checking if its some other finalizer, if so its kept
		if f != name {
			kept = append(kept, f)
		}
	}
	// checking if it was there, if not the finalizers are left untouched
	if len(kept) != len(fin) {
		o.SetFinalizers(kept)
	}
}

// injectedCABundles drops the CA bundles of webhooks, APIServices and CRD conversion webhooks when a component other than the one that wrote the object injected them, like cert-manager's cainjector or an operator's own cert controller
// the bundle is the source cluster's CA, the injector writes a new one in a new cluster, and an export carrying the old one conflicts with it on apply
// the evidence is a field manager that owns a caBundle but no service or url it belongs with
func injectedCABundles(o *resource.Object) {
	injected := false
	// going through each managedFields entry, looking for one that owns only the caBundle
	for _, e := range o.GetManagedFields() {
		// invoking kube.FieldSet to read the entry's fields, its defined in internal/kube/kube.go
		s, err := kube.FieldSet(e)
		// checking if the entry is unreadable or empty, if so it tells nothing
		if err != nil || s == nil {
			continue
		}
		// invoking ownsField to check if the entry owns a caBundle but no service or url, its defined below
		if ownsField(s, "caBundle") && !ownsField(s, "service") && !ownsField(s, "url") {
			injected = true
		}
	}
	// checking if no injector was found, if so the bundles are the user's and stay
	if !injected {
		return
	}
	// removing the caBundle of each webhook, then the APIService's and the CRD conversion webhook's
	for _, w := range items(o.Object, "webhooks") {
		unstructured.RemoveNestedField(w, "clientConfig", "caBundle")
	}
	unstructured.RemoveNestedField(o.Object, "spec", "caBundle")
	unstructured.RemoveNestedField(o.Object, "spec", "conversion", "webhook", "clientConfig", "caBundle")
}

// aksAdmissionsEnforcer drops the namespace selector expressions AKS's admissions enforcer adds to every webhook, so a webhook never sees AKS's own namespaces
// theyre AKS's, a new AKS cluster adds them again, exported they would carry AKS's selector to another cloud, and on an AKS target they would show as a difference
// the evidence is the enforcer's own field manager, admissionsenforcer, and only the two expressions it adds are dropped, anything else in the selector is the user's
func aksAdmissionsEnforcer(o *resource.Object) {
	enforced := false
	// going through each managedFields entry, looking for the enforcer's
	for _, e := range o.GetManagedFields() {
		// checking if its the enforcer, if so its expressions are in the object
		if e.Manager == "admissionsenforcer" {
			enforced = true
		}
	}
	// checking if the enforcer never wrote the object, if so the selectors are the user's
	if !enforced {
		return
	}
	// going through each webhook, removing the enforcer's expressions from its namespace selector
	for _, w := range items(o.Object, "webhooks") {
		exprs, _, _ := unstructured.NestedSlice(w, "namespaceSelector", "matchExpressions")
		var kept []any
		// keeping every expression but the enforcer's two
		for _, x := range exprs {
			// invoking enforcerExpression to check if its one the enforcer adds, its defined below
			if !enforcerExpression(x) {
				kept = append(kept, x)
			}
		}
		// checking if nothing was removed, if so the selector is left as it is
		if len(kept) == len(exprs) {
			continue
		}
		// checking if nothing is left, if so the expressions go, else the rest stay
		if len(kept) == 0 {
			unstructured.RemoveNestedField(w, "namespaceSelector", "matchExpressions")
		} else {
			_ = unstructured.SetNestedSlice(w, kept, "namespaceSelector", "matchExpressions")
		}
	}
}

// enforcerExpression tells if x is one of the two expressions AKS's admissions enforcer adds, control-plane NotIn [true] and kubernetes.azure.com/managedby NotIn [aks]
func enforcerExpression(x any) bool {
	m, ok := x.(map[string]any)
	// checking if its not a map, if so its not one of them
	if !ok {
		return false
	}
	key, _ := m["key"].(string)
	op, _ := m["operator"].(string)
	values, _ := m["values"].([]any)
	// checking if its a NotIn with one value, the enforcer's form
	if op != "NotIn" || len(values) != 1 {
		return false
	}
	v, _ := values[0].(string)
	return (key == "control-plane" && v == "true") || (key == "kubernetes.azure.com/managedby" && v == "aks")
}

// webhookDefaults takes a webhook configuration and removes the empty namespace and object selectors the API server sets on each webhook by default
// applied, an empty selector claims the field, and fights a platform component that writes it, like AKS's admissions enforcer
func webhookDefaults(o *resource.Object) {
	// going through each webhook, removing each selector thats empty
	for _, w := range items(o.Object, "webhooks") {
		for _, f := range []string{"namespaceSelector", "objectSelector"} {
			// checking if the selector is an empty map, if so its the default
			if sel, ok := w[f].(map[string]any); ok && len(sel) == 0 {
				delete(w, f)
			}
		}
	}
}

// ownsField tells if s holds a field named name, at any depth
func ownsField(s *fieldpath.Set, name string) bool {
	found := false
	s.Iterate(func(p fieldpath.Path) {
		// checking each element of the path for a field with that name
		for _, pe := range p {
			// checking if this element is a field with that name
			if pe.FieldName != nil && *pe.FieldName == name {
				found = true
			}
		}
	})
	return found
}

// scaleTarget is the object a HorizontalPodAutoscaler scales, by its kind, namespace and name
type scaleTarget struct {
	gk        schema.GroupKind
	namespace string
	name      string
}

// hpaTargets takes the objects and gives back each HorizontalPodAutoscaler's target, so Process in normalize.go can leave out the replicas the autoscaler sets
func hpaTargets(objs []*resource.Object) map[scaleTarget]bool {
	out := map[scaleTarget]bool{}
	// going through each object, looking for autoscalers
	for _, o := range objs {
		// checking if its not a HorizontalPodAutoscaler
		if gvk := o.GroupVersionKind(); gvk.Group != "autoscaling" || gvk.Kind != "HorizontalPodAutoscaler" {
			continue
		}
		ref, _, _ := unstructured.NestedStringMap(o.Object, "spec", "scaleTargetRef")
		// invoking kube.Group to take the group out of the target's apiVersion, its defined in internal/kube/kube.go
		out[scaleTarget{schema.GroupKind{Group: kube.Group(ref["apiVersion"]), Kind: ref["kind"]}, o.GetNamespace(), ref["name"]}] = true
	}
	return out
}

// deploymentDefaults takes a Deployment and removes its default deadline, history limit and rolling update strategy, and the revision annotation the controller writes
func deploymentDefaults(o *resource.Object) {
	// invoking dropIf on each default, its defined above
	dropIf(o.Object, int64(600), "spec", "progressDeadlineSeconds")
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	dropIf(o.Object, map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"},
	}, "spec", "strategy")
	// invoking dropAnnotation to remove the revision annotation, its defined below
	dropAnnotation(o, "deployment.kubernetes.io/revision")
}

// statefulSetDefaults takes a StatefulSet and removes its default management policy, history limit, update strategy and retention policy, and the defaults in its claim templates
func statefulSetDefaults(o *resource.Object) {
	// invoking dropIf on each default, its defined above
	dropIf(o.Object, "OrderedReady", "spec", "podManagementPolicy")
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	// trying each form the default update strategy takes
	for _, def := range []any{
		map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(0)}},
		// 1.37 also defaults maxUnavailable
		map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(0), "maxUnavailable": int64(1)}},
		map[string]any{"type": "RollingUpdate"},
	} {
		dropIf(o.Object, def, "spec", "updateStrategy")
	}
	dropIf(o.Object, map[string]any{"whenDeleted": "Retain", "whenScaled": "Retain"}, "spec", "persistentVolumeClaimRetentionPolicy")
	// removing the default volume mode and the status from each claim template
	for _, t := range items(o.Object, "spec", "volumeClaimTemplates") {
		dropIf(t, "Filesystem", "spec", "volumeMode")
		delete(t, "status")
	}
}

// daemonSetDefaults takes a DaemonSet and removes its default history limit and update strategy, and the template generation annotation the controller writes
func daemonSetDefaults(o *resource.Object) {
	// invoking dropIf on each default, its defined above
	dropIf(o.Object, int64(10), "spec", "revisionHistoryLimit")
	dropIf(o.Object, map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"maxSurge": int64(0), "maxUnavailable": int64(1)},
	}, "spec", "updateStrategy")
	// invoking dropAnnotation to remove the generation annotation, its defined below
	dropAnnotation(o, "deprecated.daemonset.template.generation")
}

// jobSpecDefaults takes a Job spec, of a Job or a CronJob's template, and removes its defaults
func jobSpecDefaults(spec map[string]any) {
	// invoking dropIf on each default, its defined above
	dropIf(spec, int64(6), "backoffLimit")
	dropIf(spec, "NonIndexed", "completionMode")
	// checking if parallelism is unset or 1, completions defaults to 1 only then
	// with parallelism set and completions unset the Job is a work queue, so completions 1 beside it was chosen
	if p, ok := spec["parallelism"]; !ok || equal(p, int64(1)) {
		dropIf(spec, int64(1), "completions")
	}
	dropIf(spec, int64(1), "parallelism")
	dropIf(spec, false, "suspend")
	dropIf(spec, false, "manualSelector")
	// checking if theres no pod failure policy, the replacement policy only defaults to TerminatingOrFailed then
	if _, ok := spec["podFailurePolicy"]; !ok {
		dropIf(spec, "TerminatingOrFailed", "podReplacementPolicy")
	}
}

// jobDefaults takes a Job and removes the defaults in its spec
func jobDefaults(o *resource.Object) {
	// checking if the Job has a spec, if so invoking jobSpecDefaults on it, its defined above
	if spec, ok := o.Object["spec"].(map[string]any); ok {
		jobSpecDefaults(spec)
	}
}

// cronJobDefaults takes a CronJob and removes its own defaults and the ones in its Job template
func cronJobDefaults(o *resource.Object) {
	// invoking dropIf on each default, its defined above
	dropIf(o.Object, "Allow", "spec", "concurrencyPolicy")
	dropIf(o.Object, int64(1), "spec", "failedJobsHistoryLimit")
	dropIf(o.Object, int64(3), "spec", "successfulJobsHistoryLimit")
	dropIf(o.Object, false, "spec", "suspend")
	// invoking dropEmpty to remove the empty metadata the API server fills in on the Job template, its defined above
	dropEmpty(o.Object, "spec", "jobTemplate", "metadata")
	// checking if the CronJob has a Job template spec
	if v, found, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "jobTemplate", "spec"); found {
		// checking if its a map, if so invoking jobSpecDefaults on it, its defined above
		if spec, ok := v.(map[string]any); ok {
			jobSpecDefaults(spec)
		}
	}
}

// networkPolicyDefaults takes a NetworkPolicy and removes its default policy types and the default protocol on its rules' ports
func networkPolicyDefaults(o *resource.Object) {
	spec, ok := o.Object["spec"].(map[string]any)
	// checking if the policy has no spec
	if !ok {
		return
	}
	// with no policyTypes the API server sets Ingress, and Egress when there are egress rules
	def := []any{"Ingress"}
	// checking if there are egress rules, if so Egress is part of the default
	if egress, _ := spec["egress"].([]any); len(egress) > 0 {
		def = append(def, "Egress")
	}
	// invoking dropIf to remove the policy types when theyre the default, its defined above
	dropIf(spec, def, "policyTypes")
	// going through the ingress and egress rules
	for _, dir := range []string{"ingress", "egress"} {
		// going through each rule's ports to remove the default TCP protocol
		for _, r := range items(spec, dir) {
			// going through each port of the rule and dropping protocol when its TCP
			for _, p := range items(r, "ports") {
				dropIf(p, "TCP", "protocol")
			}
		}
	}
}

// storageClassDefaults takes a StorageClass and removes its default reclaim policy and binding mode
func storageClassDefaults(o *resource.Object) {
	// invoking dropIf on each default, its defined above
	dropIf(o.Object, "Delete", "reclaimPolicy")
	dropIf(o.Object, "Immediate", "volumeBindingMode")
}

// dropAnnotation takes an object and removes one annotation from it, leaving the annotations untouched when its not there
func dropAnnotation(o *resource.Object, key string) {
	ann := o.GetAnnotations()
	// checking if the annotation is there
	if _, ok := ann[key]; !ok {
		return
	}
	delete(ann, key)
	o.SetAnnotations(ann)
}

// setLabels writes labels back to an object, removing the labels field when none are left
func setLabels(o *resource.Object, labels map[string]string) {
	// checking if no labels are left
	if len(labels) == 0 {
		unstructured.RemoveNestedField(o.Object, "metadata", "labels")
		return
	}
	o.SetLabels(labels)
}

// nodePorts takes a Service and its spec and removes the node ports the cluster assigned, keeping the ones a user set, and adds a finding naming what was removed
// a user set a node port when an apply's managedFields own it or the last-applied annotation has it
func nodePorts(o *resource.Object, spec map[string]any) {
	applied := fieldpath.NewSet()
	// collecting the fields every apply owns
	for _, e := range o.GetManagedFields() {
		// checking if the entry is not an apply
		if e.Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		// invoking kube.FieldSet to read the entry's fields and checking if it could, its defined in internal/kube/kube.go
		if s, err := kube.FieldSet(e); err == nil && s != nil {
			applied = applied.Union(s)
		}
	}
	lastSpec, _, _ := unstructured.NestedMap(o.LastApplied, "spec")

	var dropped []string
	// going through each port with a node port
	for _, p := range items(spec, "ports") {
		np, ok := p["nodePort"]
		// checking if the port has no node port
		if !ok {
			continue
		}
		protocol, _ := p["protocol"].(string)
		// checking if the protocol is unset, if so its TCP, as in the managedFields key
		if protocol == "" {
			protocol = "TCP"
		}
		path, err := fieldpath.MakePath("spec", "ports", fieldpath.KeyByFields("port", p["port"], "protocol", protocol), "nodePort")
		// checking if an apply or the last-applied annotation sets the node port, if so its kept, lastAppliedPortSets is defined below
		// a port that makes no path cant be judged, so its kept too
		if err != nil || applied.Has(path) || lastAppliedPortSets(lastSpec, p["port"], protocol) {
			continue
		}
		delete(p, "nodePort")
		dropped = append(dropped, fmt.Sprint(np))
	}
	// checking if the Service has a health check node port
	if hc, ok := spec["healthCheckNodePort"]; ok {
		_, inLast := lastSpec["healthCheckNodePort"]
		// checking if neither the last-applied annotation nor an apply sets it, if so the cluster assigned it
		// MakePathOrDie cant panic here, the path is constant
		if !inLast && !applied.Has(fieldpath.MakePathOrDie("spec", "healthCheckNodePort")) {
			delete(spec, "healthCheckNodePort")
			dropped = append(dropped, fmt.Sprintf("%v (health check)", hc))
		}
	}
	// checking if any node port was removed, if so a finding names them
	if len(dropped) > 0 {
		what, verb, they, them := "node ports", "were", "they", "them"
		// checking if only one was removed, if so the finding is worded for one
		if len(dropped) == 1 {
			what, verb, they, them = "node port", "was", "it", "it"
		}
		// invoking AddFinding to note the node ports were left out, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "NODE_PORT_DROPPED",
			"%s %s %s assigned by the cluster, not set in the applied manifest, so %s %s left out and a new cluster assigns its own; "+
				"set %s in the manifest if something outside the cluster depends on %s",
			what, strings.Join(dropped, ", "), verb, they, verb, them, them)
	}
}

// lastAppliedPortSets tells if the last-applied spec sets a nodePort for the port with this number and protocol
func lastAppliedPortSets(lastSpec map[string]any, port any, protocol string) bool {
	// going through each port in the last-applied spec
	for _, p := range items(lastSpec, "ports") {
		pp, _ := p["protocol"].(string)
		// checking if the protocol is unset, if so its TCP
		if pp == "" {
			pp = "TCP"
		}
		// checking if this port has a node port and the same number and protocol
		if _, ok := p["nodePort"]; ok && equal(p["port"], port) && pp == protocol {
			return true
		}
	}
	return false
}

// restartedAtAnnotation is the timestamp kubectl rollout restart writes on a pod template, podSpecDefaults removes it
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// serviceAccountUIDAnnotation is set by the token controller on a service account token secret, its the source cluster's uid so secretDefaults removes it
const serviceAccountUIDAnnotation = "kubernetes.io/service-account.uid"

// secretDefaults takes a Secret and removes the service account uid annotation from a service account token
func secretDefaults(o *resource.Object) {
	// checking if its a service account token, if so invoking dropAnnotation to remove the uid, its defined above
	if t, _ := o.Object["type"].(string); t == "kubernetes.io/service-account-token" {
		dropAnnotation(o, serviceAccountUIDAnnotation)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
