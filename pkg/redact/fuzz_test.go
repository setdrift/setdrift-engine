package redact

import (
	"encoding/base64"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// A value the patterns match never survives Values, wherever it is.
func FuzzValues(f *testing.F) {
	for _, s := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_0123456789abcdefghijklmnopqrstuvwxyz", "plain", "-----BEGIN RSA PRIVATE KEY-----\nx", "postgres://u:p@h/db"} {
		f.Add(s, "key")
	}
	f.Fuzz(func(t *testing.T, s, key string) {
		in := map[string]any{key: s, "nested": map[string]any{"list": []any{s, map[string]any{key: s}}}}
		out, _ := Values(in)
		if what := matchValue(s); what != "" && holds(out, s) {
			t.Fatalf("%q matches %s and survived Values: %v", s, what, out)
		}
	})
}

// holds reports whether v holds the string s anywhere.
func holds(v any, s string) bool {
	switch v := v.(type) {
	case string:
		return v == s
	case map[string]any:
		for _, e := range v {
			if holds(e, s) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if holds(e, s) {
				return true
			}
		}
	}
	return false
}

// helmRelease decodes base64, gzip and JSON from a Secret anyone with write
// access to a namespace can create: it may give up, never panic.
func FuzzHelmRelease(f *testing.F) {
	for _, s := range []string{"", "{}", "\x1f\x8b\x08\x00", "{\"name\":\"x\",\"chart\":{\"metadata\":{\"name\":\"c\"}}}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		enc := base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(raw)))
		helmRelease(resource.New(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "helm.sh/release.v1",
			"metadata": map[string]any{"name": "sh.helm.release.v1.x.v1", "namespace": "ns"},
			"data":     map[string]any{"release": enc},
		}}))
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
