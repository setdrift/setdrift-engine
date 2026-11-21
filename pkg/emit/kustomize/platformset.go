// writes the platform set, the export plus what only the source's platform serves, for restoring onto a cluster on the same platform like platform/aws/eks

package kustomize

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// eksDir is the eks platform set's directory, and awsDir its cloud's
const (
	eksDir = "platform/aws/eks"
	awsDir = "platform/aws/"
)

// exportPlatform gives the source's platform and its directory in the export (platform/aws/eks), empty when the platform isnt known with confidence
// a snapshot with an aws inventory is from eks, since --aws read the cluster
func exportPlatform(s *snapshotv1alpha1.Snapshot) (*platform.Classification, string) {
	// invoking classification to classify the source's platform, its defined in restore.go
	cls := classification(s)
	// invoking cls.Dir for the platform set's directory, its defined in pkg/platform/platform.go
	dir := cls.Dir()
	// invoking cls.Is and checking if the platform is eks or the snapshot has an aws inventory, if so its the eks set, its defined in pkg/platform/platform.go
	if cls.Is(platform.EKS) || s.GetInventory().GetAws() != nil {
		dir = eksDir
	}
	return cls, dir
}

// platformCRDKinds gives the group and kind of every CustomResourceDefinition the platform manages (left out of the export)
// their custom resources work only on that platform
func platformCRDKinds(s *snapshotv1alpha1.Snapshot) map[schema.GroupKind]bool {
	out := map[schema.GroupKind]bool{}
	// going through the resources to find the platform's crds
	for _, r := range s.GetResources() {
		// invoking isCRD and Platform, both defined in kustomize.go, to skip anything but a crd the platform manages
		if !isCRD(r) || !Platform(r) {
			continue
		}
		// crd holds the parts of the crd's manifest that name the kind it defines
		var crd struct {
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Kind string `json:"kind"`
				} `json:"names"`
			} `json:"spec"`
		}
		// decoding the manifest, checking if it parses and names a kind, if so that group and kind are the platform's
		if json.Unmarshal([]byte(r.ManifestJson), &crd) == nil && crd.Spec.Names.Kind != "" {
			out[schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}] = true
		}
	}
	return out
}

// resourceGroupKind gives r's api group and kind
func resourceGroupKind(r *snapshotv1alpha1.Resource) schema.GroupKind {
	// invoking kube.Group to take the group out of the api version, its defined in internal/kube/kube.go
	return schema.GroupKind{Group: kube.Group(r.ApiVersion), Kind: r.Kind}
}

// writePlatformSet writes the platform set at dir (platform/<cloud>/<leaf>), a kustomization of the generic set's directories plus the objects only this platform serves, and its README
// it lists the generic directories rather than the cluster's directory, since that contains this one and kustomize rejects it as a cycle
func writePlatformSet(out *outDir, dir string, cls *platform.Classification, generic, buckets []string, objects int) error {
	// the path from dir back up to the cluster's directory
	up := strings.Repeat("../", strings.Count(dir, "/")+1)
	var resources []string
	// listing each generic directory relative to dir
	for _, b := range generic {
		resources = append(resources, up+b)
	}
	// invoking sortedCopy to add the platform's own buckets sorted, its defined in restore.go
	resources = append(resources, sortedCopy(buckets)...)
	// invoking writeKustomization to write the platform set's kustomization.yaml, its defined in kustomize.go
	if err := out.writeKustomization(dir, resources); err != nil {
		return err
	}
	// invoking platformReadme for its README, its defined below, and out.write to write it, its defined in kustomize.go
	return out.write(filepath.Join(dir, "README.md"), []byte(platformReadme(dir, cls, objects)))
}

// platformReadme builds the README that explains a platform set, what it adds, how to apply it and how the platform was identified
func platformReadme(dir string, cls *platform.Classification, objects int) string {
	// invoking Name for the distribution's display name, its defined in pkg/platform/platform.go
	name := cls.Distribution.Name()
	// checking if the distribution has no name or the set is for vms, if so the infrastructure names it
	if name == "" || strings.HasSuffix(dir, "/vm") {
		name = cls.Infrastructure.Name()
		// checking if the distribution is known, if so its "<distribution> on <infrastructure>"
		if cls.Distribution != platform.Unknown {
			name = cls.Distribution.Name() + " on " + name
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# The %s set\n\n", name)
	// invoking cls.Summary to name the source's platform, its defined in pkg/platform/platform.go
	fmt.Fprintf(&b, "The source cluster ran on %s. This directory is the export for a cluster\n"+
		"on the same platform: kustomization.yaml includes the generic set (the\n"+
		"cluster's directory, which applies to any cluster)", cls.Summary())
	// checking if the set has objects of its own, if so it says how many, otherwise that its the same as the generic set
	// invoking text.Plural for the count, its defined in internal/text/text.go
	if objects > 0 {
		fmt.Fprintf(&b, " and %s that only\n%s serves: custom resources of APIs the platform provides,\n"+
			"which another cluster would reject.\n\n", text.Plural(objects, "object", "objects"), name)
	} else {
		b.WriteString(". This\nexport has no objects that only this platform serves, so the two are the\nsame.\n\n")
	}
	fmt.Fprintf(&b, "On %s, point Argo CD or Flux at this directory, or apply it:\n\n"+
		"    kubectl apply --server-side -k %s\n\n"+
		"Anywhere else, use the cluster's directory.\n", name, dir)
	// checking if the set is on aws, if so it points at the aws RESTORE.md steps
	if strings.HasPrefix(dir, awsDir) {
		fmt.Fprintf(&b, "\nRESTORE.md here and in %s/ has the steps for restoring onto\n"+
			"%s that are not Kubernetes objects; the cluster's RESTORE.md says when.\n", filepath.ToSlash(filepath.Dir(dir)), name)
	}
	// invoking evidence to list what the classification rests on, its defined below
	fmt.Fprintf(&b, "\nHow SetDrift identified the platform: %s.\n", evidence(cls))
	return b.String()
}

// evidence lists what the classification rests on, joined for one line of the README
func evidence(cls *platform.Classification) string {
	var parts []string
	// collecting the detail of each piece of evidence
	for _, e := range cls.Evidence {
		parts = append(parts, e.Detail)
	}
	// checking if theres no evidence at all, if so the README says so
	if len(parts) == 0 {
		return "no evidence recorded"
	}
	return strings.Join(parts, "; ")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
