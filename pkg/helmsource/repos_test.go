package helmsource

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeRepos(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helm-repos.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bad files fail on load, naming the entry, before anything is read.
func TestLoadReposRejects(t *testing.T) {
	for content, want := range map[string]string{
		"- nginx: https://x.example.com\n":            "want a map",
		"nginx: {url: https://x.example.com}\n":       "want a map",
		"nginx: charts.example.com\n":                 "must start with",
		"nginx: ftp://charts.example.com\n":           "must start with",
		"nginx: https://user:pw@charts.example.com\n": "credentials",
		"a/b/c: https://charts.example.com\n":         "neither a chart",
		"/web: https://charts.example.com\n":          "neither a chart",
		"apps/: https://charts.example.com\n":         "neither a chart",
		"nginx: oci://\n":                             "no host",
	} {
		_, err := LoadRepos(writeRepos(t, content))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got error %v, want one saying %q", content, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%q: error repeats the credentials", content)
		}
	}
}

// A release's own entry wins, then the post-renderer's annotation, then an
// entry for the chart; release entries no release matched are reported.
func TestReposLookup(t *testing.T) {
	r, err := LoadRepos(writeRepos(t, `
nginx: https://charts.example.com
apps/web: oci://registry.example.com/charts
apps/gone: https://charts.example.com
only-in-another-cluster: https://charts.example.com
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ns, release, chart, recorded, want string
	}{
		{"apps", "web", "nginx", "https://annotated.example.com", "oci://registry.example.com/charts"},
		{"apps", "other", "nginx", "https://annotated.example.com", "https://annotated.example.com"},
		{"apps", "third", "nginx", "", "https://charts.example.com"},
		{"apps", "fourth", "redis", "", ""},
		{"apps", "fifth", "", "", ""},
	} {
		if got := r.Lookup(c.ns, c.release, c.chart, c.recorded); got != c.want {
			t.Errorf("%s/%s (%s): %q, want %q", c.ns, c.release, c.chart, got, c.want)
		}
	}
	// only release entries: an unused chart entry is normal in a shared file
	if got := r.Unmatched(); !slices.Equal(got, []string{"apps/gone"}) {
		t.Errorf("unmatched: %v, want [apps/gone]", got)
	}

	var none *Repos
	if got := none.Lookup("apps", "web", "nginx", "https://annotated.example.com"); got != "https://annotated.example.com" {
		t.Errorf("nil Repos: %q", got)
	}
	if none.Unmatched() != nil {
		t.Error("nil Repos has unmatched entries")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
