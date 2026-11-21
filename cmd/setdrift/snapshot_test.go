// tests for setdrift snapshot json; decoding and the JSON format are tested
// in pkg/snapshot

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshot json on an exported .pb prints what --snapshot-json writes for
// the same export: valid JSON, with each manifest inlined as an object.
func TestSnapshotJSON(t *testing.T) {
	dir := t.TempDir()
	pb, js := filepath.Join(dir, "snap.pb"), filepath.Join(dir, "snap.json")
	root := newRootCmd()
	root.SetArgs([]string{"export", "--from-dir", corpusInput,
		"-o", filepath.Join(dir, "out"), "--snapshot", pb, "--snapshot-json", js})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root = newRootCmd()
	root.SetArgs([]string{"snapshot", "json", pb})
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(js)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Error("snapshot json output differs from --snapshot-json")
	}
	var doc struct {
		Resources []struct {
			Manifest json.RawMessage `json:"manifest"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(doc.Resources) == 0 {
		t.Fatal("no resources")
	}
	for i, r := range doc.Resources {
		if !bytes.HasPrefix(bytes.TrimSpace(r.Manifest), []byte("{")) {
			t.Errorf("resource %d: manifest is not an object: %.40s", i, r.Manifest)
		}
	}
}

// A file that isn't a snapshot fails with a message saying so.
func TestSnapshotJSONRejectsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"empty": "", "text": "apiVersion: v1\nkind: ConfigMap\n"} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		root := newRootCmd()
		root.SetArgs([]string{"snapshot", "json", f})
		root.SetOut(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "not a snapshot") {
			t.Errorf("%s: got error %v, want one saying not a snapshot", name, err)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
