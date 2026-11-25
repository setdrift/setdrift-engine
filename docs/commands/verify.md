# setdrift verify

Checks an export against a cluster by server-side applying every resource it holds with `dryRun=All` and comparing the result with the live object. The API server checks each apply and then discards it, so nothing in the cluster changes.

It answers two questions. Is the export right, does it hold what the cluster runs? And after a restore, did it work, does the new cluster match the export? To compare two clusters, or a cluster with an earlier snapshot, use [`diff`](diff.md) instead.

## Synopsis

```sh
setdrift verify <path-to-export> [flags]
```

## Inputs

`<path-to-export>` is an export, or one cluster's directory in it.

| Argument | Checks | Example |
|---|---|---|
| An export holding one cluster | That cluster's directory | `setdrift verify setdrift-export` |
| A cluster's directory | That directory | `setdrift verify setdrift-export/clusters/prod` |
| An export holding several clusters | An error naming them, pass one cluster's directory | `setdrift verify ./exports/clusters/east` |

What the directory's `kustomization.yaml` builds is checked, as `kubectl kustomize` builds it, in a sealed build that refuses remote bases and files from outside the export. Secrets under `redacted/` are skipped, since they hold placeholders and are never applied, and `external-secrets/` is not applied, so not checked.

The cluster is the current kubeconfig context, or the one `--context` names. To see your contexts, run `kubectl config get-contexts`, see [Finding your contexts](diff.md#finding-your-contexts).

### What passes

It passes when at least 95% of the resources (`--threshold`) would not change and every remaining difference has a known explanation. The differences are grouped by cause, with the cluster's and the export's values, who changed the resource after the export was taken, and the likely cause overall.

A server-side apply only changes the fields it sends, so `verify` can't see a field the export left out, since the cluster keeps it. It flags a resource that lost all of its spec or data. Other missing fields only show when the export is applied to a new cluster.

### What this cluster runs

On a cluster other than the export's own, like a restore or migration target, `verify` also checks that the cluster runs what the export's objects name, since an object naming something missing applies cleanly and never works.

- **A StorageClass of a cloud's CSI driver the cluster doesn't run** waits forever for volumes. For EKS it names the variant, the EBS CSI add-on (`ebs.csi.aws.com`) or EKS Auto Mode (`ebs.csi.eks.amazonaws.com`), since a translation for EKS writes Auto Mode's.
- **An Ingress naming an IngressClass** that neither the cluster nor the export has never gets an address.

Each shows under "Won't work on this platform". A list `verify` can't read is skipped rather than reported. A translation can be given directly, `setdrift verify --context gke-prod ./setdrift-export/clusters/prod/translations/te2g`, and is checked with its decisions, whatever the cluster is.

### Network policies

A NetworkPolicy applies on any cluster, but only does anything where a network policy engine enforces it. When the export has NetworkPolicies and its source enforced them, `verify` reads this cluster's system DaemonSets in `kube-system` and `calico-system`, and EKS's `amazon-vpc-cni` ConfigMap, to find its engine.

| This cluster | Result |
|---|---|
| Enforces them, like with Calico, Cilium, GKE Dataplane V2, the EKS VPC CNI's agent or Azure Network Policy Manager | Passes this check, and says which engine |
| Doesn't enforce them | Fails, since the policies would apply and do nothing, saying how to turn enforcement on for this platform |
| Can't be read, like by a pipeline's credentials limited to their own namespaces | Fails the same way, naming the permission it lacked |

To pass without enforcement on purpose, in an emergency or when you know the cluster enforces them but the pipeline can't see it, pass `--accept-unenforced-network-policies`, or for a translation choose `accept-unenforced` in its `translate-decisions.yaml`. The result still says, loudly, that the policies aren't confirmed enforced. An export from before SetDrift recorded the source's engine only gets a warning.

## Examples

The export against the cluster it came from.

```sh
setdrift verify setdrift-export
```

```
Verifying setdrift-export/clusters/prod/platform/aws/eks against context prod (the platform/aws/eks set: this cluster is eks, high confidence)

PASSED: 11 of 11 resources would not change (100.0%; 95% required)
Skipped 1: redacted (Secret values were replaced; not applied on purpose)

Report: setdrift-export/verify/2026-11-23T191402Z-prod-report.md (and .json)
Passed.
```

After restoring the export into another cluster.

```sh
setdrift verify setdrift-export --context restored-prod
```

Every resource must match, with the reports kept outside the export.

```sh
setdrift verify setdrift-export -o ./verify-reports --threshold 100
```

A restore onto a cluster that doesn't enforce NetworkPolicies yet, in an emergency, passing on purpose.

```sh
setdrift verify setdrift-export --context dr-prod --accept-unenforced-network-policies
```

One namespace, in CI, printing the result only.

```sh
setdrift verify setdrift-export -n shop --no-report
```

The generic set, on a cluster of another platform.

```sh
setdrift verify setdrift-export --context kind-test --set generic
```

## Flags

### `--context`

*text · default none*

The kubeconfig context of the cluster to check. Without it the current context is used. It can be the export's own cluster or another one, like the cluster an export was restored into, and the report says which.

```sh
setdrift verify setdrift-export --context restored-prod
```

### `--kubeconfig`

*text · default none*

The kubeconfig file the context comes from. Without it the `KUBECONFIG` variable is used, then `~/.kube/config`, and a path may start with `~`.

```sh
setdrift verify setdrift-export --kubeconfig ~/.kube/prod --context prod
```

### `--namespace`, `-n`

*list · default none*

Checks only the export's resources in these namespaces and their Namespace objects, and leaves out cluster-scoped resources. Repeat it or separate names with commas. A namespace that doesn't exist in the cluster is not an error, its resources show as not in the cluster, as before a restore.

```sh
setdrift verify setdrift-export -n shop -n billing
```

### `--set`

*text · default `auto`*

Which part of the export to check. An export is generic, it applies to any Kubernetes cluster, and when its platform was identified it also has a platform set, like `platform/aws/eks`, that adds what only that platform serves.

| Value | Checks |
|---|---|
| `auto` | The platform set when this cluster is on the export's platform, otherwise the generic set |
| `generic` | The cluster's directory, for any cluster |
| `platform` | The platform set, and an error when the export has none |

The first line of the output says which set was chosen and why.

```sh
setdrift verify setdrift-export --set generic
```

### `--threshold`

*number · default `95`*

The percent of checked resources that must not change for the run to pass, from 0 to 100. Use `100` after a restore, where everything should match.

```sh
setdrift verify setdrift-export --threshold 100
```

### `--output`, `-o`

*text · default none · not with `--no-report`*

The directory the report goes to. Without it the report goes to `<path-to-export>/verify/`, which the export's `.gitignore` keeps out of Git. Each run writes Markdown and JSON, named for when it ran and the cluster, like `2026-11-20T191402Z-prod-report.md`.

```sh
setdrift verify setdrift-export -o ./verify-reports
```

### `--no-report`

*on/off · default off*

Writes no report, and only prints the result, for CI logs.

```sh
setdrift verify setdrift-export --no-report
```

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

### `--accept-unenforced-network-policies`

*on/off · default off*

`--accept-unenforced-network-policy` is the same flag. The NetworkPolicies themselves are applied either way: the flag only decides whether an unconfirmed enforcement passes. Lets the run pass when the source enforced the export's NetworkPolicies and this cluster doesn't, or verify can't tell. Without it, that fails the run, since the policies would apply here and do nothing. Meant for an emergency: the result still says, loudly, that the policies aren't enforced. For a translation, choosing `accept-unenforced` in its `translate-decisions.yaml` does the same and keeps the choice on record.

## Output

The terminal shows which set was checked against which context, how many resources would not change out of how many, what was skipped and why, the differences grouped by cause, and where the report is. The last line is `Passed.`, or the reason it failed.

| Status | Means |
|---|---|
| `0` | Passed |
| `1` | Failed, or an error, the last line says which |

A resource `verify` may not get, or may not patch for the dry-run, counts as not checked. It is left out of the percentage and the run fails for it, naming the permission. A refusal is never reported as a difference.

## Permissions

`get` and `patch` on what the export holds, the dry-run changes nothing but the API server checks `patch` as it would for a real apply. For the network policy check, `list` on DaemonSets in `kube-system` and `calico-system`, and `get` on the ConfigMap `kube-system/amazon-vpc-cni`. Every verb is in [Kubernetes permissions](../permissions/kubernetes.md), and [deploy/permissions](https://github.com/setdrift/setdrift-engine/tree/main/deploy/permissions/kubernetes) has a `setdrift-verify` role to apply.

## See also

- [`export`](export.md), which writes what `verify` checks
- [`diff`](diff.md), comparing two clusters or a cluster with its snapshot
- [Choose a comparison](../which-comparison.md)
