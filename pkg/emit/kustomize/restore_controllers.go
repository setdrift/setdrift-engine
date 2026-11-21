// writes RESTORE.md's controllers step, the helm releases, gitops tools and admission webhooks to have in place before the rest is applied

package kustomize

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/helmsource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// helmInstall is a helm release to install before the rest, as the export shows it
type helmInstall struct {
	ns, name, chart, version string
	// how many written objects belong to the release
	objects int
	// the release record from the inventory, nil when the export has none
	meta *snapshotv1alpha1.HelmRelease
}

// controllers writes the step for what must run before the rest is applied, helm releases, gitops tools and admission webhooks
func (d *restoreData) controllers(p *playbook) {
	// invoking gatherControllers to find all three in what was written, its defined below
	releases, gitopsObjects, webhooks := d.gatherControllers()
	// checking if there are none of them, if so theres no step
	if len(releases) == 0 && gitopsObjects == 0 && len(webhooks) == 0 {
		return
	}
	p.section("Controllers to install first")
	// checking if there are helm releases, if so helmSteps writes how to install them, its defined below
	if len(releases) > 0 {
		d.helmSteps(p, releases)
	}
	// checking if argo cd or flux manage objects, if so gitopsNote says they restore them, its defined below
	if gitopsObjects > 0 {
		gitopsNote(p, gitopsObjects)
	}
	// checking if there are admission webhooks, if so webhookSteps says to wait for their services, its defined below
	if len(webhooks) > 0 {
		webhookSteps(p, webhooks)
	}
}

// gatherControllers gives back the helm releases the written objects belong to, how many objects argo cd or flux manage, and the services admission webhooks call
func (d *restoreData) gatherControllers() (releases map[string]*helmInstall, gitopsObjects int, webhooks []string) {
	releases = map[string]*helmInstall{}
	// going through what was written
	for _, w := range d.written {
		r := w.resource
		// checking who manages the object, helm, argo cd or flux
		switch r.GetProvenance().GetSource() {
		case snapshotv1alpha1.Source_SOURCE_HELM:
			// invoking helmRelease for the release that installed it, its defined below
			m := helmRelease(r)
			// checking if only a low-confidence guess says helm, if so its not counted as a release
			if m == "" {
				break
			}
			rel := releases[m]
			// checking if this is the release's first object, if so its added
			if rel == nil {
				ns, name, _ := strings.Cut(m, "/")
				rel = &helmInstall{ns: ns, name: name}
				releases[m] = rel
			}
			rel.objects++
			// checking if the chart isnt known yet, if so chartOf reads it from the helm.sh/chart label, its defined below, labels in manifest.go
			if rel.chart == "" {
				rel.chart, rel.version = chartOf(labels(d.manifests.of(r))["helm.sh/chart"])
			}
		case snapshotv1alpha1.Source_SOURCE_ARGO_CD, snapshotv1alpha1.Source_SOURCE_FLUX:
			gitopsObjects++
		}
		// checking if its a webhook configuration, if so webhookServices gives the services it calls, its defined below
		if r.Kind == "MutatingWebhookConfiguration" || r.Kind == "ValidatingWebhookConfiguration" {
			webhooks = append(webhooks, webhookServices(d.manifests.of(r))...)
		}
	}
	// matching the inventory's helm release records to the releases found
	for _, h := range d.s.GetInventory().GetHelmReleases() {
		// checking if the record is for one of the releases
		if rel := releases[h.Namespace+"/"+h.Name]; rel != nil {
			rel.meta = h
			// checking if the record names the chart, if so it wins over the label
			if h.Chart != "" {
				rel.chart, rel.version = h.Chart, h.ChartVersion
			}
		}
	}
	return releases, gitopsObjects, webhooks
}

// helmSteps writes the helm releases to install, a table, the commands, and what is left for the user to fill in
func (d *restoreData) helmSteps(p *playbook, releases map[string]*helmInstall) {
	p.printf("**Helm releases.** Their objects are in this export, but Helm's release\n" +
		"records are not, so applying the export would leave them unmanaged by Helm.\n" +
		"Install each release with Helm before applying the rest; the exported\n" +
		"copies then match and change nothing. Take the values from the source;\n" +
		"they can hold secrets, so the commands keep them in a temporary file\n" +
		"outside the export:\n\n")
	p.printf("| Release | Namespace | Chart | Objects |\n|---|---|---|---|\n")
	// sortedMapKeys is defined in restore.go
	keys := sortedMapKeys(releases)
	// writing a table row per release, cell is defined in manifest.go
	for _, k := range keys {
		r := releases[k]
		chart := r.chart
		// checking if the chart is unknown, otherwise if the version is known its added
		if chart == "" {
			chart = "unknown"
		} else if r.version != "" {
			chart += " " + r.version
		}
		p.printf("| %s | %s | %s | %d |\n", cell(r.name), cell(r.ns), cell(chart), r.objects)
	}
	p.printf("\n")
	// invoking helmInstallCommands for the commands and what they couldnt fill in, its defined below
	lines, borrowed, noChart, unknown := d.helmInstallCommands(releases, keys)
	p.code(lines...)
	// checking if some repositories were taken from argo cd or flux, if so the step says to check them
	if len(borrowed) > 0 {
		p.printf("Helm does not record which repository a chart came from; these are\n"+
			"taken from Argo CD or Flux installing the same chart in this cluster.\n"+
			"Check them, or name the repository with `--helm-repos`:\n\n%s\n\n", strings.Join(borrowed, "\n"))
	}
	// checking if some charts are unknown, if so chartsToFind says how to find them, its defined below
	if len(noChart) > 0 {
		chartsToFind(p, noChart)
	}
	// checking if some repositories are unknown, if so reposToFind says how to fill them in, its defined below
	if len(unknown) > 0 {
		reposToFind(p, unknown)
	}
}

// helmInstallCommands gives back, for each release in keys' order, the commands that copy its values from the source and install it
// it also gives back the repositories taken from argo cd or flux, and the releases whose chart or repository is unknown
func (d *restoreData) helmInstallCommands(releases map[string]*helmInstall, keys []string) (lines, borrowed []string, noChart, unknown []*helmInstall) {
	// invoking gitopsChartRepos for the repositories argo cd and flux install charts from, its defined below
	gitops := gitopsChartRepos(d.s.GetResources(), &d.manifests)
	// building the commands for each release
	for _, k := range keys {
		r := releases[k]
		// the chart's name comes from a helm release record or a label, so its quoted, text.ShellQuote is defined in internal/text/text.go
		chartName := text.ShellQuote(r.chart)
		// checking if the chart is unknown, if so the command gets a placeholder and the release is listed to find
		if r.chart == "" {
			chartName = "<chart>"
			noChart = append(noChart, r)
		}
		// helm install <release> <chart ref> [--repo <url>]
		chart := "<repo>/" + chartName
		// invoking Lookup for the repository, the --helm-repos file first, then the post-renderer's annotation, its defined in pkg/helmsource/repos.go
		repo := d.helmRepos.Lookup(r.ns, r.name, r.chart, r.meta.GetRepository())
		// checking if theres still no repository and argo cd or flux installs the same chart, if so theirs is borrowed
		if g, ok := gitops[r.chart]; ok && repo == "" && r.chart != "" {
			repo = g.repo
			borrowed = append(borrowed, fmt.Sprintf("- %s: %s, as %s installs it", r.name, g.repo, g.from))
		}
		// checking what kind of repository it is, an oci one takes the chart in its url, an https one goes in --repo, none is unknown
		switch {
		case strings.HasPrefix(repo, "oci://"):
			chart = text.ShellQuote(strings.TrimSuffix(repo, "/") + "/" + cmp.Or(r.chart, "<chart>"))
		case repo != "":
			chart = chartName + " --repo " + text.ShellQuote(repo)
		default:
			unknown = append(unknown, r)
		}
		// the values file is outside the export, so the values (which can hold secrets) never reach git, SafeName is defined in kustomize.go
		values := `"${TMPDIR:-/tmp}/setdrift-` + SafeName(r.ns) + "-" + SafeName(r.name) + `-values.yaml"`
		install := fmt.Sprintf(`helm --kube-context "$DST" install %s %s -n %s --create-namespace -f %s`,
			text.ShellQuote(r.name), chart, text.ShellQuote(r.ns), values)
		// checking if the chart version is known, if so the install pins it
		if r.version != "" {
			install += " --version " + text.ShellQuote(r.version)
		}
		lines = append(lines,
			fmt.Sprintf(`helm --kube-context "$SRC" get values %s -n %s -o yaml > %s   # source cluster, read-only`,
				text.ShellQuote(r.name), text.ShellQuote(r.ns), values),
			install)
	}
	return lines, borrowed, noChart, unknown
}

// chartsToFind says how to find the charts of releases the export has no record of, by listing them on the source
func chartsToFind(p *playbook, noChart []*helmInstall) {
	p.printf("The export has no Helm release record for these (Helm keeps it in a Secret\n" +
		"in the release's namespace, which --from-dir input lacks), and their objects\n" +
		"have no helm.sh/chart label, so `<chart>` is for you to fill in. The CHART\n" +
		"column on the source shows it:\n\n")
	var cmds []string
	// building a helm list command per release, filtered to its exact name
	for _, r := range noChart {
		cmds = append(cmds, fmt.Sprintf(`helm --kube-context "$SRC" list -n %s --filter %s`, text.ShellQuote(r.ns), text.ShellQuote("^"+regexp.QuoteMeta(r.name)+"$")))
	}
	p.code(cmds...)

}

// reposToFind lists what the charts of releases with an unknown repository say about themselves, and a --helm-repos file to fill in
func reposToFind(p *playbook, unknown []*helmInstall) {
	p.printf("Helm does not record which repository a chart came from, so `<repo>` is\n" +
		"for you to fill in. What the charts say about themselves:\n\n")
	// writing each release's hints, its home page and sources
	for _, r := range unknown {
		var hints []string
		// checking if the chart names a home page
		if h := r.meta.GetHome(); h != "" {
			hints = append(hints, "home "+h)
		}
		// adding each source the chart names
		for _, src := range r.meta.GetSources() {
			hints = append(hints, "source "+src)
		}
		// checking if the chart says nothing, if so the line says so
		if len(hints) == 0 {
			hints = append(hints, "nothing")
		}
		p.printf("- %s: %s\n", r.name, strings.Join(hints, ", "))
	}
	// a ready-to-fill --helm-repos file, one entry per chart
	var charts []string
	// collecting each chart once, or the release when the chart is unknown
	for _, r := range unknown {
		key := r.chart
		// checking if the chart is unknown, if so the entry is for the release
		if key == "" {
			key = r.ns + "/" + r.name
		}
		// checking if the entry isnt listed yet
		if !slices.Contains(charts, key) {
			charts = append(charts, key)
		}
	}
	sort.Strings(charts)
	p.printf("\nTo have the next export write the full commands, fill in this file,\n" +
		"save it at the root of the export repository (outside clusters/, which\n" +
		"each export replaces), and pass it with `setdrift export --helm-repos\n" +
		"<file>`. An entry for a chart covers every release of it;\n" +
		"`<namespace>/<release>` covers one. One file can serve every cluster.\n\n")
	lines := []string{"# chart (or <namespace>/<release>): repository URL, https:// or oci://"}
	// adding a line per entry to fill in
	for _, c := range charts {
		lines = append(lines, fmt.Sprintf("%s: <url>", c))
	}
	p.printf("```yaml\n%s\n```\n\n", strings.Join(lines, "\n"))
	p.printf("Teams that run Helm from scripts or CI can record the repository at\n" +
		"install time instead, with the post-renderer in contrib/helm of the\n" +
		"SetDrift repository.\n\n")

}

// gitopsNote says that argo cd or flux restore what they manage, so those objects need nothing more
func gitopsNote(p *playbook, objects int) {
	// text.Plural is defined in internal/text/text.go
	p.printf("**GitOps.** %s are managed by Argo CD or Flux from Git. The\n"+
		"Applications and Kustomizations are in this export, so once Argo CD or Flux\n"+
		"runs in the new cluster (installed from this export or as you installed\n"+
		"it), it syncs them from Git; the exported copies of what they manage match\n"+
		"what they apply.\n\n", text.Plural(objects, "object", "objects"))

}

// webhookSteps says to wait for the admission webhooks' services, since applying fails until they run
func webhookSteps(p *playbook, webhooks []string) {
	// sorting and dropping repeats, several webhooks can call the same service
	sort.Strings(webhooks)
	webhooks = slices.Compact(webhooks)
	p.printf("**Admission webhooks.** The API server calls these services before it\n" +
		"accepts some objects. Until they run in the new cluster, applying those\n" +
		"objects fails with `failed calling webhook`; when that happens, wait for\n" +
		"them and apply again (the apply is idempotent):\n\n")
	var lines []string
	// building a wait command per service, it waits for the service to have an endpoint
	for _, w := range webhooks {
		ns, svc, _ := strings.Cut(w, "/")
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" -n %s wait --for=jsonpath='{.subsets[0].addresses[0].ip}' endpoints/%s --timeout=300s`,
			text.ShellQuote(ns), text.ShellQuote(svc)))
	}
	p.code(lines...)

}

// chartSource is where argo cd or flux installs a chart from, and which object does it
type chartSource struct{ repo, from string }

// gitopsChartRepos maps chart names to the repository an argo cd Application or a flux HelmRelease installs them from, for helm releases of the same chart installed by hand
// a chart installed from two repositories is left out, since theres no telling which one the hand-installed release used
func gitopsChartRepos(resources []*snapshotv1alpha1.Resource, man *manifests) map[string]chartSource {
	found := chartRepos{} // chart > repo > what installs it
	// for flux, HelmRelease > HelmRepository (spec.chart) or OCIRepository (spec.chartRef)
	sources := map[string]map[string]any{}
	var helmReleases []*snapshotv1alpha1.Resource
	// going through the resources for flux sources, flux HelmReleases and argo cd Applications
	for _, r := range resources {
		// invoking kube.Group for the api group, its defined in internal/kube/kube.go
		g := kube.Group(r.ApiVersion)
		// checking which of the three it is, of and str are defined in manifest.go
		switch {
		case g == "source.toolkit.fluxcd.io" && (r.Kind == "HelmRepository" || r.Kind == "OCIRepository"):
			sources[r.Kind+"/"+r.Namespace+"/"+r.Name] = man.of(r)
		case g == "helm.toolkit.fluxcd.io" && r.Kind == "HelmRelease":
			helmReleases = append(helmReleases, r)
		case g == "argoproj.io" && r.Kind == "Application":
			m := man.of(r)
			from := fmt.Sprintf("Argo CD Application %s/%s", r.Namespace, r.Name)
			// invoking add for the single source, its defined below
			found.add(str(m, "spec", "source", "chart"), str(m, "spec", "source", "repoURL"), from)
			list, _, _ := unstructured.NestedSlice(m, "spec", "sources")
			// going through the multi-source list too
			for _, item := range list {
				// checking if the item is a map, if so its chart and repo are added
				if src, ok := item.(map[string]any); ok {
					found.add(str(src, "chart"), str(src, "repoURL"), from)
				}
			}
		}
	}
	// invoking addFlux for each flux HelmRelease once all sources are known, its defined below
	for _, r := range helmReleases {
		found.addFlux(r, man.of(r), sources)
	}
	out := map[string]chartSource{}
	// keeping only the charts installed from one repository
	for chart, repos := range found {
		// checking if theres exactly one repository
		if len(repos) == 1 {
			// taking that one entry out of the map
			for repo, from := range repos {
				out[chart] = chartSource{repo, from}
			}
		}
	}
	return out
}

// chartRepos holds, by chart, each repository something installs it from, and what installs it
type chartRepos map[string]map[string]string

// add records that from installs chart from repo, if repo is a valid repository url
func (c chartRepos) add(chart, repo, from string) {
	// invoking helmsource.CheckRepoURL and checking if the chart is empty or the url isnt valid, if so nothing is recorded, its defined in pkg/helmsource/helmsource.go
	if chart == "" || helmsource.CheckRepoURL(repo) != nil {
		return
	}
	repo = strings.TrimSuffix(repo, "/")
	// checking if this is the chart's first repository
	if c[chart] == nil {
		c[chart] = map[string]string{}
	}
	// checking if the repository isnt recorded yet, if so the first one to install from it is kept
	if _, ok := c[chart][repo]; !ok {
		c[chart][repo] = from
	}
}

// addFlux records the chart and repository of a flux HelmRelease (m), from the HelmRepository or OCIRepository it names in sources
func (c chartRepos) addFlux(r *snapshotv1alpha1.Resource, m map[string]any, sources map[string]map[string]any) {
	from := fmt.Sprintf("Flux HelmRelease %s/%s", r.Namespace, r.Name)
	// ref finds a source by kind and name, in the HelmRelease's own namespace when it names none
	ref := func(kind, name, ns string) map[string]any {
		return sources[kind+"/"+cmp.Or(ns, r.Namespace)+"/"+name]
	}
	// checking if the release names its chart in spec.chart, if so the repository comes from its sourceRef
	if chart := str(m, "spec", "chart", "spec", "chart"); chart != "" {
		// checking if the source it names is in the snapshot, a sourceRef without a kind means HelmRepository
		if src := ref(cmp.Or(str(m, "spec", "chart", "spec", "sourceRef", "kind"), "HelmRepository"),
			str(m, "spec", "chart", "spec", "sourceRef", "name"), str(m, "spec", "chart", "spec", "sourceRef", "namespace")); src != nil {
			// invoking add with the source's url, its defined above
			c.add(chart, str(src, "spec", "url"), from)
		}
		return
	}
	// checking if the release points to anything but an OCIRepository, if so theres nothing to record
	if str(m, "spec", "chartRef", "kind") != "OCIRepository" {
		return
	}
	// the OCIRepository's url is the chart itself, oci://<registry>/<path>/<chart>
	// checking if the OCIRepository it names is in the snapshot
	if src := ref("OCIRepository", str(m, "spec", "chartRef", "name"), str(m, "spec", "chartRef", "namespace")); src != nil {
		// checking if the url has a path, if so its last part is the chart and the rest the repository
		if url := strings.TrimSuffix(str(src, "spec", "url"), "/"); strings.Contains(url, "/") {
			i := strings.LastIndex(url, "/")
			c.add(url[i+1:], url[:i], from)
		}
	}
}

// chartOf splits a helm.sh/chart label, <chart>-<version>, into the chart and its version
func chartOf(label string) (chart, version string) {
	// trying each dash from the right, the first one followed by a digit starts the version
	for i := strings.LastIndex(label, "-"); i > 0; i = strings.LastIndex(label[:i], "-") {
		// checking if a digit follows this dash
		if i+1 < len(label) && label[i+1] >= '0' && label[i+1] <= '9' {
			return label[:i], label[i+1:]
		}
	}
	return label, ""
}

// helmRelease gives back "<namespace>/<release>" of the helm release that installed r, or "" if helm didnt (or only the low-confidence label says so)
func helmRelease(r *snapshotv1alpha1.Resource) string {
	p := r.GetProvenance()
	// checking if helm didnt install it, or only the low-confidence label says so
	if p.GetSource() != snapshotv1alpha1.Source_SOURCE_HELM || p.GetEvidence()["confidence"] == "low" {
		return ""
	}
	return p.GetManager()
}

// webhookServices gives back "<namespace>/<service>" of the services a webhook configuration calls
func webhookServices(m map[string]any) []string {
	webhooks, _, _ := unstructured.NestedSlice(m, "webhooks")
	var out []string
	// going through the configuration's webhooks
	for _, item := range webhooks {
		w, _ := item.(map[string]any)
		// checking if the webhook calls a service, a webhook can call a url instead
		if name := str(w, "clientConfig", "service", "name"); name != "" {
			out = append(out, str(w, "clientConfig", "service", "namespace")+"/"+name)
		}
	}
	return out
}

// manager names who installs r, for RESTORE.md's tables
func manager(r *snapshotv1alpha1.Resource) string {
	p := r.GetProvenance()
	// the source's enum name lowercased is the fallback, like operator or platform
	src := strings.ToLower(strings.TrimPrefix(p.GetSource().String(), "SOURCE_"))
	// checking which source it is, the known ones get their proper names
	switch p.GetSource() {
	case snapshotv1alpha1.Source_SOURCE_HELM:
		src = "Helm"
	case snapshotv1alpha1.Source_SOURCE_ARGO_CD:
		src = "Argo CD"
	case snapshotv1alpha1.Source_SOURCE_FLUX:
		src = "Flux"
	case snapshotv1alpha1.Source_SOURCE_CI_CD:
		src = "CI/CD"
	case snapshotv1alpha1.Source_SOURCE_UNMANAGED, snapshotv1alpha1.Source_SOURCE_UNSPECIFIED:
		return "applied by hand"
	}
	// checking if the manager is known, like the release or application name, if so its added
	if m := p.GetManager(); m != "" {
		return src + " " + m
	}
	return src
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
