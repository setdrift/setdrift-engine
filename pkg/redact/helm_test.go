// tests reading the chart metadata from a Helm release record, without keeping its values or rendered manifest

package redact

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// releaseSecret builds a Helm release Secret with the given status, encoded the way Helm stores it, with a password in its values and a Secret in its manifest
func releaseSecret(t *testing.T, status string) *resource.Object {
	t.Helper()
	js := `{"name":"web","namespace":"shop","version":3,"info":{"status":"` + status + `"},
	  "chart":{"metadata":{"name":"nginx","version":"1.2.3","appVersion":"1.27","home":"https://example.com","sources":["https://github.com/example/charts"]}},
	  "config":{"password":"hunter2"},"manifest":"kind: Secret\ndata: {token: c2VjcmV0}"}`
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(js))
	_ = w.Close()
	helm := base64.StdEncoding.EncodeToString(gz.Bytes())
	return resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "helm.sh/release.v1",
		"metadata": map[string]any{"name": "sh.helm.release.v1.web.v3", "namespace": "shop"},
		"data":     map[string]any{"release": base64.StdEncoding.EncodeToString([]byte(helm))},
	}})
}

// check that a deployed release gives its chart metadata, keeps no values or manifest, its record is redacted, and a superseded revision is not read
func TestHelmReleaseMetadata(t *testing.T) {
	o := releaseSecret(t, "deployed")
	if _, _, err := (SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	h := o.HelmRelease
	if h == nil || h.Name != "web" || h.Namespace != "shop" || h.Revision != 3 || h.Chart != "nginx" || h.ChartVersion != "1.2.3" ||
		h.AppVersion != "1.27" || h.Home != "https://example.com" || len(h.Sources) != 1 {
		t.Fatalf("release %v", h)
	}
	if strings.Contains(h.String(), "hunter2") || strings.Contains(h.String(), "c2VjcmV0") {
		t.Error("values or manifest kept")
	}
	if d := o.Object["data"].(map[string]any)["release"]; d == "" || strings.Contains(d.(string), "H4sI") {
		t.Errorf("record not redacted: %v", d)
	}
	superseded := releaseSecret(t, "superseded")
	_, _, _ = (SecretRedactor{}).Process(context.Background(), []*resource.Object{superseded})
	if superseded.HelmRelease != nil {
		t.Error("superseded revision read")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
