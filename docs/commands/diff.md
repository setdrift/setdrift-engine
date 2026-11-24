# setdrift diff

Compares two clusters, or one cluster with an earlier snapshot of itself, and says what differs and what it can tell about why. For each difference it shows who manages the resource on each side, whether a Helm chart version differs, and which differences are only each cluster's own values (its name, region, account). It only reads, and neither side is changed.

Each comparison answers one of two questions by holding one thing fixed (see [Choose a comparison](../which-comparison.md)).

- **Across locations -** How do `east` and `west` differ right now? Before a release, for DR readiness, or between blue and green.
- **Across time -** What changed in `prod` since Monday's snapshot, and who changed it?

To check whether an export applies cleanly to a cluster, use [`verify`](verify.md) instead.

## Synopsis

```sh
setdrift diff A B [flags]
```

## Inputs

`A` and `B` are each a kubeconfig context, a snapshot file or a directory of manifests, in any combination. `diff` decides which from the argument itself.

| Argument | Read as | Example |
|---|---|---|
| `context:NAME` | Always the kubeconfig context `NAME`, read live, `context:arn:aws:eks:...` too | `setdrift diff context:east context:west` |
| A path, one with `/` or `\`, or ending in `.pb`, `.json`, `.yaml` or `.yml` | Always that file or directory, and an error if it does not exist | `setdrift diff ./snapshots/2026-11-16T090000Z-prod.pb context:prod` |
| A bare name that is a file or directory here | That file or directory | `setdrift diff prod-export staging-export` |
| A bare name that is a kubeconfig context | The context, read live | `setdrift diff east west` |
| A bare name that is **both** | An error that says to write `./prod` for the directory or `context:prod` for the cluster | `setdrift diff ./prod context:prod` |
| A bare name that is neither | An error listing the kubeconfig's contexts | |

Exports are often named after their cluster (`setdrift export --context prod -o prod`), so in that directory `prod` would be ambiguous. Rather than pick one silently, `diff` asks. Both arguments are checked before either is read, and every problem is reported at once.

- **A context** is read the way `export` reads it, with the same [permissions](../permissions/kubernetes.md). Two contexts is the same as exporting both now and comparing the snapshots.
- **A snapshot file** is one `export` saved in its `snapshots/` directory, `.pb` or `.json`. Its capture time is shown, so you can tell how old it is.
- **A directory** is read as manifests, like `export --from-dir`, either an export's cluster directory or a folder of YAML. Its records (`snapshots/`, `verify/`, `cluster_profile/`) are left out.
- **The same argument twice** compares one cluster or snapshot with itself, which needs `--map-namespace` to pair two of its namespaces.

### Finding your contexts

A context is a name in your kubeconfig for one cluster and the credentials you reach it with. To see them.

```sh
kubectl config get-contexts           # every context, the current one marked with *
kubectl config current-context        # the one kubectl and setdrift use by default
```

`diff` reads contexts from one kubeconfig, `--kubeconfig` if given, else the `KUBECONFIG` variable, else `~/.kube/config`. Name the file, then the contexts in it.

```sh
setdrift diff east west --kubeconfig ~/.kube/multi-region
```

Contexts in two files can be merged in `KUBECONFIG`, separated by `:` (`;` on Windows).

```sh
KUBECONFIG=~/.kube/config:~/.kube/other setdrift diff staging prod
```

A cluster with no context yet gets one from its cloud's CLI, for example `aws eks update-kubeconfig --name prod --alias prod`, `gcloud container clusters get-credentials prod` or `az aks get-credentials --name prod --resource-group platform`.

### What counts as a difference

Both sides are normalized the way `export` normalizes them, so server defaults, status and secret values never show up as differences, and Secrets compare by their keys. Each platform's own objects (EKS's, kind's) are not compared. [What counts as a difference](../which-comparison.md#what-counts-as-a-difference) has the details, like why the same ECR image in two regions matches.

## Examples

Two clusters, by their kubeconfig contexts.

```sh
setdrift diff east west
```

```
Comparing east with west

2 in both: 1 identical, 1 differ
Only in east: 0   Only in west: 1

Differ (1):
  shop/web Deployment.apps  (applied by hand)
      spec.replicas: 3 -> 5
      spec.template.spec.containers[web].image: "ghcr.io/example/web:1.4.2" -> "ghcr.io/example/web:1.4.3"

Only in west (1):
  shop, applied by hand: ConfigMap feature-flags
```

What changed in a cluster since an earlier export, and who changed it.

```sh
setdrift diff setdrift-export/snapshots/2026-11-16T090000Z-prod.pb prod
```

One namespace, named differently on each side.

```sh
setdrift diff staging prod -n shop-staging --map-namespace shop-staging=shop
```

Two namespaces of one cluster, a canary against the main release.

```sh
setdrift diff prod prod --map-namespace shop=shop-canary
```

An export directory against the cluster it came from, when the directory has the cluster's name.

```sh
setdrift diff ./prod context:prod
```

In CI, fail the job when two clusters differ beyond their own values.

```sh
setdrift diff context:east context:west --exit-code
```

Every field, for a script or a report.

```sh
setdrift diff east west --format json
```

## Flags

### `--namespace`, `-n`

*list · default none*

Compares only these namespaces, named as `A` names them, and leaves out cluster-scoped resources (ClusterRoles, CRDs, StorageClasses) except the Namespace objects themselves. Repeat it or separate names with commas, as in `-n shop -n billing` or `-n shop,billing`.

With `--map-namespace`, `B` reads each mapped namespace by its own name, so `-n shop-staging --map-namespace shop-staging=shop` reads `shop-staging` in `A` and `shop` in `B`. For a live side only those namespaces are read, and for a snapshot or a directory the rest is set aside.

```sh
setdrift diff east west -n shop -n billing
```

### `--map-namespace`

*list · default none*

Pairs namespace `A_NS` in `A` with `B_NS` in `B`, written `A_NS=B_NS`, so their resources are compared with each other instead of each showing as in one side only. Repeat it for more pairs. A mapped namespace's name is also set apart as one of the cluster's own values, so a ConfigMap holding `shop-staging.svc` on one side and `shop.svc` on the other matches.

Each namespace in a pair must be on its side. A typo is an error that lists the namespaces the side has, rather than a comparison with nothing paired.

Comparing a cluster or snapshot with itself needs at least one pair, and with no `-n` only the mapped namespaces are read on each side.

```sh
setdrift diff staging prod --map-namespace shop-staging=shop --map-namespace billing-staging=billing
setdrift diff prod prod --map-namespace shop=shop-canary
```

### `--map-value`

*list · default none*

Sets apart a value that each cluster has its own of, written `A_VALUE=B_VALUE`, so fields differing only in it count as the clusters' own values, like their names and regions. Use it for what holds none of the cluster's name, region or account, like a registry host or a domain. Repeat it for more pairs.

Each value must be at least 3 characters, so a short one doesn't match inside other words. A value matches only with a character other than a letter or digit, or the string's edge, on each side, so `shop` doesn't match inside `shopping`.

```sh
setdrift diff east west --map-value harbor-east.acme.com=harbor-west.acme.com
setdrift diff staging prod --map-value staging.acme.com=acme.com --map-value mirror-staging=mirror
```

### `--format`

*text · default `text`*

`text` prints the summary shown above, for people. `json` prints every field of the comparison, for scripts and reports, with each side (its cluster, platform, region, account and capture time), each resource that differs with every differing field, those only in one side, and those that differ only in each cluster's own values. Anything else is an error.

```sh
setdrift diff east west --format json > diff.json
```

### `--exit-code`

*on/off · default off*

Sets the exit status by the result, for CI.

| Status | Means |
|---|---|
| `0` | The sides match, apart from each cluster's own values |
| `1` | They differ beyond those |
| `2` | An error, like an argument that can't be read or a refused read |

Without it, `diff` exits `0` whatever it finds, and `1` on an error. Differences only in each cluster's own values (its name, region, account, a mapped namespace or a mapped value) never count.

```sh
setdrift diff context:east context:west --exit-code || echo "east and west have drifted"
```

### `--max-fields`

*number · default `5`*

How many differing fields the text output shows for each resource. The rest are counted (`... 3 more`), and `--format json` always has every field.

```sh
setdrift diff east west --max-fields 20
```

### `--kubeconfig`

*text · default none*

The kubeconfig file the contexts come from. Without it the `KUBECONFIG` variable is used, then `~/.kube/config`, and a path may start with `~`. Both contexts come from the same kubeconfig, see [Finding your contexts](#finding-your-contexts) for contexts in two files.

```sh
setdrift diff east west --kubeconfig ~/.kube/multi-region
```

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

## Output

The text output names both sides, counts what is in both (identical or not) and what is only in one, then lists these.

- **Differ -** Each resource that differs beyond the clusters' own values, with who manages it on each side and up to `--max-fields` fields as `path: A -> B`. Secret values show as `(redacted)`.
- **Only in A, Only in B -** By namespace and manager. A resource `helm template | kubectl apply` deployed is applied by hand, see [Pipeline resources showing as unmanaged](../pipeline-integration/unmanaged-pipeline-resources.md).
- **Differ only in each cluster's own values -** Resources whose every difference is a cluster's name, region, account or mapped namespace.
- **Nodes -** The shape of each side's nodes (count, instance types, zones), shown but never counted as a difference.

When one side read less, some namespaces or a type it could not list, the other is limited to the same, so nothing shows as in one side only for not having been read. What was left out is named.

## Permissions

For each live side, what [`export`](export.md) needs, `get` and `list` on what it reads. A snapshot or a directory needs none. Every verb is in [Kubernetes permissions](../permissions/kubernetes.md).

## See also

- [Choose a comparison](../which-comparison.md), when to compare across time or across locations
- [`export`](export.md), which saves the snapshots `diff` reads
- [`verify`](verify.md), whether an export applies cleanly to a cluster
- [Snapshots](../snapshots.md)
