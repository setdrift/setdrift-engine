// RESTORE.md's controllers step: Helm releases, GitOps tools and admission
// webhooks to have in place before the rest is applied.

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

// controllers: Helm releases, GitOps tools and admission webhooks that must
// be in place before the rest is applied.
// helmInstall is a Helm release to install before the rest, as the export
// shows it.
type helmInstall struct {
	ns, name, chart, version string
	objects                  int
	meta                     *snapshotv1alpha1.HelmRelease
}

// controllers writes the step for what must run before the rest is applied:
// Helm releases, GitOps tools and admission webhooks.
func (d *restoreData) controllers(p *playbook) {
	releases, gitopsObjects, webhooks := d.gatherControllers()
	if len(releases) == 0 && gitopsObjects == 0 && len(webhooks) == 0 {
		return
	}
	p.section("Controllers to install first")
	if len(releases) > 0 {
		d.helmSteps(p, releases)
	}
	if gitopsObjects > 0 {
		gitopsNote(p, gitopsObjects)
	}
	if len(webhooks) > 0 {
		webhookSteps(p, webhooks)
	}
}

// gatherControllers returns the Helm releases the written objects belong
// to, how many objects Argo CD or Flux manage, and the services admission
// webhooks call.
func (d *restoreData) gatherControllers() (releases map[string]*helmInstall, gitopsObjects int, webhooks []string) {
	releases = map[string]*helmInstall{}
	for _, w := range d.written {
		r := w.resource
		switch r.GetProvenance().GetSource() {
		case snapshotv1alpha1.Source_SOURCE_HELM:
			m := helmRelease(r)
			if m == "" {
				break
			}
			rel := releases[m]
			if rel == nil {
				ns, name, _ := strings.Cut(m, "/")
				rel = &helmInstall{ns: ns, name: name}
				releases[m] = rel
			}
			rel.objects++
			if rel.chart == "" {
				rel.chart, rel.version = chartOf(labels(d.manifests.of(r))["helm.sh/chart"])
			}
		case snapshotv1alpha1.Source_SOURCE_ARGO_CD, snapshotv1alpha1.Source_SOURCE_FLUX:
			gitopsObjects++
		}
		if r.Kind == "MutatingWebhookConfiguration" || r.Kind == "ValidatingWebhookConfiguration" {
			webhooks = append(webhooks, webhookServices(d.manifests.of(r))...)
		}
	}
	for _, h := range d.s.GetInventory().GetHelmReleases() {
		if rel := releases[h.Namespace+"/"+h.Name]; rel != nil {
			rel.meta = h
			if h.Chart != "" {
				rel.chart, rel.version = h.Chart, h.ChartVersion
			}
		}
	}
	return releases, gitopsObjects, webhooks
}

// helmSteps writes the Helm releases to install: a table, the commands, and
// what is left for the user to fill in.
func (d *restoreData) helmSteps(p *playbook, releases map[string]*helmInstall) {
	p.printf("**Helm releases.** Their objects are in this export, but Helm's release\n" +
		"records are not, so applying the export would leave them unmanaged by Helm.\n" +
		"Install each release with Helm before applying the rest; the exported\n" +
		"copies then match and change nothing. Take the values from the source;\n" +
		"they can hold secrets, so the commands keep them in a temporary file\n" +
		"outside the export:\n\n")
	p.printf("| Release | Namespace | Chart | Objects |\n|---|---|---|---|\n")
	keys := sortedMapKeys(releases)
	for _, k := range keys {
		r := releases[k]
		chart := r.chart
		if chart == "" {
			chart = "unknown"
		} else if r.version != "" {
			chart += " " + r.version
		}
		p.printf("| %s | %s | %s | %d |\n", cell(r.name), cell(r.ns), cell(chart), r.objects)
	}
	p.printf("\n")
	lines, borrowed, noChart, unknown := d.helmInstallCommands(releases, keys)
	p.code(lines...)
	if len(borrowed) > 0 {
		p.printf("Helm does not record which repository a chart came from; these are\n"+
			"taken from Argo CD or Flux installing the same chart in this cluster.\n"+
			"Check them, or name the repository with `--helm-repos`:\n\n%s\n\n", strings.Join(borrowed, "\n"))
	}
	if len(noChart) > 0 {
		chartsToFind(p, noChart)
	}
	if len(unknown) > 0 {
		reposToFind(p, unknown)
	}
}

// helmInstallCommands returns, for each release in keys' order, the
// commands that copy its values from the source and install it; the
// repositories taken from Argo CD or Flux; and the releases whose chart or
// repository is unknown.
func (d *restoreData) helmInstallCommands(releases map[string]*helmInstall, keys []string) (lines, borrowed []string, noChart, unknown []*helmInstall) {
	gitops := gitopsChartRepos(d.s.GetResources(), &d.manifests)
	for _, k := range keys {
		r := releases[k]
		// the chart's name comes from a Helm release record or a label: quoted
		chartName := text.ShellQuote(r.chart)
		if r.chart == "" {
			chartName = "<chart>"
			noChart = append(noChart, r)
		}
		// helm install <release> <chart ref> [--repo <url>]
		chart := "<repo>/" + chartName
		// the --helm-repos file, then the post-renderer's annotation, then Argo CD or Flux installing the same chart
		repo := d.helmRepos.Lookup(r.ns, r.name, r.chart, r.meta.GetRepository())
		if g, ok := gitops[r.chart]; ok && repo == "" && r.chart != "" {
			repo = g.repo
			borrowed = append(borrowed, fmt.Sprintf("- %s: %s, as %s installs it", r.name, g.repo, g.from))
		}
		switch {
		case strings.HasPrefix(repo, "oci://"):
			chart = text.ShellQuote(strings.TrimSuffix(repo, "/") + "/" + cmp.Or(r.chart, "<chart>"))
		case repo != "":
			chart = chartName + " --repo " + text.ShellQuote(repo)
		default:
			unknown = append(unknown, r)
		}
		// Outside the export, so the values (which can hold secrets) never reach Git.
		values := `"${TMPDIR:-/tmp}/setdrift-` + SafeName(r.ns) + "-" + SafeName(r.name) + `-values.yaml"`
		install := fmt.Sprintf(`helm --kube-context "$DST" install %s %s -n %s --create-namespace -f %s`,
			text.ShellQuote(r.name), chart, text.ShellQuote(r.ns), values)
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

// chartsToFind says how to find the charts of releases the export has no
// record of.
func chartsToFind(p *playbook, noChart []*helmInstall) {
	p.printf("The export has no Helm release record for these (Helm keeps it in a Secret\n" +
		"in the release's namespace, which --from-dir input lacks), and their objects\n" +
		"have no helm.sh/chart label, so `<chart>` is for you to fill in. The CHART\n" +
		"column on the source shows it:\n\n")
	var cmds []string
	for _, r := range noChart {
		cmds = append(cmds, fmt.Sprintf(`helm --kube-context "$SRC" list -n %s --filter %s`, text.ShellQuote(r.ns), text.ShellQuote("^"+regexp.QuoteMeta(r.name)+"$")))
	}
	p.code(cmds...)

}

// reposToFind lists what the charts of releases with an unknown repository
// say about themselves, and a --helm-repos file to fill in.
func reposToFind(p *playbook, unknown []*helmInstall) {
	p.printf("Helm does not record which repository a chart came from, so `<repo>` is\n" +
		"for you to fill in. What the charts say about themselves:\n\n")
	for _, r := range unknown {
		var hints []string
		if h := r.meta.GetHome(); h != "" {
			hints = append(hints, "home "+h)
		}
		for _, src := range r.meta.GetSources() {
			hints = append(hints, "source "+src)
		}
		if len(hints) == 0 {
			hints = append(hints, "nothing")
		}
		p.printf("- %s: %s\n", r.name, strings.Join(hints, ", "))
	}
	// a ready-to-fill --helm-repos file, one entry per chart
	var charts []string
	for _, r := range unknown {
		key := r.chart
		if key == "" {
			key = r.ns + "/" + r.name
		}
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
	for _, c := range charts {
		lines = append(lines, fmt.Sprintf("%s: <url>", c))
	}
	p.printf("```yaml\n%s\n```\n\n", strings.Join(lines, "\n"))
	p.printf("Teams that run Helm from scripts or CI can record the repository at\n" +
		"install time instead, with the post-renderer in contrib/helm of the\n" +
		"SetDrift repository.\n\n")

}

// gitopsNote says that Argo CD or Flux restore what they manage.
func gitopsNote(p *playbook, objects int) {
	p.printf("**GitOps.** %s are managed by Argo CD or Flux from Git. The\n"+
		"Applications and Kustomizations are in this export, so once Argo CD or Flux\n"+
		"runs in the new cluster (installed from this export or as you installed\n"+
		"it), it syncs them from Git; the exported copies of what they manage match\n"+
		"what they apply.\n\n", text.Plural(objects, "object", "objects"))

}

// webhookSteps says to wait for the admission webhooks' services.
func webhookSteps(p *playbook, webhooks []string) {
	sort.Strings(webhooks)
	webhooks = slices.Compact(webhooks)
	p.printf("**Admission webhooks.** The API server calls these services before it\n" +
		"accepts some objects. Until they run in the new cluster, applying those\n" +
		"objects fails with `failed calling webhook`; when that happens, wait for\n" +
		"them and apply again (the apply is idempotent):\n\n")
	var lines []string
	for _, w := range webhooks {
		ns, svc, _ := strings.Cut(w, "/")
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" -n %s wait --for=jsonpath='{.subsets[0].addresses[0].ip}' endpoints/%s --timeout=300s`,
			text.ShellQuote(ns), text.ShellQuote(svc)))
	}
	p.code(lines...)

}

// chartSource is where Argo CD or Flux installs a chart from.
type chartSource struct{ repo, from string }

// gitopsChartRepos maps chart names to the repository an Argo CD Application or a Flux HelmRelease installs them
// from, for Helm releases of the same chart installed by hand. A chart installed from two repositories is left out.
func gitopsChartRepos(resources []*snapshotv1alpha1.Resource, man *manifests) map[string]chartSource {
	found := chartRepos{} // chart > repo > what installs it
	// Flux: HelmRelease > HelmRepository (spec.chart) or OCIRepository (spec.chartRef)
	sources := map[string]map[string]any{}
	var helmReleases []*snapshotv1alpha1.Resource
	for _, r := range resources {
		g := kube.Group(r.ApiVersion)
		switch {
		case g == "source.toolkit.fluxcd.io" && (r.Kind == "HelmRepository" || r.Kind == "OCIRepository"):
			sources[r.Kind+"/"+r.Namespace+"/"+r.Name] = man.of(r)
		case g == "helm.toolkit.fluxcd.io" && r.Kind == "HelmRelease":
			helmReleases = append(helmReleases, r)
		case g == "argoproj.io" && r.Kind == "Application":
			m := man.of(r)
			from := fmt.Sprintf("Argo CD Application %s/%s", r.Namespace, r.Name)
			found.add(str(m, "spec", "source", "chart"), str(m, "spec", "source", "repoURL"), from)
			list, _, _ := unstructured.NestedSlice(m, "spec", "sources")
			for _, item := range list {
				if src, ok := item.(map[string]any); ok {
					found.add(str(src, "chart"), str(src, "repoURL"), from)
				}
			}
		}
	}
	for _, r := range helmReleases {
		found.addFlux(r, man.of(r), sources)
	}
	out := map[string]chartSource{}
	for chart, repos := range found {
		if len(repos) == 1 {
			for repo, from := range repos {
				out[chart] = chartSource{repo, from}
			}
		}
	}
	return out
}

// chartRepos is, by chart, each repository something installs it from, and
// what.
type chartRepos map[string]map[string]string

// add records that from installs chart from repo, a valid repository URL.
func (c chartRepos) add(chart, repo, from string) {
	if chart == "" || helmsource.CheckRepoURL(repo) != nil {
		return
	}
	repo = strings.TrimSuffix(repo, "/")
	if c[chart] == nil {
		c[chart] = map[string]string{}
	}
	if _, ok := c[chart][repo]; !ok {
		c[chart][repo] = from
	}
}

// addFlux records the chart and repository of a Flux HelmRelease (m), from
// the HelmRepository or OCIRepository it names in sources.
func (c chartRepos) addFlux(r *snapshotv1alpha1.Resource, m map[string]any, sources map[string]map[string]any) {
	from := fmt.Sprintf("Flux HelmRelease %s/%s", r.Namespace, r.Name)
	ref := func(kind, name, ns string) map[string]any {
		return sources[kind+"/"+cmp.Or(ns, r.Namespace)+"/"+name]
	}
	if chart := str(m, "spec", "chart", "spec", "chart"); chart != "" {
		if src := ref(cmp.Or(str(m, "spec", "chart", "spec", "sourceRef", "kind"), "HelmRepository"),
			str(m, "spec", "chart", "spec", "sourceRef", "name"), str(m, "spec", "chart", "spec", "sourceRef", "namespace")); src != nil {
			c.add(chart, str(src, "spec", "url"), from)
		}
		return
	}
	if str(m, "spec", "chartRef", "kind") != "OCIRepository" {
		return
	}
	// The OCIRepository's URL is the chart itself: oci://<registry>/<path>/<chart>
	if src := ref("OCIRepository", str(m, "spec", "chartRef", "name"), str(m, "spec", "chartRef", "namespace")); src != nil {
		if url := strings.TrimSuffix(str(src, "spec", "url"), "/"); strings.Contains(url, "/") {
			i := strings.LastIndex(url, "/")
			c.add(url[i+1:], url[:i], from)
		}
	}
}

// chartOf returns the chart and version from a helm.sh/chart label,
// <chart>-<version>.
func chartOf(label string) (chart, version string) {
	for i := strings.LastIndex(label, "-"); i > 0; i = strings.LastIndex(label[:i], "-") {
		if i+1 < len(label) && label[i+1] >= '0' && label[i+1] <= '9' {
			return label[:i], label[i+1:]
		}
	}
	return label, ""
}

// helmRelease returns "<namespace>/<release>" of the Helm release that
// installed r, or "" if Helm did not (or only the low-confidence label says
// so).
func helmRelease(r *snapshotv1alpha1.Resource) string {
	p := r.GetProvenance()
	if p.GetSource() != snapshotv1alpha1.Source_SOURCE_HELM || p.GetEvidence()["confidence"] == "low" {
		return ""
	}
	return p.GetManager()
}

// webhookServices returns "<namespace>/<service>" of the services a webhook
// configuration calls.
func webhookServices(m map[string]any) []string {
	webhooks, _, _ := unstructured.NestedSlice(m, "webhooks")
	var out []string
	for _, item := range webhooks {
		w, _ := item.(map[string]any)
		if name := str(w, "clientConfig", "service", "name"); name != "" {
			out = append(out, str(w, "clientConfig", "service", "namespace")+"/"+name)
		}
	}
	return out
}

// manager names who installs r, for RESTORE.md's tables.
func manager(r *snapshotv1alpha1.Resource) string {
	p := r.GetProvenance()
	src := strings.ToLower(strings.TrimPrefix(p.GetSource().String(), "SOURCE_"))
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
	if m := p.GetManager(); m != "" {
		return src + " " + m
	}
	return src
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
