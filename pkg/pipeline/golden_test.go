// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline_test

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/corpus/*/expected")

const corpusDir = "../../testdata/corpus"

// TestGolden runs every case in testdata/corpus: <case>/input holds manifests
// as read from a cluster, <case>/expected holds the snapshot JSON and emitted
// repository. Run `make golden` to regenerate after an intended change, then
// review the diff.
func TestGolden(t *testing.T) {
	cases, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			caseDir := filepath.Join(corpusDir, c.Name())
			got := t.TempDir()
			run(t, filepath.Join(caseDir, "input"), got)

			want := filepath.Join(caseDir, "expected")
			if *update {
				if err := os.RemoveAll(want); err != nil {
					t.Fatal(err)
				}
				if err := os.CopyFS(want, os.DirFS(got)); err != nil {
					t.Fatal(err)
				}
				return
			}
			compareTrees(t, want, got)
		})
	}
}

func run(t *testing.T, input, out string) {
	t.Helper()
	snap, err := pipeline.New(collect.FileSource{Dir: input}).Run(context.Background(), snapshot.Meta{
		ID:         "golden",
		CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Cluster:    &snapshotv1alpha1.Cluster{Name: "golden", KubernetesVersion: "v1.35.0"},
		Producer:   &snapshotv1alpha1.Producer{Name: "setdrift-test", Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The CLI's defaults.
	es, err := kustomize.DefaultExternalSecrets("auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := (kustomize.Emitter{ExternalSecrets: es}).Emit(snap, filepath.Join(out, "repo")); err != nil {
		t.Fatal(err)
	}
	js, err := snapshot.MarshalInspectJSON(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "snapshot.json"), js, 0o644); err != nil {
		t.Fatal(err)
	}

	// The protobuf form must round-trip.
	b, err := snapshot.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	back, err := snapshot.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	js2, err := snapshot.MarshalInspectJSON(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(js, js2) {
		t.Error("snapshot changed after a protobuf round trip")
	}
}

func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	files := func(root string) map[string][]byte {
		m := map[string][]byte{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			m[rel], err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatalf("reading %s: %v (run `make golden` to create it)", root, err)
		}
		return m
	}
	w, g := files(want), files(got)
	for name, wb := range w {
		gb, ok := g[name]
		switch {
		case !ok:
			t.Errorf("missing output file %s", name)
		case !bytes.Equal(wb, gb):
			t.Errorf("%s differs from golden.\n--- want\n%s\n--- got\n%s", name, wb, gb)
		}
	}
	for name := range g {
		if _, ok := w[name]; !ok {
			t.Errorf("unexpected output file %s", name)
		}
	}
}
