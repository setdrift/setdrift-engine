// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func obj(m map[string]any) *resource.Object {
	return resource.New(&unstructured.Unstructured{Object: m})
}

func TestBuild(t *testing.T) {
	objs := []*resource.Object{
		obj(map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": "db", "namespace": "shop"},
			"spec": map[string]any{"volumeClaimTemplates": []any{map[string]any{"metadata": map[string]any{"name": "data"}}}}}),
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "data-db-0", "namespace": "shop"},
			"spec":   map[string]any{"volumeName": "pv-1", "storageClassName": "gp3", "accessModes": []any{"ReadWriteOnce"}},
			"status": map[string]any{"capacity": map[string]any{"storage": "10Gi"}}}),
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": "pv-1", "annotations": map[string]any{"pv.kubernetes.io/provisioned-by": "ebs.csi.eks.amazonaws.com"}},
			"spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "volumeMode": "Filesystem",
				"csi": map[string]any{"driver": "ebs.csi.eks.amazonaws.com", "volumeHandle": "vol-0123456789abcdef0", "fsType": "ext4"},
				"nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
					map[string]any{"key": "topology.kubernetes.io/zone", "operator": "In", "values": []any{"us-east-2a"}}}}}}}}}),
		// In-tree EBS, bound.
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "legacy", "namespace": "shop"},
			"spec": map[string]any{"volumeName": "pv-2"}}),
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "pv-2"},
			"spec": map[string]any{"capacity": map[string]any{"storage": "5Gi"}, "storageClassName": "gp2",
				"awsElasticBlockStore": map[string]any{"volumeID": "aws://us-east-2b/vol-0aaaaaaaaaaaaaaaa"}}}),
		// Pending: no volume yet.
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "pending", "namespace": "shop"}}),
		// Bound, but its volume was not collected (-n).
		obj(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "scoped", "namespace": "shop"},
			"spec": map[string]any{"volumeName": "pv-3"}}),
		obj(map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web", "namespace": "shop"},
			"spec":   map[string]any{"type": "LoadBalancer"},
			"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"hostname": "a.elb.amazonaws.com"}}}}}),
		obj(map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "internal", "namespace": "shop"},
			"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "10.0.0.1"}}}}}),
		obj(map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]any{"name": "web", "namespace": "shop"},
			"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "1.2.3.4"}}}}}),
	}
	inv := Build(objs)
	if len(inv.Volumes) != 3 {
		t.Fatalf("volumes %v", inv.Volumes)
	}
	v := inv.Volumes[0]
	if v.Claim != "data-db-0" || v.StatefulSet != "db" || v.VolumeHandle != "vol-0123456789abcdef0" || v.Zone != "us-east-2a" ||
		v.Capacity != "10Gi" || v.StorageClass != "gp3" || v.ReclaimPolicy != "Delete" || !v.DynamicallyProvisioned || v.FsType != "ext4" {
		t.Errorf("csi volume %v", v)
	}
	if l := inv.Volumes[1]; l.Claim != "legacy" || l.VolumeHandle != "vol-0aaaaaaaaaaaaaaaa" || l.Driver != "kubernetes.io/aws-ebs" || l.Capacity != "5Gi" || l.StorageClass != "gp2" {
		t.Errorf("in-tree volume %v", l)
	}
	if sc := inv.Volumes[2]; sc.Claim != "scoped" || sc.PersistentVolume != "pv-3" || sc.VolumeHandle != "" {
		t.Errorf("uncollected volume %v", sc)
	}
	if len(inv.LoadBalancers) != 2 || inv.LoadBalancers[0].Kind != "Ingress" || inv.LoadBalancers[1].Hostnames[0] != "a.elb.amazonaws.com" {
		t.Errorf("load balancers %v", inv.LoadBalancers)
	}
	if Build(nil) != nil {
		t.Error("empty inventory is not nil")
	}
}

func TestHelmReleases(t *testing.T) {
	rec := obj(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sh.helm.release.v1.web.v3", "namespace": "shop"}})
	rec.HelmRelease = &snapshotv1alpha1.HelmRelease{Namespace: "shop", Name: "web", Chart: "nginx"}
	other := obj(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sh.helm.release.v1.api.v1", "namespace": "shop"}})
	other.HelmRelease = &snapshotv1alpha1.HelmRelease{Namespace: "shop", Name: "api", Chart: "api"}
	sa := obj(map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "web", "namespace": "shop",
		"annotations": map[string]any{"meta.helm.sh/release-name": "web", "setdrift.io/source-repo": "https://charts.example.com"}}})
	inv := Build([]*resource.Object{rec, other, sa})
	if len(inv.HelmReleases) != 2 || inv.HelmReleases[0].Name != "api" || inv.HelmReleases[0].Repository != "" ||
		inv.HelmReleases[1].Repository != "https://charts.example.com" {
		t.Errorf("releases %v", inv.HelmReleases)
	}
}
