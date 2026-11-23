# setdrift helm-postrender

A Helm post-renderer that records which chart repository a release came from. Helm passes it the manifests it rendered, and it writes them back with the annotation `setdrift.io/chart-repo=<url>` on every object. Helm doesn't record a chart's repository, so with this annotation each later [`export`](export.md) finds it in the cluster and RESTORE.md can write the exact `helm install` command.

Most teams don't need it. `export --helm-repos <file>` names the repositories at export time, with no change to how Helm runs. This is for teams that run Helm from scripts or CI and want every export to find the repository without a file to keep up. [contrib/helm](https://github.com/setdrift/setdrift-engine/tree/main/contrib/helm) has the full setup.

## Synopsis

```sh
setdrift helm-postrender --chart-repo <url> [flags]
```

## Inputs

It reads the rendered manifests on standard input and writes them to standard output, as Helm runs every post-renderer. You don't run it yourself except to check it.

| Helm | How | Example |
|---|---|---|
| Helm 4 | Install the plugin from `contrib/helm`, then name it with `--post-renderer` | `helm install web ./chart --post-renderer setdrift-chart-repo --post-renderer-args --chart-repo=https://charts.example.com` |
| Helm 3.10 or later | Pass the plugin's script as the post-renderer | `helm install web ./chart --post-renderer ./contrib/helm/setdrift-chart-repo/postrender.sh --post-renderer-args --chart-repo=https://charts.example.com` |

It needs `setdrift` or `kubectl-setdrift` on `PATH` wherever Helm runs. Helm 4 is tested by hand on macOS, Helm 3 and Windows are not yet tested.

## Examples

Check it by hand, since Helm 4 only says a plugin "exited with error".

```sh
setdrift helm-postrender --chart-repo=https://charts.example.com < /dev/null
```

What it does to rendered manifests.

```sh
helm template web ./chart | setdrift helm-postrender --chart-repo oci://registry.example.com/charts
```

## Flags

### `--chart-repo`

*text · required*

The chart repository the release was installed from, starting with `https://`, `http://` or `oci://`. For OCI it's the registry path the chart is under, as in `helm install <release> oci://registry.example.com/charts/<chart>`. A URL with a username or password is refused, since the annotation ends up in Git through the export. Log in with `helm repo add --username` or `helm registry login` instead.

```sh
setdrift helm-postrender --chart-repo https://kubernetes.github.io/ingress-nginx < rendered.yaml
```

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

## Output

The same manifests, each with `metadata.annotations["setdrift.io/chart-repo"]` set to the URL, and nothing else changed. An entry in a `--helm-repos` file for the same release still wins when exporting.

## See also

- [contrib/helm](https://github.com/setdrift/setdrift-engine/tree/main/contrib/helm), installing the plugin and checking it worked
- [`export`](export.md), `--helm-repos`
