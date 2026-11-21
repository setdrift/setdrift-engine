// Package inventory records what restoring a cluster elsewhere needs that the exported manifests dont hold, the volumes behind PersistentVolumeClaims with their storage ids, and the addresses load balancers were given
// it reads the objects right after redaction, before the ownership and normalize stages drop dynamically provisioned volumes, StatefulSet claims and status
package inventory

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// zoneKeys are the node affinity keys that name a volume's availability zone, most specific first
var zoneKeys = []string{
	"topology.ebs.csi.aws.com/zone",
	"topology.kubernetes.io/zone",
	"failure-domain.beta.kubernetes.io/zone",
}

// Build takes the objects and gives back their inventory, the volumes, load balancer addresses and Helm releases, or nil when theres nothing to record
// it doesnt change the objects, but the Helm releases it lists are the objects' own records, with their chart repository filled in
func Build(objs []*resource.Object) *snapshotv1alpha1.Inventory {
	inv := &snapshotv1alpha1.Inventory{}
	pvs := map[string]*resource.Object{}
	var claims []*resource.Object
	// invoking resource.ClaimTemplatePrefixes to map "<ns>/<template>-<sts>-" to its StatefulSet, its defined in pkg/resource/resource.go
	stsClaims := resource.ClaimTemplatePrefixes(objs)
	// maps "<ns>/<release>" to the release's chart repository
	repos := map[string]string{}
	// going over each object, collecting releases, repositories, volumes, claims and load balancers
	for _, o := range objs {
		// checking if the redactor read a Helm release record from this object, if so the release is listed
		if o.HelmRelease != nil {
			inv.HelmReleases = append(inv.HelmReleases, o.HelmRelease)
		}
		// checking if the object names both its chart repository and its release, the helm post-renderer adds the ChartRepo annotation, its defined in pkg/helmsource/helmsource.go
		if ann := o.GetAnnotations(); ann[helmsource.ChartRepo] != "" && ann["meta.helm.sh/release-name"] != "" {
			ns := ann["meta.helm.sh/release-namespace"]
			// checking if the release namespace is missing, if so its the object's own
			if ns == "" {
				ns = o.GetNamespace()
			}
			repos[ns+"/"+ann["meta.helm.sh/release-name"]] = ann[helmsource.ChartRepo]
		}
		gvk := o.GroupVersionKind()
		// checking the object's group and kind for the ones the inventory reads
		switch gvk.Group + "/" + gvk.Kind {
		case "/PersistentVolume":
			pvs[o.GetName()] = o
		case "/PersistentVolumeClaim":
			claims = append(claims, o)
		case "/Service":
			// checking if the Service is a LoadBalancer, if so its addresses are recorded, invoking addLoadBalancer for that, its defined below
			if t, _, _ := unstructured.NestedString(o.Object, "spec", "type"); t == "LoadBalancer" {
				addLoadBalancer(inv, o, "status", "loadBalancer", "ingress")
			}
		case "networking.k8s.io/Ingress":
			// invoking addLoadBalancer to record the Ingress's addresses, its defined below
			addLoadBalancer(inv, o, "status", "loadBalancer", "ingress")
		}
	}

	// going over each claim to record the volume behind it
	for _, c := range claims {
		pvName, _, _ := unstructured.NestedString(c.Object, "spec", "volumeName")
		// checking if the claim is bound yet, if not theres no data to keep
		if pvName == "" {
			continue
		}
		pv := pvs[pvName]
		// checking if the PersistentVolume wasnt collected, with -n cluster-scoped types arent, so only what the claim says is known
		if pv == nil {
			// invoking resource.New to stand in an empty object, its defined in pkg/resource/resource.go
			pv = resource.New(&unstructured.Unstructured{Object: map[string]any{}})
		}
		// reading the claim's and the volume's fields, invoking str and strs for that, theyre defined below
		v := &snapshotv1alpha1.Volume{
			Namespace:        c.GetNamespace(),
			Claim:            c.GetName(),
			PersistentVolume: pvName,
			StorageClass:     str(c.Object, "spec", "storageClassName"),
			Capacity:         str(c.Object, "status", "capacity", "storage"),
			AccessModes:      strs(c.Object, "spec", "accessModes"),
			VolumeMode:       str(pv.Object, "spec", "volumeMode"),
			ReclaimPolicy:    str(pv.Object, "spec", "persistentVolumeReclaimPolicy"),
			DynamicallyProvisioned: pv.GetAnnotations()["pv.kubernetes.io/provisioned-by"] != "" ||
				pv.GetAnnotations()["volume.kubernetes.io/provisioner-deletion-secret-name"] != "",
		}
		// checking if the claim has no capacity in its status, if so the volume's capacity is used
		if v.Capacity == "" {
			v.Capacity = str(pv.Object, "spec", "capacity", "storage")
		}
		// checking if the claim names no storage class, if so the volume's is used
		if v.StorageClass == "" {
			v.StorageClass = str(pv.Object, "spec", "storageClassName")
		}
		// checking if the volume is a CSI one or an in-tree EBS one, invoking has for that, its defined below
		switch {
		case has(pv.Object, "spec", "csi"):
			v.Driver = str(pv.Object, "spec", "csi", "driver")
			v.VolumeHandle = str(pv.Object, "spec", "csi", "volumeHandle")
			v.FsType = str(pv.Object, "spec", "csi", "fsType")
		case has(pv.Object, "spec", "awsElasticBlockStore"):
			v.Driver = "kubernetes.io/aws-ebs"
			v.VolumeHandle = str(pv.Object, "spec", "awsElasticBlockStore", "volumeID")
			// checking if the volume id has a slash, the in-tree form is aws://<zone>/<volume-id> and only the id is kept
			if i := strings.LastIndex(v.VolumeHandle, "/"); i >= 0 {
				v.VolumeHandle = v.VolumeHandle[i+1:]
			}
			v.FsType = str(pv.Object, "spec", "awsElasticBlockStore", "fsType")
		}
		// invoking zone to read the availability zone the volume is tied to, its defined below
		v.Zone = zone(pv)
		// checking if the claim's name has an ordinal after a dash, if so the prefix before it may match a StatefulSet's claim template
		if i := strings.LastIndex(c.GetName(), "-"); i > 0 {
			v.StatefulSet = stsClaims[c.GetNamespace()+"/"+c.GetName()[:i+1]]
		}
		inv.Volumes = append(inv.Volumes, v)
	}

	// going over each release to fill in its chart repository from the annotations
	for _, h := range inv.HelmReleases {
		h.Repository = repos[h.Namespace+"/"+h.Name]
	}
	// checking if nothing at all was found, if so theres no inventory
	if len(inv.Volumes)+len(inv.LoadBalancers)+len(inv.HelmReleases) == 0 {
		return nil
	}
	// sorting each list so the inventory comes out the same every run
	slices.SortFunc(inv.HelmReleases, func(a, b *snapshotv1alpha1.HelmRelease) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	slices.SortFunc(inv.Volumes, func(a, b *snapshotv1alpha1.Volume) int {
		return strings.Compare(a.Namespace+"/"+a.Claim, b.Namespace+"/"+b.Claim)
	})
	slices.SortFunc(inv.LoadBalancers, func(a, b *snapshotv1alpha1.LoadBalancer) int {
		return strings.Compare(a.Kind+"/"+a.Namespace+"/"+a.Name, b.Kind+"/"+b.Namespace+"/"+b.Name)
	})
	return inv
}

// addLoadBalancer takes the inventory, a Service or Ingress and the path to its ingress list, and records the hostnames and ips it was given, when it has any
func addLoadBalancer(inv *snapshotv1alpha1.Inventory, o *resource.Object, path ...string) {
	ingress, _, _ := unstructured.NestedSlice(o.Object, path...)
	lb := &snapshotv1alpha1.LoadBalancer{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}
	// going over each ingress entry
	for _, i := range ingress {
		m, _ := i.(map[string]any)
		// checking if the entry has a hostname
		if h, _ := m["hostname"].(string); h != "" {
			lb.Hostnames = append(lb.Hostnames, h)
		}
		// checking if the entry has an ip
		if ip, _ := m["ip"].(string); ip != "" {
			lb.Ips = append(lb.Ips, ip)
		}
	}
	// checking if any address was found, a load balancer without one isnt recorded
	if len(lb.Hostnames)+len(lb.Ips) > 0 {
		inv.LoadBalancers = append(inv.LoadBalancers, lb)
	}
}

// zone takes a PersistentVolume and gives back the availability zone its node affinity requires, empty when it names none
func zone(pv *resource.Object) string {
	terms, _, _ := unstructured.NestedSlice(pv.Object, "spec", "nodeAffinity", "required", "nodeSelectorTerms")
	// going over the zone keys, most specific first, so a more specific key wins
	for _, key := range zoneKeys {
		// going over each node selector term
		for _, t := range terms {
			tm, _ := t.(map[string]any)
			exprs, _, _ := unstructured.NestedSlice(tm, "matchExpressions")
			// going over each match expression in the term
			for _, e := range exprs {
				em, _ := e.(map[string]any)
				// checking if the expression is about this key, if not its skipped
				if em["key"] != key {
					continue
				}
				// checking if the expression has values, the first one is the zone
				if vals, _ := em["values"].([]any); len(vals) > 0 {
					// checking if that value is a string
					if s, ok := vals[0].(string); ok {
						return s
					}
				}
			}
		}
	}
	return ""
}

// str reads the string at path in obj, empty when its missing or not a string
func str(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// strs reads the string list at path in obj, nil when its missing or not a string list
func strs(obj map[string]any, path ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(obj, path...)
	return s
}

// has tells if obj has any field at path
func has(obj map[string]any, path ...string) bool {
	_, ok, _ := unstructured.NestedFieldNoCopy(obj, path...)
	return ok
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
