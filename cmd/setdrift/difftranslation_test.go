package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
	"setdrift.io/setdrift-engine/pkg/translate"
)

// check --translation-dir takes only a translation, and names the export's translations otherwise
func TestLoadTranslationRefusesOthers(t *testing.T) {
	root := miniExport(t, false)
	tr := filepath.Join(root, "translations", "te2g")
	_ = os.MkdirAll(tr, 0o750)
	_ = os.WriteFile(filepath.Join(tr, "translation.yaml"), []byte("code: te2g\nfrom: eks\nto: gke\n"), 0o600)
	for dir, want := range map[string]string{
		root:                              "is an export, not a translation: give one of its translations, like " + tr,
		t.TempDir():                       "isn't a translation (it has no translation.yaml)",
		filepath.Join(root, "namespaces"): "isn't a translation",
	} {
		if _, err := loadTranslation(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", dir, err, want)
		}
	}
	// the mini export has no snapshot, which diff needs
	if _, err := loadTranslation(tr); err == nil || !strings.Contains(err.Error(), "export again") {
		t.Errorf("no snapshot: %v", err)
	}
}

// check the target is the side whose objects carry the translation's code, else the side on the translation's platform, else its an error
func TestTranslationDirection(t *testing.T) {
	in := &translationInput{stamp: &translate.Stamp{Code: "te2g", From: "eks", To: "gke"}, dir: folder{kind: folderTranslation, rel: "translations/te2g", stamp: &translate.Stamp{Code: "te2g", From: "eks", To: "gke"}}}
	snap := func(tattoo bool, d platform.Distribution) *snapshotv1alpha1.Snapshot {
		m := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`
		if tattoo {
			m = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a","annotations":{"setdrift.io/translation":"te2g"}}}`
		}
		s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{}, Resources: []*snapshotv1alpha1.Resource{{ApiVersion: "v1", Kind: "ConfigMap", Name: "a", ManifestJson: m}}}
		if d != platform.Unknown {
			s.Cluster.Classification = platform.Decide([]platform.Signal{{Distribution: d, Weight: 100, Source: "test"}}).Proto()
		}
		return s
	}
	if swap, _, err := in.direction(snap(false, platform.Unknown), snap(true, platform.Unknown)); swap || err != nil {
		t.Errorf("target second: %v %v", swap, err)
	}
	if swap, why, err := in.direction(snap(true, platform.Unknown), snap(false, platform.Unknown)); !swap || err != nil || !strings.Contains(why, "carry this translation's annotation") {
		t.Errorf("target first: %v %q %v", swap, why, err)
	}
	if swap, why, err := in.direction(snap(false, platform.GKE), snap(false, platform.EKS)); !swap || err != nil || !strings.Contains(why, "the translation's target") {
		t.Errorf("by platform: %v %q %v", swap, why, err)
	}
	if _, _, err := in.direction(snap(false, platform.AKS), snap(false, platform.EKS)); err == nil || !strings.Contains(err.Error(), "neither side is this translation's target") {
		t.Errorf("neither: %v", err)
	}
}

// check the target's objects from another run of the translation are said, with how many and the run, and none when theyre all from this one
func TestOlderApply(t *testing.T) {
	in := &translationInput{stamp: &translate.Stamp{Code: "te2g", ID: "aaaa1111", From: "eks", To: "gke"}, dir: folder{root: "/x/clusters/prod", rel: "translations/te2g"}}
	snap := func(values ...string) *snapshotv1alpha1.Snapshot {
		s := &snapshotv1alpha1.Snapshot{}
		for i, v := range values {
			s.Resources = append(s.Resources, &snapshotv1alpha1.Resource{Kind: "ConfigMap", Name: string(rune('a' + i)),
				ManifestJson: `{"metadata":{"annotations":{"setdrift.io/translation":"` + v + `"}}}`})
		}
		return s
	}
	if note := in.olderApply(snap("te2g-aaaa1111", "te2g-aaaa1111")); note != "" {
		t.Errorf("current: %s", note)
	}
	if note := in.olderApply(snap("te2g-aaaa1111", "te2g-bbbb2222", "te2g-bbbb2222", "tg2e-cccc")); !strings.Contains(note, "2 from te2g-bbbb2222") || strings.Contains(note, "tg2e") {
		t.Errorf("older: %s", note)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
