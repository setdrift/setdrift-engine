// verify's network policy check: a NetworkPolicy applies on any cluster, but only does anything where an engine enforces it
// when the source enforced the export's NetworkPolicies and this cluster doesnt, or verify cant tell, verify fails loudly
// unless the user accepts it on purpose, with accept-unenforced in a translation's decisions or --accept-unenforced-network-policies

package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/netpolicy"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// networkPolicyCheck is what the check found, for the printed result and the report
type networkPolicyCheck struct {
	// Policies counts the export's NetworkPolicies that were checked
	Policies int `json:"policies"`
	// Source and Target are the engines enforcing them, netpolicy.None, or empty when unknown
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	// TargetUnknown says why this cluster's enforcement couldnt be read
	TargetUnknown string `json:"targetUnknown,omitempty"`
	// Accepted says how unenforced policies were accepted, empty when they werent
	Accepted string `json:"accepted,omitempty"`
	// Failed makes the run fail, Warning is said loudly without failing it
	Failed  bool   `json:"failed"`
	Warning bool   `json:"warning,omitempty"`
	Message string `json:"message"`
}

// the system workloads and settings the check reads, to tell if this cluster enforces policies
var (
	daemonSetsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// targetNetworkPolicy reads this cluster's system DaemonSets and the VPC CNI's ConfigMap for netpolicy.Detect
// it gives back netpolicy.Unknown and why when it cant read them, like a CI identity limited to its own namespaces
// idle names the engines installed but running on no nodes yet, like GKE's calico-node before its nodes are recreated
func targetNetworkPolicy(ctx context.Context, dyn dynamic.Interface) (engine, unknownWhy, idle string) {
	var objs []*unstructured.Unstructured
	for _, ns := range netpolicy.Namespaces() {
		l, err := dyn.Resource(daemonSetsGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsForbidden(err):
			return netpolicy.Unknown, fmt.Sprintf("these credentials can't list DaemonSets in %s (verify needs list on daemonsets.apps there to see the network policy engine)", ns), ""
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return netpolicy.Unknown, fmt.Sprintf("listing DaemonSets in %s failed: %v", ns, err), ""
		}
		for i := range l.Items {
			objs = append(objs, &l.Items[i])
		}
	}
	cm, err := dyn.Resource(configMapsGVR).Namespace("kube-system").Get(ctx, netpolicy.VPCCNIConfigMap, metav1.GetOptions{})
	switch {
	case err == nil:
		objs = append(objs, cm)
	case apierrors.IsForbidden(err):
		// checking if a DaemonSet already shows an engine, if so the ConfigMap doesnt matter
		if e := netpolicy.Detect(objs); netpolicy.Enforced(e) {
			return e, "", ""
		}
		return netpolicy.Unknown, fmt.Sprintf("these credentials can't read the ConfigMap kube-system/%s (verify needs get on it to see EKS's network policy setting)", netpolicy.VPCCNIConfigMap), ""
	case !apierrors.IsNotFound(err):
		return netpolicy.Unknown, fmt.Sprintf("reading the ConfigMap kube-system/%s failed: %v", netpolicy.VPCCNIConfigMap, err), ""
	}
	// invoking netpolicy.Idle for the engines installed but not running yet, its defined in pkg/netpolicy/netpolicy.go
	return netpolicy.Detect(objs), "", strings.Join(netpolicy.Idle(objs), ", ")
}

// checkNetworkPolicies weighs the source's enforcement, this cluster's and how many policies the export has, and gives back nil when theres nothing to say
// accepted names how unenforced policies were accepted, empty when they werent, distribution is this cluster's, for how to turn enforcement on
// idle names an engine installed here but running on no nodes yet
func checkNetworkPolicies(policies int, source, target, unknownWhy, idle, accepted, distribution string) *networkPolicyCheck {
	// checking if the export has no NetworkPolicies, or the source didnt enforce them, if so nothing changes on any cluster
	if policies == 0 || source == netpolicy.None {
		return nil
	}
	c := &networkPolicyCheck{Policies: policies, Source: source, Target: target, TargetUnknown: unknownWhy}
	n := text.Plural(policies, "NetworkPolicy", "NetworkPolicies")
	how := netpolicy.HowToEnforce(distribution)
	switch {
	case netpolicy.Enforced(target):
		c.Message = fmt.Sprintf("%s enforced here by %s.", n, target)
		return c
	case source == netpolicy.Unknown:
		// an export from an earlier version, or of some namespaces, doesnt say, so its a warning, not a failure
		c.Warning = true
		c.Message = fmt.Sprintf("The export doesn't record whether the source enforced its %s (export again with this version to record it), and %s. If the source enforced them, they do nothing here: %s.", n, targetState(target, unknownWhy, idle), how)
		return c
	}
	c.Message = fmt.Sprintf("The source enforced its %s with %s, and %s, so here they apply and do nothing. To enforce them, %s.", n, source, targetState(target, unknownWhy, idle), how)
	if accepted != "" {
		c.Accepted, c.Warning = accepted, true
		c.Message += " Accepted unenforced (" + accepted + ")."
		return c
	}
	c.Failed = true
	c.Message += fmt.Sprintf(" To pass without enforcement, on purpose, choose %s for %s in the translation's %s, or pass --accept-unenforced-network-policies.", translate.AcceptUnenforced, translate.NetworkPolicyFeature, translate.DecisionsFile)
	return c
}

// targetState says what verify found about this cluster's enforcement
func targetState(target, unknownWhy, idle string) string {
	switch {
	case target == netpolicy.Unknown:
		return "verify can't tell if this cluster enforces them: " + unknownWhy
	case idle != "":
		return "this cluster doesn't enforce them yet: " + idle + " is installed but runs on no nodes"
	}
	return "this cluster doesn't enforce them"
}

// countNetworkPolicies counts the NetworkPolicies among the objects verify checks, in the namespaces it was limited to, if any
func countNetworkPolicies(objs []*unstructured.Unstructured, namespaces []string) int {
	n := 0
	for _, o := range objs {
		if o.GroupVersionKind().Group == "networking.k8s.io" && o.GetKind() == "NetworkPolicy" &&
			(len(namespaces) == 0 || slices.Contains(namespaces, o.GetNamespace())) {
			n++
		}
	}
	return n
}

// the types the served check lists
var (
	csiDriversGVR     = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "csidrivers"}
	ingressClassesGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"}
)

// served reads the names of this cluster's CSI drivers and IngressClasses, for verify's check that what objects name is there
// a list that cant be read is left nil, which skips that part of the check rather than flag what it couldnt see
func served(ctx context.Context, dyn dynamic.Interface) *verify.Served {
	names := func(gvr schema.GroupVersionResource) map[string]bool {
		l, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil
		}
		out := map[string]bool{}
		for _, o := range l.Items {
			out[o.GetName()] = true
		}
		return out
	}
	return &verify.Served{CSIDrivers: names(csiDriversGVR), IngressClasses: names(ingressClassesGVR)}
}

// acceptSingular reads --accept-unenforced-network-policy as --accept-unenforced-network-policies, so either spelling works and the help shows one
func acceptSingular(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if name == "accept-unenforced-network-policy" {
		name = "accept-unenforced-network-policies"
	}
	return pflag.NormalizedName(name)
}

// printNetworkPolicy prints the check, loudly when it fails or was accepted
func printNetworkPolicy(w io.Writer, c *networkPolicyCheck) {
	if c == nil {
		return
	}
	switch {
	case c.Failed:
		fmt.Fprintf(w, "\nNETWORK POLICIES NOT ENFORCED: %s\n", c.Message)
	case c.Warning:
		fmt.Fprintf(w, "\nWARNING, network policies: %s\n", c.Message)
	default:
		fmt.Fprintf(w, "Network policies: %s\n", c.Message)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
