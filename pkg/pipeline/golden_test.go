// runs the golden corpus through the pipeline and the kustomize emitter and compares the output with the files on record

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

// update rewrites the golden files instead of comparing against them, make golden sets it
var update = flag.Bool("update", false, "rewrite golden files in testdata/corpus/*/expected")

// corpusDir is where the golden cases live
const corpusDir = "../../testdata/corpus"

// check that every case in testdata/corpus gives the snapshot json and the emitted repository in <case>/expected from the manifests in <case>/input, as read from a cluster
// run `make golden` to regenerate after an intended change, then review the diff
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

// run takes a case's input directory, captures and emits it into out, and checks the snapshot survives a protobuf round trip
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
	// the cli's defaults
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

	// the protobuf form must round-trip
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

// compareTrees reports every file missing from got, differing from want, or not expected at all
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

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
