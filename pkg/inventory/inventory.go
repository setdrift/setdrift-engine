// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package inventory records what restoring a cluster elsewhere needs that
// the exported manifests do not hold: the volumes behind
// PersistentVolumeClaims (with their storage IDs) and the addresses load
// balancers were given. It reads the objects right after redaction, before
// the ownership and normalize stages drop dynamically provisioned volumes,
// StatefulSet claims and status.
package inventory

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// zoneKeys are node affinity keys that name a volume's availability zone,
// most specific first.
var zoneKeys = []string{
	"topology.ebs.csi.aws.com/zone",
	"topology.kubernetes.io/zone",
	"failure-domain.beta.kubernetes.io/zone",
}

// Build returns the inventory of objs, or nil if there is nothing to
// record. It does not change the objects; the Helm releases it lists are the
// objects' own records, with their chart repository filled in.
func Build(objs []*resource.Object) *snapshotv1alpha1.Inventory {
	inv := &snapshotv1alpha1.Inventory{}
	pvs := map[string]*resource.Object{}
	var claims []*resource.Object
	stsClaims := resource.ClaimTemplatePrefixes(objs) // "<ns>/<template>-<sts>-" > StatefulSet
	repos := map[string]string{}                      // "<ns>/<release>" > chart repository
	for _, o := range objs {
		if o.HelmRelease != nil {
			inv.HelmReleases = append(inv.HelmReleases, o.HelmRelease)
		}
		if ann := o.GetAnnotations(); ann[helmsource.ChartRepo] != "" && ann["meta.helm.sh/release-name"] != "" {
			ns := ann["meta.helm.sh/release-namespace"]
			if ns == "" {
				ns = o.GetNamespace()
			}
			repos[ns+"/"+ann["meta.helm.sh/release-name"]] = ann[helmsource.ChartRepo]
		}
		gvk := o.GroupVersionKind()
		switch gvk.Group + "/" + gvk.Kind {
		case "/PersistentVolume":
			pvs[o.GetName()] = o
		case "/PersistentVolumeClaim":
			claims = append(claims, o)
		case "/Service":
			if t, _, _ := unstructured.NestedString(o.Object, "spec", "type"); t == "LoadBalancer" {
				addLoadBalancer(inv, o, "status", "loadBalancer", "ingress")
			}
		case "networking.k8s.io/Ingress":
			addLoadBalancer(inv, o, "status", "loadBalancer", "ingress")
		}
	}

	for _, c := range claims {
		pvName, _, _ := unstructured.NestedString(c.Object, "spec", "volumeName")
		if pvName == "" {
			continue // not bound yet: no data to keep
		}
		pv := pvs[pvName]
		if pv == nil {
			// Not collected (with -n, cluster-scoped types are not), so
			// only what the claim says is known.
			pv = resource.New(&unstructured.Unstructured{Object: map[string]any{}})
		}
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
		if v.Capacity == "" {
			v.Capacity = str(pv.Object, "spec", "capacity", "storage")
		}
		if v.StorageClass == "" {
			v.StorageClass = str(pv.Object, "spec", "storageClassName")
		}
		switch {
		case has(pv.Object, "spec", "csi"):
			v.Driver = str(pv.Object, "spec", "csi", "driver")
			v.VolumeHandle = str(pv.Object, "spec", "csi", "volumeHandle")
			v.FsType = str(pv.Object, "spec", "csi", "fsType")
		case has(pv.Object, "spec", "awsElasticBlockStore"):
			v.Driver = "kubernetes.io/aws-ebs"
			v.VolumeHandle = str(pv.Object, "spec", "awsElasticBlockStore", "volumeID")
			// The in-tree form is aws://<zone>/<volume-id>.
			if i := strings.LastIndex(v.VolumeHandle, "/"); i >= 0 {
				v.VolumeHandle = v.VolumeHandle[i+1:]
			}
			v.FsType = str(pv.Object, "spec", "awsElasticBlockStore", "fsType")
		}
		v.Zone = zone(pv)
		if i := strings.LastIndex(c.GetName(), "-"); i > 0 {
			v.StatefulSet = stsClaims[c.GetNamespace()+"/"+c.GetName()[:i+1]]
		}
		inv.Volumes = append(inv.Volumes, v)
	}

	for _, h := range inv.HelmReleases {
		h.Repository = repos[h.Namespace+"/"+h.Name]
	}
	if len(inv.Volumes)+len(inv.LoadBalancers)+len(inv.HelmReleases) == 0 {
		return nil
	}
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

func addLoadBalancer(inv *snapshotv1alpha1.Inventory, o *resource.Object, path ...string) {
	ingress, _, _ := unstructured.NestedSlice(o.Object, path...)
	lb := &snapshotv1alpha1.LoadBalancer{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}
	for _, i := range ingress {
		m, _ := i.(map[string]any)
		if h, _ := m["hostname"].(string); h != "" {
			lb.Hostnames = append(lb.Hostnames, h)
		}
		if ip, _ := m["ip"].(string); ip != "" {
			lb.Ips = append(lb.Ips, ip)
		}
	}
	if len(lb.Hostnames)+len(lb.Ips) > 0 {
		inv.LoadBalancers = append(inv.LoadBalancers, lb)
	}
}

// zone returns the availability zone a PersistentVolume's node affinity
// requires, if it names one.
func zone(pv *resource.Object) string {
	terms, _, _ := unstructured.NestedSlice(pv.Object, "spec", "nodeAffinity", "required", "nodeSelectorTerms")
	for _, key := range zoneKeys {
		for _, t := range terms {
			tm, _ := t.(map[string]any)
			exprs, _, _ := unstructured.NestedSlice(tm, "matchExpressions")
			for _, e := range exprs {
				em, _ := e.(map[string]any)
				if em["key"] != key {
					continue
				}
				if vals, _ := em["values"].([]any); len(vals) > 0 {
					if s, ok := vals[0].(string); ok {
						return s
					}
				}
			}
		}
	}
	return ""
}

func str(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func strs(obj map[string]any, path ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(obj, path...)
	return s
}

func has(obj map[string]any, path ...string) bool {
	_, ok, _ := unstructured.NestedFieldNoCopy(obj, path...)
	return ok
}
