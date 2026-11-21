// recognizes objects the cluster creates and keeps up to date itself, without an ownerReference to drop them by
// exported, they would make a GitOps tool fight the component that writes them, and they add noise to every namespace
// each rule names what generates the object, and Process counts them per reason
// a rule must only match objects nobody configures by hand, when an object can be either, like the default ServiceAccount, it matches only the untouched form

package ownership

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// index holds what the rules need to know about the other objects
type index struct {
	// "<namespace>/<name>" of each Service with a selector
	services map[string]bool
	// "<namespace>/<template>-<statefulset>-" for each volume claim template, the name of each claim it creates without the ordinal
	claims map[string]bool
}

// newIndex takes the objects and builds the index the rules read, the Services with a selector and the StatefulSet claim prefixes
func newIndex(objs []*resource.Object) index {
	claims := map[string]bool{}
	// invoking resource.ClaimTemplatePrefixes for the claim prefixes, its defined in pkg/resource/resource.go, only the prefixes are kept
	for prefix := range resource.ClaimTemplatePrefixes(objs) {
		claims[prefix] = true
	}
	// invoking servicesWithSelector for the Services, its defined below
	return index{services: servicesWithSelector(objs), claims: claims}
}

// generatedBy takes an object and the index and gives back what generates it, worded for a finding, or empty when its configuration
//
//revive:disable-next-line:cognitive-complexity one independent rule per kind, clearest as one switch
func generatedBy(o *resource.Object, idx index) string {
	gvk := o.GroupVersionKind()
	labels, ann := o.GetLabels(), o.GetAnnotations()
	ns, name := o.GetNamespace(), o.GetName()

	// checking the object's group and kind, each one has its own rules
	switch gvk.Group + "/" + gvk.Kind {
	case "/ConfigMap":
		// checking if its one of the ConfigMaps the cluster publishes itself
		switch {
		case name == "kube-root-ca.crt":
			return "the root CA publisher (kube-root-ca.crt ConfigMaps)"
		case ns == "kube-system" && name == "extension-apiserver-authentication":
			return "the API server (extension-apiserver-authentication)"
		case ns == "kube-system" && name == "kube-apiserver-legacy-service-account-token-tracking":
			return "the API server (legacy token tracking)"
		}
	case "/Secret":
		// checking if its a bootstrap token, those come from node bootstrapping
		if t, _ := o.Object["type"].(string); t == "bootstrap.kubernetes.io/token" {
			return "node bootstrapping (bootstrap tokens)"
		}
	case "/ServiceAccount":
		// checking if its a default ServiceAccount nobody changed, invoking untouchedDefaultServiceAccount, its defined below
		if name == "default" && untouchedDefaultServiceAccount(o) {
			return "the service account controller (default ServiceAccounts)"
		}
		// invoking controllerServiceAccount and checking if its one of kube-controller-manager's own, its defined below
		if controllerServiceAccount(o) {
			return "kube-controller-manager (its controllers' ServiceAccounts)"
		}
	case "/Service":
		// checking if its the kubernetes Service the API server keeps
		if ns == "default" && name == "kubernetes" {
			return "the API server (the kubernetes Service)"
		}
	case "/Endpoints":
		// the endpoints controller writes these for every Service with a selector, and the API server for the kubernetes Service
		// Endpoints for a Service without a selector are configuration
		// checking if its the kubernetes Service's or a selector Service's
		if (ns == "default" && name == "kubernetes") || idx.services[ns+"/"+name] {
			return "the endpoints controller (Endpoints of Services with a selector)"
		}
	case "discovery.k8s.io/EndpointSlice":
		// the slices Kubernetes controllers manage carry a managed-by label ending in .k8s.io
		// those from a Service have an owner reference too, but mirrored ones from selectorless Endpoints and the kubernetes Service's dont
		// checking if the managed-by label ends in .k8s.io
		if strings.HasSuffix(labels["endpointslice.kubernetes.io/managed-by"], ".k8s.io") {
			return "the EndpointSlice controllers"
		}
		// checking if its the kubernetes Service's slice
		if ns == "default" && name == "kubernetes" {
			return "the API server (the kubernetes Service's EndpointSlice)"
		}
	case "networking.k8s.io/ServiceCIDR":
		// checking if its the default ServiceCIDR the API server creates
		if name == "kubernetes" {
			return "the API server (the default ServiceCIDR)"
		}
	case "certificates.k8s.io/ClusterTrustBundle":
		// checking if its the bundle of the API server's serving CA
		if strings.HasPrefix(name, "kubernetes.io:kube-apiserver-serving:") {
			return "kube-controller-manager (the API server's serving CA bundle)"
		}
	case "scheduling.k8s.io/PriorityClass":
		// checking if its one of the two built-in PriorityClasses
		if name == "system-cluster-critical" || name == "system-node-critical" {
			return "the API server (built-in PriorityClasses)"
		}
	case "rbac.authorization.k8s.io/ClusterRole", "rbac.authorization.k8s.io/ClusterRoleBinding",
		"rbac.authorization.k8s.io/Role", "rbac.authorization.k8s.io/RoleBinding":
		// checking if its one of the default RBAC objects, the API server labels those
		if labels["kubernetes.io/bootstrapping"] == "rbac-defaults" {
			return "the API server (default RBAC)"
		}
	case "apiregistration.k8s.io/APIService":
		// checking if the aggregator manages it, either way it labels them
		if v := labels["kube-aggregator.kubernetes.io/automanaged"]; v == "true" || v == "onstart" {
			return "the API server (built-in APIServices)"
		}
	case "flowcontrol.apiserver.k8s.io/FlowSchema", "flowcontrol.apiserver.k8s.io/PriorityLevelConfiguration":
		// checking if the API server still keeps it up to date, "false" means someone took the object over from the API server
		if ann["apf.kubernetes.io/autoupdate-spec"] == "true" {
			return "the API server (default API priority and fairness)"
		}
	case "/PersistentVolume":
		// checking if it was dynamically provisioned, if so the PersistentVolumeClaim, which is exported, provisions a new one
		if ann["pv.kubernetes.io/provisioned-by"] != "" {
			return "dynamic provisioning (PersistentVolumes created for a claim)"
		}
	case "/PersistentVolumeClaim":
		// a claim named <template>-<statefulset>-<ordinal> was created from a volume claim template, which the StatefulSet exports
		// with the default Retain policy the claims have no ownerReference, and a new cluster's StatefulSet creates them with new volumes
		// checking if the name ends in an ordinal and the rest matches a claim template, invoking isOrdinal, its defined below
		if i := strings.LastIndex(name, "-"); i > 0 && isOrdinal(name[i+1:]) && idx.claims[ns+"/"+name[:i+1]] {
			return "StatefulSets (claims from volumeClaimTemplates)"
		}
	}
	return ""
}

// untouchedDefaultServiceAccount tells if a ServiceAccount is as the controller created it
// pull secrets, an IRSA role annotation or automountServiceAccountToken make it configuration
func untouchedDefaultServiceAccount(o *resource.Object) bool {
	// checking if it has any labels or annotations, if so someone configured it
	if len(o.GetLabels()) > 0 || len(o.GetAnnotations()) > 0 {
		return false
	}
	// going over the fields a person would set
	for _, f := range []string{"imagePullSecrets", "secrets", "automountServiceAccountToken"} {
		// checking if the field is there, if so its configuration
		if _, ok := o.Object[f]; ok {
			return false
		}
	}
	return true
}

// controllerServiceAccount tells if o is one of the ServiceAccounts kube-controller-manager, or a cloud controller manager, creates in kube-system, one per controller, when it runs with --use-service-account-credentials
// theyre created with no managed fields, which no client writing through the API can do
// the name check keeps manifests read from files, which usually lack managedFields, from matching every ServiceAccount in kube-system
func controllerServiceAccount(o *resource.Object) bool {
	// checking if its outside kube-system, has managed fields, or was changed, invoking untouchedDefaultServiceAccount, its defined above
	if o.GetNamespace() != "kube-system" || len(o.GetManagedFields()) > 0 || !untouchedDefaultServiceAccount(o) {
		return false
	}
	name := o.GetName()
	// going over the name endings the controllers' ServiceAccounts have
	for _, suffix := range []string{"-controller", "-publisher", "cleaner", "-collector", "-binder", "-autoscaler", "-cloud-provider"} {
		// checking if the name ends that way
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// servicesWithSelector takes the objects and gives back "<namespace>/<name>" of every Service that has a selector, whose Endpoints the endpoints controller writes
func servicesWithSelector(objs []*resource.Object) map[string]bool {
	out := map[string]bool{}
	// going over each object looking for Services
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		// checking if its a core Service, if not its skipped
		if gvk.Group != "" || gvk.Kind != "Service" {
			continue
		}
		// checking if the Service has a non-empty selector
		if sel, _, _ := unstructured.NestedMap(o.Object, "spec", "selector"); len(sel) > 0 {
			out[o.GetNamespace()+"/"+o.GetName()] = true
		}
	}
	return out
}

// isOrdinal tells if s is a StatefulSet ordinal, one or more digits and nothing else
func isOrdinal(s string) bool {
	// checking if its empty, an empty string isnt an ordinal
	if s == "" {
		return false
	}
	// going over each character
	for _, r := range s {
		// checking if its not a digit
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
