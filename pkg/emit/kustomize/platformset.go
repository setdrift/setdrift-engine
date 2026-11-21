// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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

// eksDir is the EKS platform set's directory, and awsDir its cloud's.
const (
	eksDir = "platform/aws/eks"
	awsDir = "platform/aws/"
)

// exportPlatform is the source's platform and its directory in the export
// (platform/aws/eks), empty when the platform is not known with confidence.
// A snapshot with an AWS inventory is EKS's: --aws read the cluster.
func exportPlatform(s *snapshotv1alpha1.Snapshot) (*platform.Classification, string) {
	cls := classification(s)
	dir := cls.Dir()
	if cls.Is(platform.EKS) || s.GetInventory().GetAws() != nil {
		dir = eksDir
	}
	return cls, dir
}

// platformCRDKinds returns the group and kind of every CustomResourceDefinition
// the platform manages (left out of the export): their custom resources work
// only on that platform.
func platformCRDKinds(s *snapshotv1alpha1.Snapshot) map[schema.GroupKind]bool {
	out := map[schema.GroupKind]bool{}
	for _, r := range s.GetResources() {
		if !isCRD(r) || !Platform(r) {
			continue
		}
		var crd struct {
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Kind string `json:"kind"`
				} `json:"names"`
			} `json:"spec"`
		}
		if json.Unmarshal([]byte(r.ManifestJson), &crd) == nil && crd.Spec.Names.Kind != "" {
			out[schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}] = true
		}
	}
	return out
}

// resourceGroupKind is r's API group and kind.
func resourceGroupKind(r *snapshotv1alpha1.Resource) schema.GroupKind {
	return schema.GroupKind{Group: kube.Group(r.ApiVersion), Kind: r.Kind}
}

// writePlatformSet writes the platform set at dir (platform/<cloud>/<leaf>):
// a kustomization of the generic set's directories plus the objects only this
// platform serves, and its README. It lists the generic directories rather
// than the cluster's directory: that contains this one, which Kustomize
// rejects as a cycle.
func writePlatformSet(out *outDir, dir string, cls *platform.Classification, generic, buckets []string, objects int) error {
	up := strings.Repeat("../", strings.Count(dir, "/")+1)
	var resources []string
	for _, b := range generic {
		resources = append(resources, up+b)
	}
	resources = append(resources, sortedCopy(buckets)...)
	if err := out.writeKustomization(dir, resources); err != nil {
		return err
	}
	return out.write(filepath.Join(dir, "README.md"), []byte(platformReadme(dir, cls, objects)))
}

// platformReadme explains a platform set.
func platformReadme(dir string, cls *platform.Classification, objects int) string {
	name := cls.Distribution.Name()
	if name == "" || strings.HasSuffix(dir, "/vm") {
		name = cls.Infrastructure.Name()
		if cls.Distribution != platform.Unknown {
			name = cls.Distribution.Name() + " on " + name
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# The %s set\n\n", name)
	fmt.Fprintf(&b, "The source cluster ran on %s. This directory is the export for a cluster\n"+
		"on the same platform: kustomization.yaml includes the generic set (the\n"+
		"cluster's directory, which applies to any cluster)", cls.Summary())
	if objects > 0 {
		fmt.Fprintf(&b, " and %s that only\n%s serves: custom resources of APIs the platform provides,\n"+
			"which another cluster would reject.\n\n", text.Plural(objects, "object", "objects"), name)
	} else {
		b.WriteString(". This\nexport has no objects that only this platform serves, so the two are the\nsame.\n\n")
	}
	fmt.Fprintf(&b, "On %s, point Argo CD or Flux at this directory, or apply it:\n\n"+
		"    kubectl apply --server-side -k %s\n\n"+
		"Anywhere else, use the cluster's directory.\n", name, dir)
	if strings.HasPrefix(dir, awsDir) {
		fmt.Fprintf(&b, "\nRESTORE.md here and in %s/ has the steps for restoring onto\n"+
			"%s that are not Kubernetes objects; the cluster's RESTORE.md says when.\n", filepath.ToSlash(filepath.Dir(dir)), name)
	}
	fmt.Fprintf(&b, "\nHow SetDrift identified the platform: %s.\n", evidence(cls))
	return b.String()
}

// evidence lists what the classification rests on.
func evidence(cls *platform.Classification) string {
	var parts []string
	for _, e := range cls.Evidence {
		parts = append(parts, e.Detail)
	}
	if len(parts) == 0 {
		return "no evidence recorded"
	}
	return strings.Join(parts, "; ")
}
