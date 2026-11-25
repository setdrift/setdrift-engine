package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// check an export, a translation and a platform set are told apart by the files SetDrift writes, and each command says what it was given
func TestFolderKind(t *testing.T) {
	root := miniExport(t, false)
	tr := filepath.Join(root, "translations", "te2g")
	ps := filepath.Join(root, "platform", "aws", "eks")
	for _, d := range []string{tr, ps} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tr, "translation.yaml"), []byte("code: te2g\nfrom: eks\nto: gke\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := folderKind(root); f.kind != folderExport {
		t.Errorf("export: %+v", f)
	}
	if f := folderKind(tr); f.kind != folderTranslation || f.rel != "translations/te2g" || f.describe() != "the EKS to GKE translation (te2g)" {
		t.Errorf("translation: %+v %s", f, f.describe())
	}
	if f := folderKind(ps); f.kind != folderPlatformSet || f.rel != "platform/aws/eks" {
		t.Errorf("platform set: %+v", f)
	}
	if f := folderKind(t.TempDir()); f.kind != folderOther {
		t.Errorf("other: %+v", f)
	}
	for dir, want := range map[string]string{tr: "is the EKS to GKE translation (te2g) of the export at", ps: "is the export's own platform set (platform/aws/eks)"} {
		if _, _, err := runTranslate(dir, "--to", "aks"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("translate %s: %v", dir, err)
		}
	}
}

// check export refuses a translation, a platform set or a place inside an export as -o, and a translation or a platform set as --from-dir, saying what to give instead
func TestExportFolders(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "clusters", "prod")
	tr := filepath.Join(root, "translations", "te2g")
	ps := filepath.Join(root, "platform", "aws", "eks")
	for _, d := range []string{tr, ps, filepath.Join(root, "namespaces")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, ".setdrift-export"), []byte("cluster: prod\n"), 0o600)
	_ = os.WriteFile(filepath.Join(tr, "translation.yaml"), []byte("code: te2g\nfrom: eks\nto: gke\n"), 0o600)
	for _, c := range []struct{ output, fromDir, want string }{
		{output: tr, want: "aren't interchangeable. To export again, give the export's root, -o " + parent},
		{output: ps, want: "is the export's own platform set (platform/aws/eks)"},
		{output: root, want: "is an export's cluster directory"},
		{output: filepath.Join(root, "namespaces"), want: "is inside the export's cluster directory"},
		{fromDir: tr, want: "Export from the export, --from-dir " + root},
		{fromDir: ps, want: "export from the export, --from-dir " + root},
	} {
		if err := checkExportFolders(c.output, c.fromDir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("-o %q --from-dir %q: %v, want %q", c.output, c.fromDir, err, c.want)
		}
	}
	// the export's root, and an export or a folder of manifests to read, are fine
	for _, c := range [][2]string{{parent, ""}, {t.TempDir(), parent}, {"", root}, {"", t.TempDir()}} {
		if err := checkExportFolders(c[0], c[1]); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
