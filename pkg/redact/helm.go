// reads the chart a Helm release record runs before the redactor replaces the record's data, only the chart metadata, never the release's values or rendered manifest

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

// helmReleaseType is the type of the Secrets Helm keeps its release records in
const helmReleaseType = "helm.sh/release.v1"

// maxRelease caps how much of a release record is decompressed, records are well under Kubernetes' 1 MiB object limit compressed
const maxRelease = 32 << 20

// helmRelease takes a Helm release Secret and reads the chart metadata from its record, so the restore playbook knows which chart to install
// data.release is base64 (the Secret encoding) of base64 (Helm's) of a gzipped JSON release
// it gives back nil when the record cant be read, the release's values (config) and rendered manifest are never kept
func helmRelease(o *resource.Object) *snapshotv1alpha1.HelmRelease {
	data, _ := o.Object["data"].(map[string]any)
	enc, _ := data["release"].(string)
	// undoing the Secret's base64
	inner, err := base64.StdEncoding.DecodeString(enc)
	// checking if it isnt base64, if so the record cant be read
	if err != nil {
		return nil
	}
	// undoing Helm's own base64
	raw, err := base64.StdEncoding.DecodeString(string(inner))
	// checking if it isnt base64 either, if so the record cant be read
	if err != nil {
		return nil
	}
	// checking if the bytes start with the gzip magic number, if so theyre decompressed, up to maxRelease
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		// checking if the gzip header is broken, if so the record cant be read
		if err != nil {
			return nil
		}
		// checking if decompressing fails, if so the record cant be read
		if raw, err = io.ReadAll(io.LimitReader(zr, maxRelease)); err != nil {
			return nil
		}
	}
	// rel holds only the parts of the release record that are kept, the values and manifest fields are never decoded
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
	// checking if the release doesnt parse or has no name, if so the record cant be read
	if json.Unmarshal(raw, &rel) != nil || rel.Name == "" {
		return nil
	}
	// checking if this revision is the deployed one, the others are superseded or failed and not kept
	if rel.Info.Status != "deployed" {
		return nil // superseded or failed revisions
	}
	m := rel.Chart.Metadata
	return &snapshotv1alpha1.HelmRelease{
		Namespace: rel.Namespace, Name: rel.Name, Revision: rel.Version,
		Chart: m.Name, ChartVersion: m.Version, AppVersion: m.AppVersion, Home: m.Home, Sources: m.Sources,
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
