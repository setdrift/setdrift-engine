// the equivalents translate knows for cloud-resource operator objects, chosen with equivalent in translate-decisions.yaml
// object storage buckets for now, an ACK Bucket, a Config Connector StorageBucket or an Azure Service Operator blob container, each written as the target's
// only the settings that mean the same on every cloud carry over, the bucket's name, versioning and public access blocked, the rest are listed for the user to set
// the equivalent is a new, empty bucket, the data stays in the source's and is copied separately

package translate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// bucket is an object storage bucket's settings, as every cloud reads them
type bucket struct {
	// Name is the bucket's name on its cloud
	Name string
	// Versioning and PublicBlocked are known when VersioningKnown and PublicKnown say so
	Versioning, VersioningKnown bool
	PublicBlocked, PublicKnown  bool
	// Unmapped are the source's spec settings with no counterpart carried over, for the user to set by hand
	Unmapped []string
}

// readBucket reads a bucket from an ACK Bucket, a Config Connector StorageBucket or an Azure Service Operator blob container, ok false for anything else
func readBucket(o *unstructured.Unstructured) (bucket, bool) {
	spec, _, _ := unstructured.NestedMap(o.Object, "spec")
	b := bucket{Name: o.GetName()}
	var mapped []string
	switch o.GroupVersionKind().Group + "/" + o.GetKind() {
	case "s3.services.k8s.aws/Bucket":
		if n, _ := spec["name"].(string); n != "" {
			b.Name = n
		}
		if v, ok, _ := unstructured.NestedString(spec, "versioning", "status"); ok {
			b.Versioning, b.VersioningKnown = v == "Enabled", true
		}
		if pab, ok, _ := unstructured.NestedMap(spec, "publicAccessBlock"); ok {
			b.PublicKnown, b.PublicBlocked = true, true
			for _, k := range []string{"blockPublicACLs", "blockPublicPolicy", "ignorePublicACLs", "restrictPublicBuckets"} {
				if on, _ := pab[k].(bool); !on {
					b.PublicBlocked = false
				}
			}
		}
		mapped = []string{"name", "versioning", "publicAccessBlock", "createBucketConfiguration"}
	case "storage.cnrm.cloud.google.com/StorageBucket":
		if n, _ := spec["resourceID"].(string); n != "" {
			b.Name = n
		}
		if v, ok, _ := unstructured.NestedBool(spec, "versioning", "enabled"); ok {
			b.Versioning, b.VersioningKnown = v, true
		}
		if v, ok := spec["publicAccessPrevention"].(string); ok {
			b.PublicBlocked, b.PublicKnown = v == "enforced", true
		}
		mapped = []string{"resourceID", "versioning", "publicAccessPrevention", "location", "uniformBucketLevelAccess"}
	case "storage.azure.com/StorageAccountsBlobServicesContainer":
		if n, _ := spec["azureName"].(string); n != "" {
			b.Name = n
		}
		v, _ := spec["publicAccess"].(string)
		b.PublicBlocked, b.PublicKnown = v == "" || v == "None", true
		mapped = []string{"azureName", "publicAccess", "owner"}
	default:
		return bucket{}, false
	}
	for k := range spec {
		if !slices.Contains(mapped, k) {
			b.Unmapped = append(b.Unmapped, k)
		}
	}
	slices.Sort(b.Unmapped)
	return b, true
}

// placeholders the user fills in, the apply fails until they do
const (
	locationPlaceholder      = "<location>"
	resourceGroupPlaceholder = "<resource group>"
)

// bucketEquivalent writes the target's bucket for o, by the target cloud's bucket strategy, with what to fill in and set by hand, nil when o isnt a bucket or the target has none SetDrift knows
func bucketEquivalent(m *capability.Map, o *unstructured.Unstructured, to platform.Distribution) ([]*unstructured.Unstructured, []string) {
	b, ok := readBucket(o)
	c := m.CloudOf(to)
	if !ok || c == nil || bucketWriters[c.Buckets] == nil {
		return nil, nil
	}
	ns := o.GetNamespace()
	meta := func(name string) map[string]any {
		m := map[string]any{"name": name}
		if ns != "" {
			m["namespace"] = ns
		}
		return m
	}
	obj := func(apiVersion, kind, name string, spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta(name), "spec": spec}}
	}
	// invoking the cloud's bucket writer, theyre listed in strategies.go and defined below
	out, notes := bucketWriters[c.Buckets](o, b, obj)
	if len(b.Unmapped) > 0 {
		notes = append(notes, "set by hand what didn't carry over: "+strings.Join(b.Unmapped, ", "))
	}
	return out, notes
}

// newObject makes an object of the target's for a bucket equivalent, in the source object's namespace
type newObject func(apiVersion, kind, name string, spec map[string]any) *unstructured.Unstructured

// ackS3Bucket writes an ACK Bucket, an S3 bucket on AWS
func ackS3Bucket(o *unstructured.Unstructured, b bucket, obj newObject) (out []*unstructured.Unstructured, notes []string) {
	spec := map[string]any{"name": b.Name, "createBucketConfiguration": map[string]any{"locationConstraint": locationPlaceholder}}
	if b.VersioningKnown {
		spec["versioning"] = map[string]any{"status": map[bool]string{true: "Enabled", false: "Suspended"}[b.Versioning]}
	}
	if b.PublicKnown && b.PublicBlocked {
		spec["publicAccessBlock"] = map[string]any{"blockPublicACLs": true, "blockPublicPolicy": true, "ignorePublicACLs": true, "restrictPublicBuckets": true}
	}
	out = append(out, obj("s3.services.k8s.aws/v1alpha1", "Bucket", o.GetName(), spec))
	notes = append(notes, "set createBucketConfiguration.locationConstraint to the AWS region")
	return out, notes
}

// configConnectorBucket writes a Config Connector StorageBucket, a Cloud Storage bucket on Google Cloud
func configConnectorBucket(o *unstructured.Unstructured, b bucket, obj newObject) (out []*unstructured.Unstructured, notes []string) {
	spec := map[string]any{"resourceID": b.Name, "location": locationPlaceholder, "uniformBucketLevelAccess": true}
	if b.VersioningKnown {
		spec["versioning"] = map[string]any{"enabled": b.Versioning}
	}
	if b.PublicKnown {
		spec["publicAccessPrevention"] = map[bool]string{true: "enforced", false: "inherited"}[b.PublicBlocked]
	}
	out = append(out, obj("storage.cnrm.cloud.google.com/v1beta1", "StorageBucket", o.GetName(), spec))
	notes = append(notes, "set location to the Google Cloud location")
	return out, notes
}

// azureBlobContainer writes Azure Service Operator's storage account, blob service and container, Azure keeps blobs in a container of a storage account
func azureBlobContainer(o *unstructured.Unstructured, b bucket, obj newObject) (out []*unstructured.Unstructured, notes []string) {
	// Azure keeps blobs in a container of a storage account, so the account and its blob service come with it
	acct := storageAccountName(b.Name)
	account := map[string]any{"location": locationPlaceholder, "kind": "StorageV2", "sku": map[string]any{"name": "Standard_LRS"},
		"owner": map[string]any{"name": resourceGroupPlaceholder}}
	if b.PublicKnown {
		account["allowBlobPublicAccess"] = !b.PublicBlocked
	}
	svc := map[string]any{"owner": map[string]any{"name": acct}}
	if b.VersioningKnown {
		svc["isVersioningEnabled"] = b.Versioning
	}
	container := map[string]any{"azureName": containerName(b.Name), "owner": map[string]any{"name": acct + "-blob"}}
	if b.PublicKnown && b.PublicBlocked {
		container["publicAccess"] = "None"
	}
	out = append(out,
		obj("storage.azure.com/v1api20230101", "StorageAccount", acct, account),
		obj("storage.azure.com/v1api20230101", "StorageAccountsBlobService", acct+"-blob", svc),
		obj("storage.azure.com/v1api20230101", "StorageAccountsBlobServicesContainer", o.GetName(), container))
	notes = append(notes, fmt.Sprintf("set the storage account %s's location to the Azure region and owner.name to its Azure Service Operator ResourceGroup (or owner.armId to an existing one)", acct))
	return out, notes
}

// nonAlnum matches what an Azure storage account name can't hold
var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

// storageAccountName makes a bucket name an Azure storage account name, 3 to 24 lowercase letters and digits, unique across Azure
func storageAccountName(name string) string {
	n := nonAlnum.ReplaceAllString(strings.ToLower(name), "")
	if len(n) > 24 {
		n = n[:24]
	}
	for len(n) < 3 {
		n += "0"
	}
	return n
}

// containerName makes a bucket name an Azure blob container name, 3 to 63 lowercase letters, digits and single hyphens
func containerName(name string) string {
	n := regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(strings.ToLower(name), "-")
	n = strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(n, "-"), "-")
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	for len(n) < 3 {
		n += "0"
	}
	return n
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
