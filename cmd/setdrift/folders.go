// tells what kind of SetDrift folder a path is, an export, a translation of one or an export's own platform set, so each command says plainly when it was given the wrong one
// an export's cluster directory has .setdrift-export, a translation is translations/<code> in it with its translation.yaml stamp, a platform set is platform/<cloud>/<dist> in it

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// the kinds of folder folderKind tells apart
const (
	folderOther       = iota // none of these, like a folder of manifests
	folderExport             // an export, or a cluster's directory in one
	folderTranslation        // a translation, translations/<code> in a cluster's directory
	folderPlatformSet        // an export's own platform set, platform/<cloud>/<dist> in a cluster's directory
)

// folder is what folderKind found
type folder struct {
	kind int
	// root is the cluster's directory the folder belongs to, for a translation or a platform set
	root string
	// rel is the folder's path in the cluster's directory, like translations/te2g
	rel string
	// stamp is a translation's
	stamp *translate.Stamp
}

// folderKind tells what dir is, by the files SetDrift writes, not by its name alone
func folderKind(dir string) folder {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return folder{}
	}
	// checking if its a translation, its stamp says so
	if b, err := os.ReadFile(filepath.Join(abs, translate.StampFile)); err == nil { //#nosec G304 -- inside the directory the user named
		var st translate.Stamp
		if yaml.Unmarshal(b, &st) == nil && st.To != "" {
			root := filepath.Dir(filepath.Dir(abs))
			return folder{kind: folderTranslation, root: root, rel: filepath.ToSlash(must(filepath.Rel(root, abs))), stamp: &st}
		}
	}
	// checking if its a cluster's directory or an export holding them
	if _, err := os.Stat(filepath.Join(abs, kustomize.ExportInfoFile)); err == nil {
		return folder{kind: folderExport, root: abs}
	}
	if m, _ := filepath.Glob(filepath.Join(abs, "clusters", "*", kustomize.ExportInfoFile)); len(m) > 0 {
		return folder{kind: folderExport, root: abs}
	}
	// checking if its platform/<cloud>/<dist> in a cluster's directory, three levels up has .setdrift-export
	root := filepath.Dir(filepath.Dir(filepath.Dir(abs)))
	if filepath.Base(filepath.Dir(filepath.Dir(abs))) == "platform" {
		if _, err := os.Stat(filepath.Join(root, kustomize.ExportInfoFile)); err == nil {
			return folder{kind: folderPlatformSet, root: root, rel: filepath.ToSlash(must(filepath.Rel(root, abs)))}
		}
	}
	return folder{}
}

// describe says what a translation is, like the EKS to GKE translation (te2g)
func (f folder) describe() string {
	if f.stamp == nil {
		return "a translation"
	}
	name := func(d string) string {
		if n := platform.Distribution(d).Name(); n != "" {
			return n
		}
		return "a cluster on no known cloud"
	}
	code := f.stamp.Code
	if code == "" {
		code = filepath.Base(f.rel)
	}
	return "the " + name(f.stamp.From) + " to " + name(f.stamp.To) + " translation (" + code + ")"
}

// checkExportFolders refuses what export can't write to or read from, whatever --force says, with what to give instead
// -o takes an export's root, the directory holding clusters/, never a translation, a platform set, a cluster's directory or a place inside one
// --from-dir takes manifests, an export or a folder of them, never a translation or a platform set, which are changes to an export, not manifests of their own
func checkExportFolders(output, fromDir string) error {
	if output != "" {
		switch f := folderKind(output); {
		case f.kind == folderTranslation:
			return fmt.Errorf("-o %s is %s inside the export at %s, and an export can't go in it: a translation and an export aren't interchangeable. To export again, give the export's root, -o %s",
				output, f.describe(), f.root, exportRootOf(f.root))
		case f.kind == folderPlatformSet:
			return fmt.Errorf("-o %s is the export's own platform set (%s), and an export can't go in it. To export again, give the export's root, -o %s", output, f.rel, exportRootOf(f.root))
		}
		// checking if -o is a cluster's directory or inside one, an export there would nest inside the export
		if c := clusterDirAbove(output); c != "" {
			where := "inside the export's cluster directory " + c
			// checking if -o is the cluster's directory itself, if so it says so
			if abs, _ := filepath.Abs(output); abs == c {
				where = "an export's cluster directory"
			}
			return fmt.Errorf("-o %s is %s, and an export can't go in it. To export again, give the export's root, the directory holding clusters/: -o %s", output, where, exportRootOf(c))
		}
	}
	if fromDir != "" {
		switch f := folderKind(fromDir); f.kind {
		case folderTranslation:
			return fmt.Errorf("--from-dir %s is %s, changes for another platform rather than manifests: a translation and an export aren't interchangeable. Export from the export, --from-dir %s, or from the cluster the translation was applied to", fromDir, f.describe(), f.root)
		case folderPlatformSet:
			return fmt.Errorf("--from-dir %s is the export's own platform set (%s), changes to the export rather than manifests: export from the export, --from-dir %s", fromDir, f.rel, f.root)
		}
	}
	return nil
}

// clusterDirAbove gives back the export's cluster directory dir is, or is inside, the nearest one with .setdrift-export, empty when theres none
func clusterDirAbove(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, kustomize.ExportInfoFile)); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// exportRootOf gives back an export's root from its cluster's directory, two levels up from clusters/<cluster>
func exportRootOf(clusterDir string) string {
	return filepath.Dir(filepath.Dir(clusterDir))
}

// staleTranslation says when a translation is older than its export, so its overlay and decisions may be behind it, empty when it isnt
func staleTranslation(root, rel string, info *kustomize.ExportInfo) string {
	f := folderKind(filepath.Join(root, filepath.FromSlash(rel)))
	if f.kind != folderTranslation || info == nil {
		return ""
	}
	ex, e1 := time.Parse(time.RFC3339, info.ExportedAt)
	tr, e2 := time.Parse(time.RFC3339, f.stamp.TranslatedAt)
	if e1 != nil || e2 != nil || !ex.After(tr) {
		return ""
	}
	return fmt.Sprintf("Note: the export was taken again (%s) after this translation (%s), so it may be behind it: run setdrift translate %s --to %s, which asks again about any decision whose object changed.",
		info.ExportedAt, f.stamp.TranslatedAt, exportRootOf(root), f.stamp.To)
}

// olderApplyNote says when verify found objects whose translation annotation differs from the translation's, applied from another run of it, empty when none
func olderApplyNote(r *verify.Report) string {
	n, from, to := 0, "", ""
	for _, d := range r.Diffs {
		for _, f := range d.Fields {
			if strings.HasSuffix(f.Path, capability.TranslationAnnotation) || strings.Contains(f.Path, `"`+capability.TranslationAnnotation+`"`) {
				n++
				from, to = fmt.Sprint(f.Cluster), fmt.Sprint(f.Export)
				break
			}
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d of the differences are objects applied from another run of this translation (%s, where the export's is %s): apply the current translation.", n, from, to)
}

// keptTranslations says, for each translation an export kept from before, that its older than the export now and how to bring it in step
func keptTranslations(root, out string) []string {
	var lines []string
	entries, _ := os.ReadDir(filepath.Join(root, "translations"))
	for _, e := range entries {
		f := folderKind(filepath.Join(root, "translations", e.Name()))
		if f.kind != folderTranslation {
			continue
		}
		lines = append(lines, fmt.Sprintf("Kept %s (translations/%s) and its decisions. It was made from the previous export: run setdrift translate %s --to %s to bring it in step.",
			f.describe(), e.Name(), out, f.stamp.To))
	}
	return lines
}

// must gives back a path, Rel only fails across volumes, which these never are
func must(s string, _ error) string { return s }

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
