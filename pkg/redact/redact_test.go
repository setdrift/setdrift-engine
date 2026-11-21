// tests the redaction of core Secrets, that no value survives in any form and the placeholders keep the shape the API server needs

package redact_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
)

// check that a Secret's value is gone from data, stringData and the last-applied annotation, in plain text and in base64
func TestSecretValuesNeverSurvive(t *testing.T) {
	const secret = "hunter2"
	obj := resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": "db",
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"stringData":{"password":"` + secret + `"}}`,
			},
		},
		"data":       map[string]any{"password": "aHVudGVyMg=="},
		"stringData": map[string]any{"token": secret},
	}})

	out, _, err := redact.SecretRedactor{}.Process(context.Background(), []*resource.Object{obj})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out[0].Object)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{secret, "aHVudGVyMg=="} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("redacted object still contains %q: %s", leaked, b)
		}
	}
	if got := len(out[0].Redactions); got != 3 {
		t.Errorf("got %d redaction records, want 3", got)
	}
}

// check that a ConfigMap value that isnt a secret is left as it was
func TestNonSecretDataKept(t *testing.T) {
	obj := resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "cm"},
		"data":       map[string]any{"LOG_LEVEL": "info"},
	}})
	out, _, err := redact.SecretRedactor{}.Process(context.Background(), []*resource.Object{obj})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := unstructured.NestedString(out[0].Object, "data", "LOG_LEVEL"); v != "info" {
		t.Errorf("ConfigMap data changed to %q", v)
	}
}

// secretObj builds a Secret of the given type with the given data, for the tests below
func secretObj(typ string, data map[string]any) *resource.Object {
	return resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       typ,
		"metadata":   map[string]any{"name": "s", "namespace": "shop"},
		"data":       data,
	}})
}

// check that image pull secrets get a valid JSON placeholder, as the API server requires, keeping nothing of the value, not even the registry hosts
func TestImagePullSecretPlaceholders(t *testing.T) {
	const creds = `"username":"ci","password":"hunter2","auth":"Y2k6aHVudGVyMg=="`
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		typ, key, value, want string
	}{
		{"kubernetes.io/dockerconfigjson", ".dockerconfigjson",
			`{"auths":{"111122223333.dkr.ecr.us-east-1.amazonaws.com":{` + creds + `},"ghcr.io":{` + creds + `}}}`,
			redact.DockerConfigJSONPlaceholder},
		{"kubernetes.io/dockercfg", ".dockercfg",
			`{"ghcr.io":{` + creds + `}}`,
			redact.DockerCfgPlaceholder},
		{"kubernetes.io/dockerconfigjson", ".dockerconfigjson", "not json", redact.DockerConfigJSONPlaceholder},
	} {
		o := secretObj(tc.typ, map[string]any{tc.key: enc(tc.value)})
		if _, _, err := (redact.SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
			t.Fatal(err)
		}
		got, err := base64.StdEncoding.DecodeString(o.Object["data"].(map[string]any)[tc.key].(string))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %s, want %s", tc.typ, got, tc.want)
		}
		if strings.Contains(string(got), "ghcr.io") || strings.Contains(string(got), "dkr.ecr") {
			t.Errorf("%s: the placeholder keeps a registry host: %s", tc.typ, got)
		}
		if !json.Valid(got) {
			t.Errorf("%s: placeholder is not valid JSON: %s", tc.typ, got)
		}
		if len(o.Redactions) != 1 {
			t.Errorf("%s: %d redactions, want 1", tc.typ, len(o.Redactions))
		}
	}
}

// check that a service account token's data is removed rather than redacted, since the token controller writes all of it, so the Secret stays applicable
func TestServiceAccountTokenData(t *testing.T) {
	o := secretObj("kubernetes.io/service-account-token", map[string]any{"token": "ZXlKaGJHY2lPaUpT", "ca.crt": "LS0t"})
	if _, _, err := (redact.SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Object["data"]; ok || len(o.Redactions) != 0 {
		t.Errorf("data %v, redactions %v: want no data and no redactions", o.Object["data"], o.Redactions)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
