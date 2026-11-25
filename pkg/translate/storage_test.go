package translate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// storageCase translates one class, with claims of the given sizes, and gives back its new parameters, provisioner and the notes
func storageCase(t *testing.T, class string, from, to platform.Distribution, claims ...string) (map[string]any, string, *Result) {
	t.Helper()
	objs := []*unstructured.Unstructured{u(t, "apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata: {name: fast}\n"+class)}
	for i, size := range claims {
		objs = append(objs, u(t, "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: c"+string(rune('a'+i))+", namespace: shop}\nspec: {storageClassName: fast, resources: {requests: {storage: "+size+"}}}"))
	}
	r, err := Translate(objs, from, to, load(t), nil, Source{})
	if err != nil {
		t.Fatal(err)
	}
	p := patchFor(r, "StorageClass", "fast")
	if p == nil {
		t.Fatalf("no patch: %+v", r)
	}
	return opAt(p, "/parameters").Value.(map[string]any), opAt(p, "/provisioner").Value.(string), r
}

// notes joins a result's notes, for the checks
func notes(r *Result) string { return strings.Join(r.Notes, "\n") }

// gp3's IOPS and throughput carry over to Premium SSD v2 and back, the same model on both, and what doesn't carry is said
func TestStorageCarriesPerformance(t *testing.T) {
	p, prov, r := storageCase(t, "provisioner: ebs.csi.eks.amazonaws.com\nparameters: {type: gp3, iops: \"6000\", throughput: \"250\", encrypted: \"true\", csi.storage.k8s.io/fstype: xfs}", platform.EKS, platform.AKS)
	if prov != "disk.csi.azure.com" || p["skuName"] != "PremiumV2_LRS" || p["DiskIOPSReadWrite"] != "6000" || p["DiskMBpsReadWrite"] != "250" || p["csi.storage.k8s.io/fstype"] != "xfs" {
		t.Errorf("to AKS %s %+v", prov, p)
	}
	if !strings.Contains(notes(r), "settings encrypted=true don't carry over") {
		t.Errorf("encrypted isn't said:\n%s", notes(r))
	}
	p, _, _ = storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: PremiumV2_LRS, cachingMode: None, DiskIOPSReadWrite: \"6000\", DiskMBpsReadWrite: \"250\"}", platform.AKS, platform.EKS)
	if p["type"] != "gp3" || p["iops"] != "6000" || p["throughput"] != "250" {
		t.Errorf("back to EKS %+v", p)
	}
}

// provisioned-IOPS disks: io2 becomes Premium SSD v2, Ultra Disk above its 80,000, and Ultra comes to EKS as io2, capped at 256,000
func TestStorageProvisionedIOPS(t *testing.T) {
	p, _, _ := storageCase(t, "provisioner: ebs.csi.aws.com\nparameters: {type: io2, iops: \"20000\"}", platform.EKS, platform.AKS)
	if p["skuName"] != "PremiumV2_LRS" || p["DiskIOPSReadWrite"] != "20000" {
		t.Errorf("io2 20000 %+v", p)
	}
	p, _, _ = storageCase(t, "provisioner: ebs.csi.aws.com\nparameters: {type: io2, iops: \"100000\"}", platform.EKS, platform.AKS)
	if p["skuName"] != "UltraSSD_LRS" || p["DiskIOPSReadWrite"] != "100000" {
		t.Errorf("io2 100000 %+v", p)
	}
	p, _, r := storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: UltraSSD_LRS, cachingMode: None, DiskIOPSReadWrite: \"300000\"}", platform.AKS, platform.EKS)
	if p["type"] != "io2" || p["iops"] != "256000" || !strings.Contains(notes(r), "capped at 256000") {
		t.Errorf("Ultra 300000 %+v\n%s", p, notes(r))
	}
	// io2 without carried IOPS still sets them, io2 needs them
	p, _, _ = storageCase(t, "provisioner: pd.csi.storage.gke.io\nparameters: {type: pd-ssd}", platform.GKE, platform.EKS)
	if p["type"] != "io2" || p["iops"] != "3000" {
		t.Errorf("pd-ssd %+v", p)
	}
	// IOPS per GiB isn't read elsewhere, and is said
	_, _, r = storageCase(t, "provisioner: ebs.csi.aws.com\nparameters: {type: io1, iopsPerGB: \"50\"}", platform.EKS, platform.AKS)
	if !strings.Contains(notes(r), "sets 50 IOPS per GiB") {
		t.Errorf("per GiB %s", notes(r))
	}
}

// Azure's Premium SSD (v1), its performance set by size, becomes gp3, and an HDD class becomes st1 only when every claim is at least 125 GiB
func TestStorageAKSToEKSTiers(t *testing.T) {
	p, _, _ := storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: Premium_LRS}", platform.AKS, platform.EKS)
	if p["type"] != "gp3" {
		t.Errorf("Premium_LRS %+v", p)
	}
	p, _, _ = storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: Standard_LRS}", platform.AKS, platform.EKS, "500Gi", "200Gi")
	if p["type"] != "st1" {
		t.Errorf("HDD with big claims %+v", p)
	}
	p, _, _ = storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: Standard_LRS}", platform.AKS, platform.EKS, "500Gi", "32Gi")
	if p["type"] != "gp3" {
		t.Errorf("HDD with a 32Gi claim %+v", p)
	}
}

// a zone-redundant disk says, loudly, that EBS keeps each disk in one zone, in the notes and RESTORE.md
func TestStorageZoneRedundancyLost(t *testing.T) {
	p, _, r := storageCase(t, "provisioner: disk.csi.azure.com\nparameters: {skuName: StandardSSD_ZRS}", platform.AKS, platform.EKS)
	if p["type"] != "gp3" || !strings.Contains(notes(r), "ZONE REDUNDANCY LOST") || len(r.Lost) != 1 {
		t.Errorf("%+v\n%s", p, notes(r))
	}
	if doc := restore(r, RestoreInfo{}); !strings.Contains(doc, "**StorageClass fast's zone redundancy**") {
		t.Errorf("RESTORE.md:\n%s", doc)
	}
}

// a target whose class takes no IOPS from a StorageClass says so instead of dropping them
func TestStoragePerformanceNotTaken(t *testing.T) {
	_, _, r := storageCase(t, "provisioner: ebs.csi.eks.amazonaws.com\nparameters: {type: gp3, iops: \"6000\"}", platform.EKS, platform.GKE)
	if !strings.Contains(notes(r), "sets 6000 IOPS, which GKE's general-ssd class doesn't take") {
		t.Errorf("%s", notes(r))
	}
}

// a translated class records its original, translating on to a third platform starts from it, and back home restores it exactly
func TestStorageSourceClassRecord(t *testing.T) {
	pdssd := u(t, "apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata: {name: fast}\nprovisioner: pd.csi.storage.gke.io\nparameters: {type: pd-ssd, replication-type: none}")
	onAKS := translated(t, []*unstructured.Unstructured{pdssd}, platform.GKE, platform.AKS)
	cls := find(onAKS, "StorageClass", "fast")
	rec, _, _ := unstructured.NestedString(cls.Object, "metadata", "annotations", "setdrift.io/source-class")
	if !strings.Contains(rec, `"platform":"gke"`) || !strings.Contains(rec, `"type":"pd-ssd"`) {
		t.Fatalf("record %q", rec)
	}
	// AKS has no premium SSD of its own to read back as premium, so EKS is written from the original pd-ssd, its premium tier
	onEKS := translated(t, onAKS, platform.AKS, platform.EKS)
	if typ, _, _ := unstructured.NestedString(find(onEKS, "StorageClass", "fast").Object, "parameters", "type"); typ != "io2" {
		t.Errorf("on EKS, type %q, want io2 from the original pd-ssd", typ)
	}
	back := translated(t, onEKS, platform.EKS, platform.GKE)
	if got := find(back, "StorageClass", "fast"); !same(got, pdssd) {
		t.Errorf("back on GKE: %+v", got.Object)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
