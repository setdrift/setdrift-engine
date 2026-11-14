// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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

func secretObj(typ string, data map[string]any) *resource.Object {
	return resource.New(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       typ,
		"metadata":   map[string]any{"name": "s", "namespace": "shop"},
		"data":       data,
	}})
}

// TestImagePullSecretPlaceholders: the API server requires valid JSON in
// image pull secrets. The placeholder keeps the registry hosts and nothing
// else.
func TestImagePullSecretPlaceholders(t *testing.T) {
	const creds = `"username":"ci","password":"hunter2","auth":"Y2k6aHVudGVyMg=="`
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		typ, key, value, want string
	}{
		{"kubernetes.io/dockerconfigjson", ".dockerconfigjson",
			`{"auths":{"111122223333.dkr.ecr.us-east-1.amazonaws.com":{` + creds + `},"ghcr.io":{` + creds + `}}}`,
			`{"auths":{"111122223333.dkr.ecr.us-east-1.amazonaws.com":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="},"ghcr.io":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}}`},
		{"kubernetes.io/dockercfg", ".dockercfg",
			`{"ghcr.io":{` + creds + `}}`,
			`{"ghcr.io":{"auth":"U0VURFJJRlRfUkVEQUNURUQ="}}`},
		{"kubernetes.io/dockerconfigjson", ".dockerconfigjson", "not json", `{"auths":{}}`},
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
		if !json.Valid(got) {
			t.Errorf("%s: placeholder is not valid JSON: %s", tc.typ, got)
		}
		if len(o.Redactions) != 1 {
			t.Errorf("%s: %d redactions, want 1", tc.typ, len(o.Redactions))
		}
	}
}

// TestServiceAccountTokenData: the token controller writes all of the data,
// so it is removed rather than redacted, and the Secret stays applicable.
func TestServiceAccountTokenData(t *testing.T) {
	o := secretObj("kubernetes.io/service-account-token", map[string]any{"token": "ZXlKaGJHY2lPaUpT", "ca.crt": "LS0t"})
	if _, _, err := (redact.SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Object["data"]; ok || len(o.Redactions) != 0 {
		t.Errorf("data %v, redactions %v: want no data and no redactions", o.Object["data"], o.Redactions)
	}
}
