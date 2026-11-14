// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

const helmReleaseType = "helm.sh/release.v1"

// maxRelease caps how much of a release record is decompressed: records
// are well under Kubernetes' 1 MiB object limit compressed.
const maxRelease = 32 << 20

// helmRelease reads the chart metadata from a Helm release record:
// data.release is base64 (the Secret encoding) of base64 (Helm's) of a
// gzipped JSON release. It returns nil if the record cannot be read. The
// release's values (config) and rendered manifest are never kept.
func helmRelease(o *resource.Object) *snapshotv1alpha1.HelmRelease {
	data, _ := o.Object["data"].(map[string]any)
	enc, _ := data["release"].(string)
	inner, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		return nil
	}
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil
		}
		if raw, err = io.ReadAll(io.LimitReader(zr, maxRelease)); err != nil {
			return nil
		}
	}
	var rel struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int32  `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name       string   `json:"name"`
				Version    string   `json:"version"`
				AppVersion string   `json:"appVersion"`
				Home       string   `json:"home"`
				Sources    []string `json:"sources"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if json.Unmarshal(raw, &rel) != nil || rel.Name == "" {
		return nil
	}
	if rel.Info.Status != "deployed" {
		return nil // superseded or failed revisions
	}
	m := rel.Chart.Metadata
	return &snapshotv1alpha1.HelmRelease{
		Namespace: rel.Namespace, Name: rel.Name, Revision: rel.Version,
		Chart: m.Name, ChartVersion: m.Version, AppVersion: m.AppVersion, Home: m.Home, Sources: m.Sources,
	}
}
