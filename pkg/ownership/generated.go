package ownership

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// Objects the cluster creates and keeps up to date itself, without an
// ownerReference to drop them by. Exported, they would make a GitOps tool
// fight the component that writes them, and they add noise to every
// namespace.
//
// Each rule names what generates the object; Process counts them per
// reason. A rule must only match objects nobody configures by hand: when an
// object can be either (the default ServiceAccount), it matches only the
// untouched form.

// index holds what the rules need to know about the other objects.
type index struct {
	// services is "<namespace>/<name>" of each Service with a selector
	services map[string]bool
	// claims is "<namespace>/<template>-<statefulset>-" for each volume
	// claim template, the name of each claim it creates without the ordinal
	claims map[string]bool
}

func newIndex(objs []*resource.Object) index {
	claims := map[string]bool{}
	for prefix := range resource.ClaimTemplatePrefixes(objs) {
		claims[prefix] = true
	}
	return index{services: servicesWithSelector(objs), claims: claims}
}

// generatedBy returns what generates o, or "" if o is configuration.
//
//revive:disable-next-line:cognitive-complexity one independent rule per kind, clearest as one switch
func generatedBy(o *resource.Object, idx index) string {
	gvk := o.GroupVersionKind()
	labels, ann := o.GetLabels(), o.GetAnnotations()
	ns, name := o.GetNamespace(), o.GetName()

	switch gvk.Group + "/" + gvk.Kind {
	case "/ConfigMap":
		switch {
		case name == "kube-root-ca.crt":
			return "the root CA publisher (kube-root-ca.crt ConfigMaps)"
		case ns == "kube-system" && name == "extension-apiserver-authentication":
			return "the API server (extension-apiserver-authentication)"
		case ns == "kube-system" && name == "kube-apiserver-legacy-service-account-token-tracking":
			return "the API server (legacy token tracking)"
		}
	case "/Secret":
		if t, _ := o.Object["type"].(string); t == "bootstrap.kubernetes.io/token" {
			return "node bootstrapping (bootstrap tokens)"
		}
	case "/ServiceAccount":
		if name == "default" && untouchedDefaultServiceAccount(o) {
			return "the service account controller (default ServiceAccounts)"
		}
		if controllerServiceAccount(o) {
			return "kube-controller-manager (its controllers' ServiceAccounts)"
		}
	case "/Service":
		if ns == "default" && name == "kubernetes" {
			return "the API server (the kubernetes Service)"
		}
	case "/Endpoints":
		// Written by the endpoints controller for every Service with a
		// selector, and by the API server for the kubernetes Service.
		// Endpoints for a Service without a selector are configuration.
		if (ns == "default" && name == "kubernetes") || idx.services[ns+"/"+name] {
			return "the endpoints controller (Endpoints of Services with a selector)"
		}
	case "discovery.k8s.io/EndpointSlice":
		// Slices the Kubernetes controllers manage carry a managed-by
		// label ending in .k8s.io; those from a Service have an owner
		// reference too, but mirrored ones (from selectorless Endpoints)
		// and the kubernetes Service's do not.
		if strings.HasSuffix(labels["endpointslice.kubernetes.io/managed-by"], ".k8s.io") {
			return "the EndpointSlice controllers"
		}
		if ns == "default" && name == "kubernetes" {
			return "the API server (the kubernetes Service's EndpointSlice)"
		}
	case "networking.k8s.io/ServiceCIDR":
		if name == "kubernetes" {
			return "the API server (the default ServiceCIDR)"
		}
	case "certificates.k8s.io/ClusterTrustBundle":
		if strings.HasPrefix(name, "kubernetes.io:kube-apiserver-serving:") {
			return "kube-controller-manager (the API server's serving CA bundle)"
		}
	case "scheduling.k8s.io/PriorityClass":
		if name == "system-cluster-critical" || name == "system-node-critical" {
			return "the API server (built-in PriorityClasses)"
		}
	case "rbac.authorization.k8s.io/ClusterRole", "rbac.authorization.k8s.io/ClusterRoleBinding",
		"rbac.authorization.k8s.io/Role", "rbac.authorization.k8s.io/RoleBinding":
		if labels["kubernetes.io/bootstrapping"] == "rbac-defaults" {
			return "the API server (default RBAC)"
		}
	case "apiregistration.k8s.io/APIService":
		if v := labels["kube-aggregator.kubernetes.io/automanaged"]; v == "true" || v == "onstart" {
			return "the API server (built-in APIServices)"
		}
	case "flowcontrol.apiserver.k8s.io/FlowSchema", "flowcontrol.apiserver.k8s.io/PriorityLevelConfiguration":
		// "false" means someone took the object over from the API server.
		if ann["apf.kubernetes.io/autoupdate-spec"] == "true" {
			return "the API server (default API priority and fairness)"
		}
	case "/PersistentVolume":
		// Dynamically provisioned: the PersistentVolumeClaim, which is
		// exported, provisions a new one.
		if ann["pv.kubernetes.io/provisioned-by"] != "" {
			return "dynamic provisioning (PersistentVolumes created for a claim)"
		}
	case "/PersistentVolumeClaim":
		// <template>-<statefulset>-<ordinal>: created from a volume claim
		// template, which the StatefulSet exports. With the default Retain
		// policy the claims have no ownerReference. A new cluster's
		// StatefulSet creates them, with new volumes.
		if i := strings.LastIndex(name, "-"); i > 0 && isOrdinal(name[i+1:]) && idx.claims[ns+"/"+name[:i+1]] {
			return "StatefulSets (claims from volumeClaimTemplates)"
		}
	}
	return ""
}

// untouchedDefaultServiceAccount reports whether a default ServiceAccount
// is as the controller created it. Pull secrets, an IRSA role annotation or
// automountServiceAccountToken make it configuration.
func untouchedDefaultServiceAccount(o *resource.Object) bool {
	if len(o.GetLabels()) > 0 || len(o.GetAnnotations()) > 0 {
		return false
	}
	for _, f := range []string{"imagePullSecrets", "secrets", "automountServiceAccountToken"} {
		if _, ok := o.Object[f]; ok {
			return false
		}
	}
	return true
}

// controllerServiceAccount reports whether o is one of the ServiceAccounts
// kube-controller-manager (and a cloud controller manager) creates in
// kube-system, one per controller, when it runs with
// --use-service-account-credentials. They are created with no managed
// fields, which no client writing through the API can do; the name check
// keeps manifests read from files (which usually lack managedFields) from
// matching every ServiceAccount in kube-system.
func controllerServiceAccount(o *resource.Object) bool {
	if o.GetNamespace() != "kube-system" || len(o.GetManagedFields()) > 0 || !untouchedDefaultServiceAccount(o) {
		return false
	}
	name := o.GetName()
	for _, suffix := range []string{"-controller", "-publisher", "cleaner", "-collector", "-binder", "-autoscaler", "-cloud-provider"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// servicesWithSelector returns "<namespace>/<name>" of every Service that
// has a selector, whose Endpoints the endpoints controller writes.
func servicesWithSelector(objs []*resource.Object) map[string]bool {
	out := map[string]bool{}
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		if gvk.Group != "" || gvk.Kind != "Service" {
			continue
		}
		if sel, _, _ := unstructured.NestedMap(o.Object, "spec", "selector"); len(sel) > 0 {
			out[o.GetNamespace()+"/"+o.GetName()] = true
		}
	}
	return out
}

func isOrdinal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
