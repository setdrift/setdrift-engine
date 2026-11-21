// records which cluster an export came from in .setdrift-export, so setdrift verify can tell if its checking the same cluster

package kustomize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"sigs.k8s.io/yaml"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// ExportInfoFile records which cluster an export came from, at the cluster directory's root, for setdrift verify
// it has no .yaml extension, so kubectl apply -R, argo cd and flux, which read .yaml, .yml and .json, skip it
const ExportInfoFile = ".setdrift-export"

// ExportInfo is what ExportInfoFile holds, the source cluster of an export and when it was taken
type ExportInfo struct {
	// Cluster is the name the export was written under
	Cluster string `json:"cluster"`
	// ProviderID is the cloud provider's id for the cluster, e.g. the eks arn, if known
	ProviderID string `json:"providerId,omitempty"`
	// KubeSystemUID is the kube-system Namespace's uid, every cluster has one, fixed for the cluster's life, so it tells clusters apart even when they share a name
	// its empty when the export didnt read it (e.g. with -n)
	KubeSystemUID string `json:"kubeSystemUID,omitempty"`
	// KubernetesVersion is the source cluster's kubernetes version
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// ExportedAt is when the snapshot was taken, in rfc 3339
	ExportedAt string `json:"exportedAt,omitempty"`
	// PlatformDir is the platform set, e.g. platform/aws/eks, empty when the platform was not identified
	PlatformDir string `json:"platformDir,omitempty"`
	// SetDriftVersion is the setdrift version that took the snapshot
	SetDriftVersion string `json:"setdriftVersion,omitempty"`
	// Region is the aws region, when known
	Region string `json:"region,omitempty"`
}

// exportInfoHeader goes at the top of ExportInfoFile, so a person opening it knows what its for
const exportInfoHeader = "# Written by setdrift export and read by setdrift verify: the cluster this\n" +
	"# export came from. Not a Kubernetes manifest.\n"

// exportInfo builds the ExportInfo for s
func exportInfo(s *snapshotv1alpha1.Snapshot) ExportInfo {
	info := ExportInfo{
		Cluster:           s.GetCluster().GetName(),
		ProviderID:        s.GetCluster().GetProviderId(),
		KubernetesVersion: s.GetCluster().GetKubernetesVersion(),
		SetDriftVersion:   s.GetProducer().GetVersion(),
	}
	// checking if the snapshot recorded when it was taken, if so its kept in utc
	if t := s.GetCapturedAt(); t != nil {
		info.ExportedAt = t.AsTime().UTC().Format(time.RFC3339)
	}
	// invoking exportPlatform for the platform set's directory, its defined in platformset.go
	_, info.PlatformDir = exportPlatform(s)
	// going through the resources to find the kube-system Namespace
	for _, r := range s.GetResources() {
		// checking if this is the kube-system Namespace, if so its uid is kept
		if r.ApiVersion == "v1" && r.Kind == "Namespace" && r.Name == "kube-system" {
			info.KubeSystemUID = r.SourceUid
		}
	}
	return info
}

// writeExportInfo writes ExportInfoFile into the export's directory
func writeExportInfo(out *outDir, s *snapshotv1alpha1.Snapshot) error {
	// invoking exportInfo to build what goes in the file, its defined above
	info := exportInfo(s)
	// invoking regionOf and checking if the snapshot shows the aws region, if so its kept, its defined in externalsecrets.go
	if r, ok := regionOf(s); ok {
		info.Region = r
	}
	b, err := yaml.Marshal(info)
	if err != nil {
		return err
	}
	// invoking out.write to write the header and the yaml, its defined in kustomize.go
	return out.write(ExportInfoFile, append([]byte(exportInfoHeader), b...))
}

// ReadExportInfo reads ExportInfoFile from a cluster's directory, for setdrift verify
// it gives back nil and no error when theres none, as in an export from an earlier version
func ReadExportInfo(root string) (*ExportInfo, error) {
	// reading the file from the cluster's directory
	b, err := os.ReadFile(filepath.Join(root, ExportInfoFile)) //#nosec G304 -- the export directory the user named
	// checking if the file doesnt exist, if so theres no info rather than an error
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info ExportInfo
	// decoding the file, a broken one is a terminal error naming the file
	if err := yaml.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("%s: %w", ExportInfoFile, err)
	}
	return &info, nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
