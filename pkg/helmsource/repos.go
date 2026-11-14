// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// The --helm-repos file: the chart repository of each Helm release, given
// by the user at export time. Helm keeps no record of it, and this needs no
// change to the cluster or to how Helm is run, unlike the post-renderer.

package helmsource

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Repos maps Helm releases or charts to the chart repository they were
// installed from, read from a YAML file of entries like:
//
//	ingress-nginx: https://kubernetes.github.io/ingress-nginx  # any release of the chart
//	monitoring/prometheus: https://prometheus-community.github.io/helm-charts  # one release
//
// A key with a slash is <namespace>/<release>; without one, a chart name
// (Helm chart names can't hold a slash). A nil *Repos has no entries.
type Repos struct {
	byRelease map[string]string
	byChart   map[string]string
	// matched holds the release keys a lookup matched, to report the others
	matched map[string]bool
}

// LoadRepos reads and checks a --helm-repos file.
func LoadRepos(path string) (*Repos, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries map[string]string
	// strict: a typo'd structure (a list, a nested map) fails instead of
	// loading nothing
	if err := yaml.UnmarshalStrict(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: want a map of <chart> or <namespace>/<release> to a chart repository URL: %w", path, err)
	}
	r := &Repos{byRelease: map[string]string{}, byChart: map[string]string{}, matched: map[string]bool{}}
	for key, repo := range entries {
		if err := CheckRepoURL(repo); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		ns, release, isRelease := strings.Cut(key, "/")
		switch {
		case !isRelease && key != "":
			r.byChart[key] = repo
		case isRelease && ns != "" && release != "" && !strings.Contains(release, "/"):
			r.byRelease[key] = repo
		default:
			return nil, fmt.Errorf("%s: %q is neither a chart name nor <namespace>/<release>", path, key)
		}
	}
	return r, nil
}

// Lookup returns the chart repository of a release: its own entry first,
// then recorded (the post-renderer's annotation, evidence from install
// time), then an entry for its chart. "" means unknown.
func (r *Repos) Lookup(namespace, release, chart, recorded string) string {
	if r == nil {
		return recorded
	}
	key := namespace + "/" + release
	byRelease, okRelease := r.byRelease[key]
	byChart, okChart := r.byChart[chart]
	if okRelease {
		r.matched[key] = true
	}
	switch {
	case okRelease:
		return byRelease
	case recorded != "":
		return recorded
	case okChart && chart != "":
		return byChart
	}
	return ""
}

// Unmatched returns the <namespace>/<release> entries no lookup matched,
// sorted: most likely typos, or releases no longer in the cluster. Chart
// entries are left out: a file shared by several clusters names charts
// that only some of them run, and a mistyped chart shows up anyway, as a
// chart RESTORE.md still asks a repository for.
func (r *Repos) Unmatched() []string {
	if r == nil {
		return nil
	}
	var out []string
	for k := range r.byRelease {
		if !r.matched[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
