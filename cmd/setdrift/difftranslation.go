// diff --translation-dir, a comparison through a translation: it reads the translation, the export it came from and the export's snapshot,
// works out which side is the translation's target, by the setdrift.io/translation annotation its objects carry, and sorts the differences through it

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// translationInput is a translation read for diff
type translationInput struct {
	dir   folder
	stamp *translate.Stamp
	// exported is the export's snapshot, what the source was when translated, built its overlay built
	exported *snapshotv1alpha1.Snapshot
	built    []*unstructured.Unstructured
	explain  func(*unstructured.Unstructured) string
	// note says something the user should know, like a translation older than its export
	note string
}

// loadTranslation reads the translation at dir for diff, refusing anything but a translation, with what to give instead
func loadTranslation(dir string) (*translationInput, error) {
	f := folderKind(dir)
	switch f.kind {
	case folderTranslation:
	case folderExport:
		return nil, fmt.Errorf("--translation-dir %s is an export, not a translation: give one of its translations%s", dir, translationsOf(dir))
	case folderPlatformSet:
		return nil, fmt.Errorf("--translation-dir %s is the export's own platform set (%s), not a translation: give one of the export's translations%s", dir, f.rel, translationsOf(f.root))
	default:
		return nil, fmt.Errorf("--translation-dir %s isn't a translation (it has no %s): give the directory setdrift translate wrote, like <path-to-export>/clusters/<cluster>/translations/te2g", dir, translate.StampFile)
	}
	if f.stamp.Code == "" {
		return nil, errNoTranslationCode
	}
	in := &translationInput{dir: f, stamp: f.stamp}
	info, err := kustomize.ReadExportInfo(f.root)
	if err != nil {
		return nil, err
	}
	// invoking exportSnapshot to find the snapshot the export took, the source as it was translated, its defined below
	snapPath, err := exportSnapshot(f.root, info)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(snapPath) //#nosec G304 -- inside the export the user named
	if err != nil {
		return nil, readError(snapPath, err)
	}
	if in.exported, err = snapshot.Parse(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", snapPath, err)
	}
	// invoking verify.Build to build the translation as kubectl apply -k would, its defined in pkg/verify/dryrun.go
	dirAbs := filepath.Join(f.root, filepath.FromSlash(f.rel))
	if in.built, err = verify.Build(f.root, dirAbs); err != nil {
		return nil, err
	}
	ds, err := translate.ReadDecisions(dirAbs)
	if err != nil {
		return nil, err
	}
	in.explain = translate.Explainer(ds)
	// checking if the export was taken again after the translation, if so the translation may be behind it
	if info != nil && f.stamp != nil {
		ex, e1 := time.Parse(time.RFC3339, info.ExportedAt)
		tr, e2 := time.Parse(time.RFC3339, f.stamp.TranslatedAt)
		if e1 == nil && e2 == nil && ex.After(tr) {
			in.note = fmt.Sprintf("Note: the export was taken again (%s) after this translation (%s): run setdrift translate %s --to %s to bring it in step.", info.ExportedAt, f.stamp.TranslatedAt, exportRootOf(f.root), f.stamp.To)
		}
	}
	return in, nil
}

// translationsOf lists an export's translations for a message, empty when it has none
func translationsOf(dir string) string {
	roots := []string{dir}
	if m, _ := filepath.Glob(filepath.Join(dir, "clusters", "*")); len(m) > 0 {
		roots = m
	}
	var found []string
	for _, r := range roots {
		m, _ := filepath.Glob(filepath.Join(r, capability.TranslationsDir, "*", translate.StampFile))
		for _, s := range m {
			found = append(found, filepath.Dir(s))
		}
	}
	if len(found) == 0 {
		return " (it has none yet: setdrift translate writes them)"
	}
	sort.Strings(found)
	return ", like " + strings.Join(found, " or ")
}

// exportSnapshot finds the snapshot the export took when it was written, in its snapshots/, by the time .setdrift-export records
func exportSnapshot(clusterRoot string, info *kustomize.ExportInfo) (string, error) {
	root := exportRootOf(clusterRoot)
	if info == nil || info.ExportedAt == "" {
		return "", fmt.Errorf("%s doesn't record when it was exported, so diff can't find the snapshot the translation came from: export again", clusterRoot)
	}
	at, err := time.Parse(time.RFC3339, info.ExportedAt)
	if err != nil {
		return "", fmt.Errorf("%s: %w", kustomize.ExportInfoFile, err)
	}
	m, _ := filepath.Glob(filepath.Join(root, snapshotsDir, at.UTC().Format("2006-01-02T150405Z")+"-*.pb"))
	if len(m) == 0 {
		return "", fmt.Errorf("the export at %s has no snapshot from when it was taken (%s) in %s/, which diff compares through the translation: export again without --no-snapshot", root, info.ExportedAt, snapshotsDir)
	}
	return m[0], nil
}

// direction tells if the sides are the other way around, the target given first, by the translation's annotation on their objects, else by their platforms
// it gives back why when it swaps, and an error when neither side is the translation's target
func (in *translationInput) direction(sa, sb *snapshotv1alpha1.Snapshot) (swap bool, why string, err error) {
	code := in.stamp.Code
	ta, tb := tattooed(sa, code), tattooed(sb, code)
	switch {
	case tb > 0 && ta == 0:
		return false, "", nil
	case ta > 0 && tb == 0:
		return true, fmt.Sprintf("the first side's objects carry this translation's annotation (setdrift.io/translation: %s), so it's the target, and the sides are compared the other way around", code), nil
	}
	da, db := distribution(sa), distribution(sb)
	to, from := platform.Distribution(in.stamp.To), platform.Distribution(in.stamp.From)
	switch {
	case db == to && da != to:
		return false, "", nil
	case da == to && db != to:
		return true, fmt.Sprintf("the first side is %s, the translation's target, and the second isn't, so they're compared the other way around", to.Name()), nil
	}
	return false, "", fmt.Errorf("neither side is this translation's target: it's %s, and its objects carry setdrift.io/translation: %s once applied. Compare the %s cluster it was applied to with its source (%s)",
		in.describeDirection(), code, to.Name(), from.Name())
}

// describeDirection names the translation for a message
func (in *translationInput) describeDirection() string { return in.dir.describe() }

// tattooed counts a snapshot's objects that carry the translation's annotation with this code, of any run of it
func tattooed(s *snapshotv1alpha1.Snapshot, code string) int {
	return len(tattoos(s, code))
}

// tattoos gives back the translation annotation's value on each of a snapshot's objects that carry this code, code-id, or the code alone from before ids
func tattoos(s *snapshotv1alpha1.Snapshot, code string) []string {
	var out []string
	key := `"` + capability.TranslationAnnotation + `":"`
	for _, r := range s.GetResources() {
		m := r.GetManifestJson()
		i := strings.Index(m, key)
		if i < 0 {
			continue
		}
		v := m[i+len(key):]
		v = v[:strings.IndexByte(v, '"')]
		if v == code || strings.HasPrefix(v, code+"-") {
			out = append(out, v)
		}
	}
	return out
}

// olderApply says when the target's objects were applied from another run of the translation than the export's, empty when theyre all from this one
func (in *translationInput) olderApply(target *snapshotv1alpha1.Snapshot) string {
	want := in.stamp.Code + "-" + in.stamp.ID
	if in.stamp.ID == "" {
		return ""
	}
	others := map[string]int{}
	for _, v := range tattoos(target, in.stamp.Code) {
		if v != want {
			others[v]++
		}
	}
	if len(others) == 0 {
		return ""
	}
	var parts []string
	for v, n := range others {
		parts = append(parts, fmt.Sprintf("%d from %s", n, v))
	}
	sort.Strings(parts)
	return fmt.Sprintf("Note: some of the target's objects were applied from another run of this translation (%s), not this one (%s): apply the current translation, kubectl apply --server-side -k %s",
		strings.Join(parts, ", "), want, filepath.Join(in.dir.root, filepath.FromSlash(in.dir.rel)))
}

// distribution is a snapshot's platform, as the export classified it, Unknown when it didnt
func distribution(s *snapshotv1alpha1.Snapshot) platform.Distribution {
	c := s.GetCluster().GetClassification()
	if c == nil {
		return platform.Unknown
	}
	return platform.FromProto(c).Distribution
}

// forDiff gives back the translation as diff sorts by, its objects keyed the way diff keys them
func (in *translationInput) forDiff() (diff.Translation, error) {
	t := diff.Translation{Code: in.stamp.Code, Exported: map[diff.Key]any{}, Translated: map[diff.Key]any{}}
	capMap, err := capability.Load()
	if err != nil {
		return t, err
	}
	notMoved := map[diff.Key]string{}
	for _, r := range in.exported.GetResources() {
		if kustomize.Platform(r) {
			continue
		}
		var m any
		if err := json.Unmarshal([]byte(r.GetManifestJson()), &m); err != nil {
			return t, fmt.Errorf("the export's snapshot, %s %s/%s: %w", r.GetKind(), r.GetNamespace(), r.GetName(), err)
		}
		k := diff.Key{Group: kube.Group(r.GetApiVersion()), Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName()}
		t.Exported[k] = m
		switch {
		case kustomize.ReferenceOnly(r):
			notMoved[k] = "holds secret values, which SetDrift never moves: supply them as RESTORE.md's secrets step says"
		case kustomize.RanOnce(r):
			notMoved[k] = "ran once, so the export leaves it out"
		case heldBack(capMap, r):
			notMoved[k] = "manages a real cloud resource, held back in cloud-resources/: RESTORE.md moves it on purpose"
		}
	}
	for _, o := range in.built {
		m, err := diff.Plain(o.Object)
		if err != nil {
			return t, err
		}
		gvk := o.GroupVersionKind()
		t.Translated[diff.Key{Group: gvk.Group, Kind: gvk.Kind, Namespace: o.GetNamespace(), Name: o.GetName()}] = m
	}
	t.NotMoved = func(k diff.Key) string { return notMoved[k] }
	t.Explain = func(m any) string {
		obj, ok := m.(map[string]any)
		if !ok {
			return ""
		}
		return in.explain(&unstructured.Unstructured{Object: obj})
	}
	return t, nil
}

// heldBack tells if export held a resource back in cloud-resources/, an object of an operator that creates cloud resources
func heldBack(m *capability.Map, r *snapshotv1alpha1.Resource) bool {
	op, _ := m.CloudResourceOperator(kube.Group(r.GetApiVersion()))
	return op != nil && !kustomize.ReferenceOnly(r)
}

// errNoTranslationCode is for a stamp written before codes, which diff can't match annotations to
var errNoTranslationCode = errors.New("this translation was written before translations had codes: run setdrift translate again")

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
