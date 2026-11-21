// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeProvider struct {
	existing map[string]string // key -> stored JSON
	written  map[string]string
	fail     string
}

func (*fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Exists(_ context.Context, key string) (bool, error) {
	_, ok := f.existing[key]
	return ok, nil
}
func (f *fakeProvider) Put(_ context.Context, key, value string, _ bool) error {
	if key == f.fail {
		return fmt.Errorf("denied")
	}
	f.written[key] = value
	f.existing[key] = value
	return nil
}
func (f *fakeProvider) Get(_ context.Context, key string) (string, bool, error) {
	v, ok := f.existing[key]
	return v, ok, nil
}

type fakeSecrets map[string]map[string][]byte

func (f fakeSecrets) Get(_ context.Context, ns, name string) (map[string][]byte, error) {
	d, ok := f[ns+"/"+name]
	if !ok {
		return nil, ErrNoSecret
	}
	return d, nil
}

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const redactedSecret = "apiVersion: v1\nkind: Secret\nmetadata: {name: %s, namespace: %s}\ntype: Opaque\ndata: {%s}\n"

func secretFile(ns, name string, keys ...string) string {
	var data []string
	for _, k := range keys {
		data = append(data, k+": U0VURFJJRlRfUkVEQUNURUQ=")
	}
	return fmt.Sprintf(redactedSecret, name, ns, strings.Join(data, ", "))
}

func TestLoadRedacted(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"namespaces/shop/secret/db.yaml.redacted":              secretFile("shop", "db", "password", "username"),
		"namespaces/argocd/secret/argocd-secret.yaml.redacted": "apiVersion: v1\nkind: Secret\nmetadata: {name: argocd-secret, namespace: argocd}\n",
		"namespaces/shop/deployment.apps/api.yaml.redacted":    "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api, namespace: shop}\n",
		"namespaces/shop/secret/broken.yaml.redacted":          "kind: [",
		"README.md": "not read",
	})
	f, err := LoadRedacted(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Secrets) != 1 || f.Secrets[0].Name != "db" || strings.Join(f.Secrets[0].Keys, ",") != "password,username" {
		t.Errorf("secrets = %+v (a Secret with only generated keys has nothing to copy)", f.Secrets)
	}
	if len(f.Others) != 1 || !strings.Contains(f.Others[0], "Deployment shop/api") {
		t.Errorf("others = %v", f.Others)
	}
	if len(f.Invalid) != 1 || !strings.Contains(f.Invalid[0], "broken.yaml.redacted") {
		t.Errorf("invalid = %v", f.Invalid)
	}
	if _, err := LoadRedacted(filepath.Join(dir, "README.md")); err == nil {
		t.Error("a file accepted as the folder")
	}
}

func TestRemoteKeys(t *testing.T) {
	secrets := []Source{{Namespace: "shop", Name: "db"}, {Namespace: "web", Name: "tls"}}
	keys, err := RemoteKeys(SecretsManagerID, "{{.Cluster}}/{{.Namespace}}/{{.Name}}", "prod", secrets)
	if err != nil || strings.Join(keys, " ") != "prod/shop/db prod/web/tls" {
		t.Errorf("%v %v", keys, err)
	}
	for _, c := range []struct {
		m       Manager
		tmpl    string
		wantErr string
	}{
		{SecretsManagerID, "{{.Cluster}}/{{.Name}}", "must include {{.Namespace}} and {{.Name}}"},
		{SecretsManagerID, "{{.Cluster}}/{{.Namespce}}/{{.Name}}", "unknown field .Namespce"},
		{SecretsManagerID, "{{.Cluster}}/{{.Name}}", "must include"},
		{SecretsManagerID, "{{.Cluster}} {{.Namespace}}/{{.Name}}", "Secrets Manager allows"},
		{ParameterStoreID, "{{.Cluster}}/{{.Namespace}}/{{.Name}}", "needs a leading /"},
		{ParameterStoreID, "/aws/{{.Namespace}}/{{.Name}}", "not under /aws"},
	} {
		if _, err := RemoteKeys(c.m, c.tmpl, "prod", secrets); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s %s: %v, want %q", c.m, c.tmpl, err, c.wantErr)
		}
	}
	if _, err := RemoteKeys(ParameterStoreID, "/{{.Cluster}}/{{.Namespace}}/{{.Name}}", "prod", secrets); err != nil {
		t.Errorf("parameter store: %v", err)
	}
}

// Plan, write, read back: a controller-written key makes the ExternalSecret
// merge, a binary value is stored base64, and the report compares what the
// secret manager holds with the Secret.
func TestPlanApplyVerify(t *testing.T) {
	secrets := []Source{{Namespace: "shop", Name: "db", Keys: []string{"password"}},
		{Namespace: "argocd", Name: "argocd-secret", Keys: []string{"oidc"}},
		{Namespace: "shop", Name: "keystore", Keys: []string{"jks"}},
		{Namespace: "shop", Name: "stale", Keys: []string{"token"}}}
	keys, err := RemoteKeys(SecretsManagerID, "prod/{{.Namespace}}/{{.Name}}", "prod", secrets)
	if err != nil {
		t.Fatal(err)
	}
	src := fakeSecrets{
		"shop/db":              {"password": []byte("hunter2")},
		"argocd/argocd-secret": {"oidc": []byte("x"), "server.secretkey": []byte("generated")},
		"shop/keystore":        {"jks": {0xff, 0xfe}},
		"shop/stale":           {"token": []byte("new")},
	}
	prov := &fakeProvider{existing: map[string]string{"prod/shop/stale": `{"token":"old"}`}, written: map[string]string{}}
	remotes, err := Remotes(secrets, keys)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MakePlan(context.Background(), remotes, src, prov, false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*Item{}
	for _, it := range plan.Items {
		by[it.Secret] = it
		// a plan printed in full, as a debug line or a test failure would
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if out := fmt.Sprintf(format, *it); strings.Contains(out, "hunter2") {
				t.Errorf("%s of an Item prints the secret value: %s", format, out)
			}
		}
	}
	if !by["argocd-secret"].Merge || by["db"].Merge {
		t.Error("merge: only a Secret with keys beyond the redacted ones")
	}
	if len(by["keystore"].Binary) != 1 || by["stale"].Status != Skip || plan.Writes() != 3 {
		t.Errorf("plan: %+v", plan.Items)
	}
	if err := plan.Apply(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	_ = json.Unmarshal([]byte(prov.written["prod/shop/db"]), &stored)
	if stored["password"] != "hunter2" {
		t.Errorf("stored %v", stored)
	}
	report := plan.Verify(context.Background(), prov)
	ok := map[string]Check{}
	for _, c := range report {
		ok[c.Secret] = c
	}
	if !ok["shop/db"].OK() || !ok["argocd/argocd-secret"].OK() || !ok["shop/keystore"].OK() {
		t.Errorf("report: %+v", report)
	}
	if c := ok["shop/stale"]; c.OK() || !c.Keys || c.Values || !strings.Contains(c.Note, "values differ") {
		t.Errorf("a skipped secret with old values: %+v", c)
	}
	// the report never carries a value
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "old") {
		t.Errorf("a value reached the report: %s", b)
	}
}

func TestPlanMissing(t *testing.T) {
	secrets := []Source{{Namespace: "shop", Name: "gone", Keys: []string{"k"}}, {Namespace: "shop", Name: "db", Keys: []string{"password", "user"}}}
	keys, err := RemoteKeys(SecretsManagerID, "{{.Namespace}}/{{.Name}}", "prod", secrets)
	if err != nil {
		t.Fatal(err)
	}
	remotes, err := Remotes(secrets, keys)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MakePlan(context.Background(), remotes, fakeSecrets{"shop/db": {"password": []byte("x")}},
		&fakeProvider{existing: map[string]string{}, written: map[string]string{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].Status != Missing || plan.Items[1].Status != Missing || !strings.Contains(plan.Items[1].Note, "no key user") {
		t.Errorf("%+v %+v", plan.Items[0], plan.Items[1])
	}
}
