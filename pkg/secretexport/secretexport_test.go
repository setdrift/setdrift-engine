// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

type fakeProvider struct {
	existing map[string]bool
	written  map[string]string
	fail     string
}

func (*fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Exists(_ context.Context, key string) (bool, error) {
	return f.existing[key], nil
}
func (f *fakeProvider) Put(_ context.Context, key, value string, _ bool) error {
	if key == f.fail {
		return fmt.Errorf("denied")
	}
	f.written[key] = value
	return nil
}

type fakeSecrets map[string]map[string][]byte

func (f fakeSecrets) Get(_ context.Context, ns, name string) (map[string][]byte, error) {
	d, ok := f[ns+"/"+name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return d, nil
}

// exportWith writes an export with an ExternalSecret for each redacted Secret.
func exportWith(t *testing.T, secrets map[string][]string) string {
	t.Helper()
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"}}
	for ref, keys := range secrets {
		ns, name, _ := strings.Cut(ref, "/")
		data := map[string]string{}
		var reds []*snapshotv1alpha1.Redaction
		for _, k := range keys {
			data[k] = "U0VURFJJRlRfUkVEQUNURUQ="
			reds = append(reds, &snapshotv1alpha1.Redaction{Path: "data." + k, Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE})
		}
		m, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": ns}, "data": data})
		s.Resources = append(s.Resources, &snapshotv1alpha1.Resource{ApiVersion: "v1", Kind: "Secret", Namespace: ns, Name: name, ManifestJson: string(m), Redactions: reds})
	}
	es, _ := kustomize.DefaultExternalSecrets("generic")
	dir := t.TempDir()
	if err := (kustomize.Emitter{ExternalSecrets: es}).Emit(s, dir); err != nil {
		t.Fatal(err)
	}
	return kustomize.ClusterDir(dir, "prod")
}

func TestPlanAndApply(t *testing.T) {
	root := exportWith(t, map[string][]string{
		"shop/db":       {"password", "username"},
		"shop/keystore": {"store.p12"},
		"shop/old":      {"token"},
		"shop/gone":     {"k"},
		"shop/partial":  {"a", "b"},
		"shop/denied":   {"k"},
	})
	remotes, err := Load(root)
	if err != nil || len(remotes) != 6 {
		t.Fatalf("%d remotes, %v", len(remotes), err)
	}
	src := fakeSecrets{
		"shop/db":       {"password": []byte("hunter2"), "username": []byte("app")},
		"shop/keystore": {"store.p12": {0xff, 0xfe, 0x00, 0x01}},
		"shop/old":      {"token": []byte("t")},
		"shop/partial":  {"a": []byte("x")},
		"shop/denied":   {"k": []byte("v")},
	}
	prov := &fakeProvider{existing: map[string]bool{"prod/shop/old": true}, written: map[string]string{}, fail: "prod/shop/denied"}
	plan, err := MakePlan(context.Background(), remotes, src, prov, false)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]Status{}
	for _, it := range plan.Items {
		status[it.Secret] = it.Status
	}
	want := map[string]Status{"db": Create, "keystore": Create, "old": Skip, "gone": Missing, "partial": Missing, "denied": Create}
	for k, v := range want {
		if status[k] != v {
			t.Errorf("%s: %q, want %q", k, status[k], v)
		}
	}
	if plan.Writes() != 3 {
		t.Errorf("writes %d", plan.Writes())
	}
	// Nothing about the plan prints a value.
	if b, _ := json.Marshal(plan); strings.Contains(string(b), "hunter2") {
		t.Error("plan holds a value where it can be printed")
	}
	if err := plan.Apply(context.Background(), root, prov); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("apply error %v", err)
	}
	if prov.written["prod/shop/db"] != `{"password":"hunter2","username":"app"}` {
		t.Errorf("db written %q", prov.written["prod/shop/db"])
	}
	if prov.written["prod/shop/keystore"] != `{"store.p12":"//4AAQ=="}` {
		t.Errorf("keystore written %q", prov.written["prod/shop/keystore"])
	}
	if _, ok := prov.written["prod/shop/old"]; ok {
		t.Error("existing remote secret overwritten")
	}
	es, _ := os.ReadFile(filepath.Join(root, "external-secrets", "namespaces", "shop", "externalsecret.external-secrets.io", "keystore.yaml"))
	if !strings.Contains(string(es), "decodingStrategy: Base64") {
		t.Errorf("binary key not marked:\n%s", es)
	}
	// No value reached a file.
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
		if b, err := os.ReadFile(p); err == nil && !d.IsDir() && strings.Contains(string(b), "hunter2") {
			t.Errorf("%s holds a value", p)
		}
		return nil
	})
}

func TestOverwrite(t *testing.T) {
	root := exportWith(t, map[string][]string{"shop/old": {"token"}})
	remotes, _ := Load(root)
	prov := &fakeProvider{existing: map[string]bool{"prod/shop/old": true}, written: map[string]string{}}
	plan, err := MakePlan(context.Background(), remotes, fakeSecrets{"shop/old": {"token": []byte("t2")}}, prov, true)
	if err != nil || plan.Items[0].Status != Overwrite {
		t.Fatalf("%v %v", err, plan.Items[0].Status)
	}
	if err := plan.Apply(context.Background(), root, prov); err != nil || prov.written["prod/shop/old"] != `{"token":"t2"}` {
		t.Errorf("%v %q", err, prov.written["prod/shop/old"])
	}
}

func TestLoadWithoutExternalSecrets(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "--external-secrets") {
		t.Errorf("got %v", err)
	}
}
