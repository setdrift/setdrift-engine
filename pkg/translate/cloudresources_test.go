package translate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// ackBucket is an ACK Bucket with versioning on and public access blocked, and a setting no other cloud reads the same way
const ackBucket = `
apiVersion: s3.services.k8s.aws/v1alpha1
kind: Bucket
metadata: {name: acme-orders, namespace: shop}
spec:
  name: acme-orders.prod
  versioning: {status: Enabled}
  publicAccessBlock: {blockPublicACLs: true, blockPublicPolicy: true, ignorePublicACLs: true, restrictPublicBuckets: true}
  tagging: {tagSet: [{key: team, value: orders}]}`

// a held-back cloud-resource object stops the translation with keep-source, equivalent and leave-out, and nothing is patched
func TestHeldCloudResourceDecision(t *testing.T) {
	src := Source{Held: []*unstructured.Unstructured{u(t, ackBucket)}}
	r, err := Translate(nil, platform.EKS, platform.GKE, load(t), nil, src)
	if err != nil {
		t.Fatal(err)
	}
	und := r.Undecided()
	if len(und) != 1 || und[0].Feature != "cloud-resource/Bucket/shop/acme-orders" || !slices.Equal(und[0].Options, []string{KeepSource, Equivalent, LeaveOut}) {
		t.Fatalf("decision %+v", r.Decisions)
	}
	if !strings.Contains(und[0].What, "real AWS resource through AWS Controllers for Kubernetes (ACK)") {
		t.Errorf("what: %s", und[0].What)
	}
	if len(r.Patches) != 0 {
		t.Errorf("a held-back object isn't in the generic set, nothing to patch: %+v", r.Patches)
	}
	// keep-source writes nothing, and RESTORE.md says to set the retain setting before deleting the source's object
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "cloud-resource/Bucket/shop/acme-orders", Choose: KeepSource}}, src)
	if len(r.Undecided()) != 0 || len(r.Added) != 0 || len(r.CloudResources) != 1 {
		t.Fatalf("keep-source %+v", r)
	}
	if doc := restore(r, RestoreInfo{Cluster: "prod"}); !strings.Contains(doc, "services.k8s.aws/deletion-policy: retain") {
		t.Errorf("RESTORE.md doesn't say to retain the bucket:\n%s", doc)
	}
}

// equivalent writes the target's bucket, carrying the name, versioning and public access, and lists what to fill in and set by hand
func TestBucketEquivalents(t *testing.T) {
	src := Source{Held: []*unstructured.Unstructured{u(t, ackBucket)}}
	eq := []Decision{{Feature: "cloud-resource/Bucket/shop/acme-orders", Choose: Equivalent}}

	r, err := Translate(nil, platform.EKS, platform.GKE, load(t), eq, src)
	if err != nil || len(r.Added) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	sb := r.Added[0]
	spec := sb.Object["spec"].(map[string]any)
	if sb.GetKind() != "StorageBucket" || sb.GetNamespace() != "shop" || spec["resourceID"] != "acme-orders.prod" ||
		spec["publicAccessPrevention"] != "enforced" || spec["versioning"].(map[string]any)["enabled"] != true || spec["location"] != locationPlaceholder {
		t.Errorf("StorageBucket %+v", sb.Object)
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, "set location") || !strings.Contains(notes, "set by hand what didn't carry over: tagging") {
		t.Errorf("notes %s", notes)
	}

	// on AKS a bucket is a container in a storage account, with names Azure allows
	r, _ = Translate(nil, platform.EKS, platform.AKS, load(t), eq, src)
	var kinds []string
	for _, a := range r.Added {
		kinds = append(kinds, a.GetKind()+"/"+a.GetName())
	}
	if !slices.Equal(kinds, []string{"StorageAccount/acmeordersprod", "StorageAccountsBlobService/acmeordersprod-blob", "StorageAccountsBlobServicesContainer/acme-orders"}) {
		t.Errorf("AKS %v", kinds)
	}
	if c := r.Added[2].Object["spec"].(map[string]any); c["azureName"] != "acme-orders-prod" || c["publicAccess"] != "None" {
		t.Errorf("container %+v", c)
	}

	// a Config Connector bucket going to EKS becomes an ACK Bucket
	gcs := u(t, `
apiVersion: storage.cnrm.cloud.google.com/v1beta1
kind: StorageBucket
metadata: {name: acme-media, namespace: shop}
spec: {location: US, versioning: {enabled: false}, publicAccessPrevention: inherited}`)
	r, _ = Translate(nil, platform.GKE, platform.EKS, load(t), []Decision{{Feature: "cloud-resource/StorageBucket/shop/acme-media", Choose: Equivalent}}, Source{Held: []*unstructured.Unstructured{gcs}})
	if len(r.Added) != 1 || r.Added[0].GetKind() != "Bucket" {
		t.Fatalf("EKS %+v", r.Added)
	}
	if spec := r.Added[0].Object["spec"].(map[string]any); spec["name"] != "acme-media" || spec["publicAccessBlock"] != nil || spec["versioning"].(map[string]any)["status"] != "Suspended" {
		t.Errorf("ACK Bucket %+v", spec)
	}

	// a kind with no equivalent SetDrift knows offers only keep-source and leave-out, and equivalent is refused
	table := u(t, `
apiVersion: dynamodb.services.k8s.aws/v1alpha1
kind: Table
metadata: {name: orders, namespace: shop}`)
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), nil, Source{Held: []*unstructured.Unstructured{table}})
	if !slices.Equal(r.Decisions[0].Options, []string{KeepSource, LeaveOut}) {
		t.Errorf("table options %v", r.Decisions[0].Options)
	}
	if _, err := Translate(nil, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "cloud-resource/Table/shop/orders", Choose: Equivalent}}, Source{Held: []*unstructured.Unstructured{table}}); err == nil || !strings.Contains(err.Error(), "isn't an option here") {
		t.Errorf("equivalent for a Table: %v", err)
	}
}

// the target's own operator runs there, so its objects need no decision, the export's RESTORE.md moves them
func TestTargetsOwnOperator(t *testing.T) {
	gcs := u(t, `
apiVersion: storage.cnrm.cloud.google.com/v1beta1
kind: StorageBucket
metadata: {name: acme-media, namespace: shop}`)
	r, err := Translate(nil, platform.EKS, platform.GKE, load(t), nil, Source{Held: []*unstructured.Unstructured{gcs}})
	if err != nil || len(r.Decisions) != 0 || len(r.CloudResources) != 1 || r.CloudResources[0].Choice != "" {
		t.Fatalf("%v %+v", err, r)
	}
	if doc := restore(r, RestoreInfo{}); !strings.Contains(doc, `"Cloud resources" section of RESTORE.md`) {
		t.Errorf("RESTORE.md:\n%s", doc)
	}
}

// the overlay lists the equivalents, and a later run without them removes their files
func TestWriteEquivalents(t *testing.T) {
	root := t.TempDir()
	src := Source{Held: []*unstructured.Unstructured{u(t, ackBucket)}}
	r, _ := Translate(nil, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "cloud-resource/Bucket/shop/acme-orders", Choose: Equivalent}}, src)
	if err := Write(root, r, []string{"namespaces/shop"}, "v0.3.0", RestoreInfo{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "platform", "gcp", "gke")
	k, _ := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if !strings.Contains(string(k), "equivalents/shop-storagebucket-acme-orders.yaml") {
		t.Errorf("kustomization doesn't list the equivalent:\n%s", k)
	}
	if _, err := os.Stat(filepath.Join(dir, "equivalents", "shop-storagebucket-acme-orders.yaml")); err != nil {
		t.Error(err)
	}
	r, _ = Translate(nil, platform.EKS, platform.GKE, load(t), []Decision{{Feature: "cloud-resource/Bucket/shop/acme-orders", Choose: LeaveOut}}, src)
	if err := Write(root, r, []string{"namespaces/shop"}, "v0.3.0", RestoreInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "equivalents")); !os.IsNotExist(err) {
		t.Errorf("equivalents/ should be gone after leave-out: %v", err)
	}
}

// an export whose source enforced NetworkPolicies gets a decision, require by default, a source without enforcement none, and an unknown source a note
func TestNetworkPolicyDecision(t *testing.T) {
	np := u(t, `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: deny-all, namespace: shop}
spec: {podSelector: {}}`)
	objs := []*unstructured.Unstructured{np}
	r, err := Translate(objs, platform.GKE, platform.EKS, load(t), nil, Source{NetworkPolicy: "GKE Dataplane V2"})
	if err != nil || len(r.Decisions) != 1 || r.Decisions[0].Feature != NetworkPolicyFeature || r.Decisions[0].Default != Require || len(r.Undecided()) != 0 {
		t.Fatalf("%v %+v", err, r.Decisions)
	}
	if NetworkPolicyChoice(r.Decisions) != Require || !strings.Contains(restore(r, RestoreInfo{}), "Turn on network policy enforcement") {
		t.Error("require, and the RESTORE.md step")
	}
	r, _ = Translate(objs, platform.GKE, platform.EKS, load(t), []Decision{{Feature: NetworkPolicyFeature, Choose: AcceptUnenforced}}, Source{NetworkPolicy: "GKE Dataplane V2"})
	if NetworkPolicyChoice(r.Decisions) != AcceptUnenforced || !strings.Contains(restore(r, RestoreInfo{}), "accepts them unenforced") {
		t.Error("accept-unenforced")
	}
	if r, _ := Translate(objs, platform.GKE, platform.EKS, load(t), nil, Source{NetworkPolicy: "none"}); len(r.Decisions) != 0 {
		t.Errorf("a source that didn't enforce: %+v", r.Decisions)
	}
	r, _ = Translate(objs, platform.GKE, platform.EKS, load(t), nil, Source{})
	if len(r.Decisions) != 0 || !strings.Contains(strings.Join(r.Notes, " "), "doesn't record whether the source enforced its 1 NetworkPolicy") {
		t.Errorf("unknown source: %+v %v", r.Decisions, r.Notes)
	}
	// require and accept-unenforced are valid choices in the file, keep-source isn't one for this entry
	if _, err := Translate(objs, platform.GKE, platform.EKS, load(t), []Decision{{Feature: NetworkPolicyFeature, Choose: KeepSource}}, Source{NetworkPolicy: "Calico"}); err == nil {
		t.Error("keep-source for NetworkPolicies should be refused")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, DecisionsFile), []byte("- feature: "+NetworkPolicyFeature+"\n  choose: accept-unenforced\n"), 0o600)
	if ds, err := ReadDecisions(dir); err != nil || NetworkPolicyChoice(ds) != AcceptUnenforced {
		t.Errorf("%v %+v", err, ds)
	}
}

// a translation that stopped for decisions left only the decisions file, and the next run writes over it, a directory translate didnt write is still refused
func TestWriteAfterStopping(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "platform", "gcp", "gke")
	r, _ := Translate(nil, platform.EKS, platform.GKE, load(t), nil, Source{})
	if err := WriteDecisions(dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := Write(root, r, nil, "v0.3.0", RestoreInfo{}); err != nil {
		t.Errorf("after stopping: %v", err)
	}
	other := t.TempDir()
	odir := filepath.Join(other, "platform", "gcp", "gke")
	_ = os.MkdirAll(odir, 0o750)
	_ = os.WriteFile(filepath.Join(odir, "kustomization.yaml"), []byte("resources: []\n"), 0o600)
	if err := Write(other, r, nil, "v0.3.0", RestoreInfo{}); err == nil || !strings.Contains(err.Error(), "wasn't written by setdrift translate") {
		t.Errorf("someone else's directory: %v", err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
