# Naming a Helm release's chart repository

Helm records each release's chart name and version in the cluster, but not
the repository the chart was installed from. Without it, RESTORE.md writes
`helm install <release> <repo>/<chart>` and leaves `<repo>` for you to fill
in, with the chart's `home` and `sources` links as hints. There are two ways
to give SetDrift the repository.

Charts that Argo CD or Flux install need neither: those tools record the
repository in their own Application or HelmRepository objects, and RESTORE.md
reinstalls them through the tool, not with `helm install`.

## At export time: `--helm-repos` (recommended)

List the repositories in a YAML file and pass it to the export. Nothing in
the cluster or in how you run Helm changes, so it works however the
releases were installed: by hand, from scripts, from many places.

```yaml
# helm-repos.yaml: <chart> covers every release of the chart,
# <namespace>/<release> covers one (and wins over a <chart> entry)
ingress-nginx: https://kubernetes.github.io/ingress-nginx
kube-prometheus-stack: https://prometheus-community.github.io/helm-charts
external-secrets: https://charts.external-secrets.io
payments/internal-api: oci://registry.example.com/charts
```

```sh
setdrift export -o ./prod-export --helm-repos ./prod-export/helm-repos.yaml
```

- RESTORE.md lists the charts it doesn't know a repository for, as a file
  ready to fill in: export once, copy it, add the URLs, export again.
- URLs must start with `https://`, `http://` or `oci://` (for OCI, the
  registry path the chart is under, as in `helm install <release>
  oci://registry.example.com/charts/<chart>`). URLs with a username or
  password are refused, since RESTORE.md is meant for Git: log in with
  `helm repo add --username` or `helm registry login` instead.
- Save the file at the root of the export repository, next to `clusters/`,
  and commit it with the export. Not inside `clusters/<cluster>/`: each
  export replaces that directory, file included.
- One file can serve every cluster in the repository, since a chart comes
  from the same repository wherever it runs.
- A `<namespace>/<release>` entry that matches no release gets a warning
  (a typo, or a release since removed). Chart entries don't, since a shared
  file names charts only some clusters run; a mistyped chart shows up as a
  chart RESTORE.md still asks a repository for.
- To check: RESTORE.md's `helm install` commands now have `--repo <url>` or
  an `oci://` chart path, and `grep -c '<repo>' prod-export/clusters/*/RESTORE.md`
  prints 0.

## At install time: the post-renderer

For teams that already run Helm from scripts or CI, a post-renderer can
record the repository on the release's objects, as the annotation
`setdrift.io/source-repo: <url>`, at every install or upgrade. Each export
then reads it from the cluster with no file to maintain. An entry in a
`--helm-repos` file for the same release still wins.

It needs `setdrift` or `kubectl-setdrift` on PATH wherever Helm runs; the
plugin's scripts run whichever is there.

Helm 4 (post-renderers are plugins):

```sh
# from a checkout of this repository:
helm plugin install contrib/helm/setdrift-source-repo
# or package it once and install the archive on other machines, e.g. CI runners:
make helm-plugin    # bin/setdrift-source-repo.tgz
helm plugin install setdrift-source-repo.tgz

helm install ingress-nginx ingress-nginx --repo https://kubernetes.github.io/ingress-nginx \
  --post-renderer setdrift-source-repo \
  --post-renderer-args --source-repo=https://kubernetes.github.io/ingress-nginx
```

Helm 3.10 or later (`--post-renderer` takes an executable):

```sh
helm install ingress-nginx ingress-nginx --repo https://kubernetes.github.io/ingress-nginx \
  --post-renderer ./contrib/helm/setdrift-source-repo/postrender.sh \
  --post-renderer-args --source-repo=https://kubernetes.github.io/ingress-nginx
```

Existing releases get the annotation at their next `helm upgrade` with the
post-renderer. To check: the release's objects carry the annotation
(`kubectl get <kind> <name> -n <namespace> -o yaml`), and the next export's
RESTORE.md has `--repo <url>` for the release.

If Helm only says the plugin "exited with error" (Helm 4 does not show the
plugin's message), run it by hand to see why, e.g. a URL it refused or
SetDrift missing from PATH:

```sh
setdrift helm-postrender --source-repo=<url> < /dev/null    # or kubectl-setdrift
```

Chart authors can get the same annotation without a post-renderer by
rendering it from a value, e.g. `global.sourceRepo`, into every object's
metadata.annotations.
