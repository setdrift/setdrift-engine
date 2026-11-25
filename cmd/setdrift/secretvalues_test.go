// checks end to end that no secret value survives any command, whatever form the value takes in the cluster

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// every value below is a secret, each is unique so a leak can be traced to its source
var secretValues = []string{
	"VALopaqueData7f3a",       // an Opaque Secret's data
	"VALstringData91c2",       // an Opaque Secret's stringData
	"VALlastApplied55d1",      // a copy kubectl apply left in last-applied-configuration
	"VALregistryHost.example", // the registry an image pull secret is for
	"VALregistryAuth0e1f",     // an image pull secret's credentials
	"VALdockercfgAuth6a2b",    // the same in the older .dockercfg form
	"VALsaToken0b77",          // a service account token
	"VALhelmConfig3e8a",       // a value in a Helm release's config
	"VALhelmManifestC4d0",     // a value in a Helm release's rendered manifest
}

// check that no secret value survives an export, its files, a translation's files, or what export, diff, snapshot json and translate print, in any form a cluster holds it
// each value is looked for plain and base64-encoded once and twice
func TestNoSecretValueSurvives(t *testing.T) {
	in, out := t.TempDir(), t.TempDir()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	v := secretValues

	// a helm release record is base64 of base64 of gzipped json, holding the chart, its config and manifest
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"name":"web","namespace":"shop","version":1,"info":{"status":"deployed"},` +
		`"chart":{"metadata":{"name":"web","version":"1.0.0"}},` +
		`"config":{"password":"` + v[7] + `"},"manifest":"kind: Secret\nstringData: {token: ` + v[8] + `}"}`))
	_ = zw.Close()
	release := b64(b64(gz.String()))

	manifests := `apiVersion: v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: v1
kind: Secret
metadata: {name: opaque, namespace: shop}
type: Opaque
data: {password: ` + b64(v[0]) + `}
stringData: {token: ` + v[1] + `}
---
apiVersion: v1
kind: Secret
metadata:
  name: applied
  namespace: shop
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"apiVersion":"v1","kind":"Secret","stringData":{"key":"` + v[2] + `"}}'
type: Opaque
data: {key: ` + b64(v[2]) + `}
---
apiVersion: v1
kind: Secret
metadata: {name: pull, namespace: shop}
type: kubernetes.io/dockerconfigjson
data: {.dockerconfigjson: ` + b64(`{"auths":{"`+v[3]+`":{"auth":"`+b64("ci:"+v[4])+`","password":"`+v[4]+`"}}}`) + `}
---
apiVersion: v1
kind: Secret
metadata: {name: pull-old, namespace: shop}
type: kubernetes.io/dockercfg
data: {.dockercfg: ` + b64(`{"`+v[3]+`":{"auth":"`+b64("ci:"+v[5])+`"}}`) + `}
---
apiVersion: v1
kind: Secret
metadata:
  name: sa-token
  namespace: shop
  annotations: {kubernetes.io/service-account.name: default}
type: kubernetes.io/service-account-token
data: {token: ` + b64(v[6]) + `}
---
apiVersion: v1
kind: Secret
metadata:
  name: sh.helm.release.v1.web.v1
  namespace: shop
  labels: {owner: helm, name: web, status: deployed, version: "1"}
type: helm.sh/release.v1
data: {release: ` + release + `}
`
	if err := os.WriteFile(filepath.Join(in, "secrets.yaml"), []byte(manifests), 0o600); err != nil {
		t.Fatal(err)
	}

	// every form a value could take in what is written or printed
	var forms []string
	for _, s := range v {
		forms = append(forms, s, b64(s), b64(b64(s)))
	}
	forms = append(forms, release)
	check := func(where, text string) {
		t.Helper()
		for _, f := range forms {
			if strings.Contains(text, f) {
				t.Errorf("%s holds a secret value (%q)", where, f)
			}
		}
	}

	stdout, stderr, err := runCmd(t, "export", "--from-dir", in, "--cluster-name", "leak", "-o", out)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, stderr)
	}
	check("export's output", stdout+stderr)

	// every file the export wrote, the repository, redacted/, external-secrets/, RESTORE.md and the snapshots
	files := 0
	err = filepath.WalkDir(out, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 -- a file the test's own export wrote
		if err != nil {
			return err
		}
		files++
		check(path, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 5 {
		t.Fatalf("only %d files written: the export did not run as expected", files)
	}

	// what the other commands print from the export, snapshot json, and diff against the same input
	snaps, _ := filepath.Glob(filepath.Join(out, "snapshots", "*.pb"))
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %v", snaps)
	}
	stdout, stderr, err = runCmd(t, "snapshot", "json", snaps[0])
	if err != nil {
		t.Fatalf("snapshot json: %v\n%s", err, stderr)
	}
	check("snapshot json", stdout+stderr)
	stdout, stderr, _ = runCmd(t, "diff", snaps[0], in) // differences or none, only what it prints matters
	check("diff", stdout+stderr)

	// translate, what it prints and every file of the overlay it writes, its README.md, RESTORE.md and decisions
	stdout, stderr, err = runCmd(t, "translate", out, "--to", "aks", "--unmatched", "keep-source")
	if err != nil {
		t.Fatalf("translate: %v\n%s", err, stderr)
	}
	check("translate's output", stdout+stderr)
	overlays, _ := filepath.Glob(filepath.Join(out, "clusters", "leak", "translations", "*"))
	if len(overlays) != 1 {
		t.Fatalf("translations: %v", overlays)
	}
	err = filepath.WalkDir(overlays[0], func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 -- a file the test's own translate wrote
		if err != nil {
			return err
		}
		check(path, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// the export still knows each Secret by its keys, which are fine to keep
	b, err := os.ReadFile(filepath.Join(out, "clusters", "leak", "redacted", "namespaces", "shop", "secret", "opaque.yaml.redacted"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "password:") || !strings.Contains(string(b), "token:") {
		t.Errorf("the redacted Secret lost its keys:\n%s", b)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
