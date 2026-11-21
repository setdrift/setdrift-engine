// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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

// ExportInfoFile records which cluster an export came from, at the cluster
// directory's root, for setdrift verify. It has no .yaml extension, so
// kubectl apply -R, Argo CD and Flux, which read .yaml, .yml and .json, skip it.
const ExportInfoFile = ".setdrift-export"

// ExportInfo is what ExportInfoFile holds.
type ExportInfo struct {
	// Cluster is the name the export was written under
	Cluster string `json:"cluster"`
	// ProviderID is the cloud provider's ID for the cluster, e.g. the EKS ARN, if known
	ProviderID string `json:"providerId,omitempty"`
	// KubeSystemUID is the kube-system Namespace's UID: every cluster has one, fixed for the cluster's life, so it
	// tells clusters apart even when they share a name. Empty when the export did not read it (e.g. with -n).
	KubeSystemUID     string `json:"kubeSystemUID,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	ExportedAt        string `json:"exportedAt,omitempty"`
	// PlatformDir is the platform set, e.g. platform/aws/eks, empty when the platform was not identified
	PlatformDir     string `json:"platformDir,omitempty"`
	SetDriftVersion string `json:"setdriftVersion,omitempty"`
	// SecretManager is what the ExternalSecrets read from, as --external-secrets names it
	// (aws-secrets-manager, aws-parameter-store, generic), for setdrift secrets export
	SecretManager string `json:"secretManager,omitempty"`
	// Region is the AWS region, when known
	Region string `json:"region,omitempty"`
}

const exportInfoHeader = "# Written by setdrift export and read by setdrift verify: the cluster this\n" +
	"# export came from. Not a Kubernetes manifest.\n"

// exportInfo builds the ExportInfo for s.
func exportInfo(s *snapshotv1alpha1.Snapshot) ExportInfo {
	info := ExportInfo{
		Cluster:           s.GetCluster().GetName(),
		ProviderID:        s.GetCluster().GetProviderId(),
		KubernetesVersion: s.GetCluster().GetKubernetesVersion(),
		SetDriftVersion:   s.GetProducer().GetVersion(),
	}
	if t := s.GetCapturedAt(); t != nil {
		info.ExportedAt = t.AsTime().UTC().Format(time.RFC3339)
	}
	_, info.PlatformDir = exportPlatform(s)
	for _, r := range s.GetResources() {
		if r.ApiVersion == "v1" && r.Kind == "Namespace" && r.Name == "kube-system" {
			info.KubeSystemUID = r.SourceUid
		}
	}
	return info
}

// writeExportInfo writes ExportInfoFile into root.
// secretManager is the resolved --external-secrets, or "".
func writeExportInfo(root string, s *snapshotv1alpha1.Snapshot, secretManager string) error {
	info := exportInfo(s)
	info.SecretManager = secretManager
	if r, ok := regionOf(s); ok {
		info.Region = r
	}
	out, err := yaml.Marshal(info)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(root, ExportInfoFile), append([]byte(exportInfoHeader), out...))
}

// ReadExportInfo reads ExportInfoFile from a cluster's directory. It returns nil and no error when there is none
// (an export from an earlier version).
func ReadExportInfo(root string) (*ExportInfo, error) {
	b, err := os.ReadFile(filepath.Join(root, ExportInfoFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info ExportInfo
	if err := yaml.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("%s: %w", ExportInfoFile, err)
	}
	return &info, nil
}
