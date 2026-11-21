// tests the detection of secret values outside Secrets, in env vars, ConfigMaps, custom resources and documents that arent Kubernetes objects

package redact_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
)

// process parses a JSON manifest and runs it through the redactor, giving back the object
func process(t *testing.T, manifest string) *resource.Object {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(manifest), &u.Object); err != nil {
		t.Fatal(err)
	}
	o := resource.New(u)
	if _, _, err := (redact.SecretRedactor{}).Process(context.Background(), []*resource.Object{o}); err != nil {
		t.Fatal(err)
	}
	return o
}

// paths lists the paths of an object's redactions, in the order they were recorded
func paths(o *resource.Object) []string {
	var out []string
	for _, r := range o.Redactions {
		out = append(out, r.Path)
	}
	return out
}

// check that secret env values are redacted by pattern or name, while references, files, ttls and secret names are kept
func TestEmbeddedSecretsRedacted(t *testing.T) {
	o := process(t, `{
	  "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "api", "namespace": "shop"},
	  "spec": {"template": {"spec": {"containers": [{"name": "api", "image": "app:1", "env": [
	    {"name": "DB_PASSWORD", "value": "hunter2hunter2"},
	    {"name": "stripeApiKey", "value": "sk_live_abcdef"},
	    {"name": "DATABASE_URL", "value": "postgres://app:hunter2@db:5432/app"},
	    {"name": "LOG_LEVEL", "value": "info"},
	    {"name": "TOKEN_TTL", "value": "3600"},
	    {"name": "DB_PASSWORD_FILE", "value": "/run/secrets/db"},
	    {"name": "API_TOKEN", "value": "$(TOKEN_FROM_SECRET)"},
	    {"name": "SECRET_NAME", "value": "db-creds"},
	    {"name": "REDIS_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "redis", "key": "password"}}}
	  ]}]}}}}`)
	want := []string{
		"spec.template.spec.containers[api].env[DB_PASSWORD].value",
		"spec.template.spec.containers[api].env[stripeApiKey].value",
		"spec.template.spec.containers[api].env[DATABASE_URL].value",
	}
	if got := paths(o); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("redacted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	b, _ := json.Marshal(o.Object)
	for _, leak := range []string{"hunter2", "sk_live_abcdef"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%q survived: %s", leak, b)
		}
	}
	for _, kept := range []string{"info", "3600", "/run/secrets/db", "$(TOKEN_FROM_SECRET)", "db-creds"} {
		if !strings.Contains(string(b), kept) {
			t.Errorf("%q was redacted but is not a secret", kept)
		}
	}
	if len(o.Findings) != 1 || o.Findings[0].Code != "SECRET_IN_MANIFEST" {
		t.Errorf("findings %v", o.Findings)
	}
}

// check that secret values in ConfigMap keys and config files and in custom resource fields are redacted, and certificates and references are not
func TestEmbeddedSecretsInConfigAndCustomResources(t *testing.T) {
	cm := process(t, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "app"},
	  "data": {
	    "application.properties": "db.url=jdbc:postgresql://db/app\ndb.password=hunter2hunter2\n",
	    "settings.yaml": "password: ${DB_PASSWORD}\nlevel: info\n",
	    "ca.crt": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
	    "key.pem": "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
	    "aws": "AKIAIOSFODNN7EXAMPLE",
	    "apiKey": "abcd1234efgh"
	  }}`)
	want := "data.aws data.apiKey data.application.properties data.key.pem"
	if got := strings.Join(paths(cm), " "); got != "data.apiKey data.application.properties data.aws data.key.pem" {
		t.Errorf("ConfigMap redacted %q, want the four secret keys (%s)", got, want)
	}

	cr := process(t, `{"apiVersion": "db.example.com/v1", "kind": "Database", "metadata": {"name": "orders"},
	  "spec": {"adminPassword": "s3cretValue", "passwordSecretRef": {"name": "db"}, "tokenSecretName": "x",
	    "connection": {"clientSecret": "abcdefgh", "clientId": "app"}}}`)
	if got := strings.Join(paths(cr), " "); got != "spec.adminPassword spec.connection.clientSecret" {
		t.Errorf("custom resource redacted %q", got)
	}
}

// check that SealedSecrets and ExternalSecrets are not scanned, their content is ciphertext or references
func TestSafeKindsNotScanned(t *testing.T) {
	sealed := process(t, `{"apiVersion": "bitnami.com/v1alpha1", "kind": "SealedSecret", "metadata": {"name": "db"},
	  "spec": {"encryptedData": {"password": "AgBy3i4OJSWK+PiTySYZZA=="}}}`)
	es := process(t, `{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "metadata": {"name": "db"},
	  "spec": {"data": [{"secretKey": "password", "remoteRef": {"key": "prod/db", "property": "password"}}]}}`)
	for _, o := range []*resource.Object{sealed, es} {
		if len(o.Redactions) != 0 {
			t.Errorf("%s redacted: %v", o.GetKind(), paths(o))
		}
	}
}

// check that Values uses the manifest detection on a document that isnt an object and gives back the paths it replaced, without a leading dot
func TestValues(t *testing.T) {
	v := map[string]any{
		"agent":    map[string]any{"apiKey": "0123456789abcdef0123456789abcdef", "site": "eu"},
		"replicas": float64(2),
	}
	got, paths := redact.Values(v)
	m := got.(map[string]any)["agent"].(map[string]any)
	if m["apiKey"] != redact.Placeholder || m["site"] != "eu" || len(paths) != 1 || paths[0] != "agent.apiKey" {
		t.Errorf("%v %v", got, paths)
	}
}

// check that FieldValue hides a live field's value by pattern, by the field's name or by its list item's name, and leaves its input unchanged
func TestFieldValue(t *testing.T) {
	for _, c := range []struct {
		path string
		v    any
		want any
	}{
		{"data.password", "hunter22", redact.Placeholder},
		{"spec.template.spec.containers[web].env[DB_TOKEN].value", "s3cr3tvalue", redact.Placeholder},
		{"data.config", "url: postgres://app:hunter22@db/app", redact.Placeholder},
		{"spec.replicas", float64(3), float64(3)},
		{"data.mode", "debug", "debug"},
		{"data.password", "true", "true"},
	} {
		if got := redact.FieldValue(c.path, c.v); got != c.want {
			t.Errorf("redact.FieldValue(%s, %v) = %v, want %v", c.path, c.v, got, c.want)
		}
	}
	in := map[string]any{"apiKey": "abcd1234efgh", "region": "eu"}
	out, ok := redact.FieldValue("spec.config", in).(map[string]any)
	if !ok || out["apiKey"] != redact.Placeholder || out["region"] != "eu" {
		t.Errorf("a map: %v", out)
	}
	if in["apiKey"] != "abcd1234efgh" {
		t.Error("FieldValue changed its input")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
