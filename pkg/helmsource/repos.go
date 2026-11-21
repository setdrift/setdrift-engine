// reads the --helm-repos file, the chart repository of each Helm release, given by the user at export time
// Helm keeps no record of it, and unlike the post-renderer this needs no change to the cluster or to how Helm is run

package helmsource

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Repos maps Helm releases or charts to the chart repository they were installed from, read from a YAML map of keys to URLs
// a key with a slash is <namespace>/<release> for one release, like monitoring/prometheus, without one its a chart name for any release of that chart, like ingress-nginx
// Helm chart names cant hold a slash, so the two never clash, and a nil *Repos has no entries
type Repos struct {
	// the <namespace>/<release> entries
	byRelease map[string]string
	// the chart name entries
	byChart map[string]string
	// the release keys a lookup matched, to report the others
	matched map[string]bool
}

// LoadRepos takes the path of a --helm-repos file and reads and checks it, every key must be a chart or <namespace>/<release> and every URL must pass CheckRepoURL
func LoadRepos(path string) (*Repos, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- the user's --helm-repos file
	if err != nil {
		return nil, err
	}
	var entries map[string]string
	// strict, so a mistyped structure like a list or a nested map fails instead of loading nothing
	if err := yaml.UnmarshalStrict(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: want a map of <chart> or <namespace>/<release> to a chart repository URL: %w", path, err)
	}
	r := &Repos{byRelease: map[string]string{}, byChart: map[string]string{}, matched: map[string]bool{}}
	// going over each entry to check it and file it as a chart or a release
	for key, repo := range entries {
		// invoking CheckRepoURL to refuse a bad or credentialed URL, its defined in helmsource.go
		if err := CheckRepoURL(repo); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		ns, release, isRelease := strings.Cut(key, "/")
		// checking if the key is a chart name, a release with both parts and a single slash, or neither
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

// Lookup takes a release, its chart and the repository recorded for it and gives back its chart repository, empty when unknown
// the release's own entry comes first, then recorded, the post-renderer's annotation from install time, then an entry for its chart
func (r *Repos) Lookup(namespace, release, chart, recorded string) string {
	// checking if theres no file, if so only the recorded repository is known
	if r == nil {
		return recorded
	}
	key := namespace + "/" + release
	byRelease, okRelease := r.byRelease[key]
	byChart, okChart := r.byChart[chart]
	// checking if the release has its own entry, if so its marked matched so Unmatched leaves it out
	if okRelease {
		r.matched[key] = true
	}
	// checking each source in order, the first one found wins
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

// Unmatched gives back the <namespace>/<release> entries no lookup matched, sorted, most likely typos or releases no longer in the cluster
// chart entries are left out, a file shared by several clusters names charts only some of them run, and a mistyped chart shows up anyway as a chart RESTORE.md still asks a repository for
func (r *Repos) Unmatched() []string {
	// checking if theres no file, if so nothing can be unmatched
	if r == nil {
		return nil
	}
	var out []string
	// going over each release entry
	for k := range r.byRelease {
		// checking if no lookup matched it
		if !r.matched[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
