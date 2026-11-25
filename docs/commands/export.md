# setdrift export

Reads a cluster, or a directory of manifests, and writes its configuration as a Kustomize repository you can review, commit and apply elsewhere. It only reads, and the cluster is not changed.

Each export also saves a snapshot of everything it read, which [`diff`](diff.md) can compare later, and writes a README of what it found, a RESTORE.md playbook for recreating the cluster, and a [cluster profile](../output.md#cluster-profile). Secret values never leave the cluster, see [Handle secrets](../secrets.md).

Use it to see what is in a cluster and who manages each part, to adopt what nothing manages into Git (`--unmanaged-only`), or to recreate a cluster somewhere else. To check an export against a cluster afterwards, use [`verify`](verify.md).

## Synopsis

```sh
setdrift export [flags]
```

## Inputs

`export` takes no arguments. It reads one of two sources, chosen by its flags.

| Source | How | Example |
|---|---|---|
| The current kubeconfig context | No flags | `setdrift export` |
| Another context | `--context` | `setdrift export --context prod` |
| A context in another kubeconfig | `--kubeconfig`, then `--context` | `setdrift export --kubeconfig ~/.kube/prod --context prod` |
| A directory of manifests | `--from-dir`, no cluster is contacted | `setdrift export --from-dir ./manifests --cluster-name test` |

To see your contexts, run `kubectl config get-contexts`, and `kubectl config current-context` for the one `export` uses by default. [Finding your contexts](diff.md#finding-your-contexts) has more, like merging two kubeconfig files.

The export goes into `./setdrift-export`, or the directory `-o` names, which must be empty unless `--force` is given. [Output](../output.md) describes everything it writes.

### Exports and translations

An export and a translation aren't interchangeable. An export is a cluster's configuration, a translation is a set of changes to one export for another platform, written by [`translate`](translate.md) into the export's `translations/<code>/`. Each command says plainly when it's given the wrong one, and names what to give instead.

| Given | `export` does |
|---|---|
| `-o` naming an export's root, the directory holding `clusters/` | Writes the export there, and with `--force` replaces `clusters/<cluster>/` but keeps its `translations/` and their decisions, then says each translation was made from the previous export and how to bring it in step |
| `-o` naming a translation, a platform set, a cluster's directory or a place inside one | Refuses, even with `--force`, since the export would land inside another export, and names the export's root to give instead |
| `--from-dir` naming an export | Reads its manifests, leaving out its translations, snapshots and records |
| `--from-dir` naming a translation or a platform set | Refuses, since they're changes to an export rather than manifests, and names the export to read instead |

## Examples

The current context, into `./setdrift-export`.

```sh
setdrift export
```

```
Applied by hand, so this export is their only record (no GitOps tool, Helm or operator manages them): 2
  shop                   2  ConfigMap settings; Deployment web

To recreate this cluster elsewhere, follow setdrift-export/clusters/east/RESTORE.md.
Overview: setdrift-export/clusters/east/README.md (0 redactions, 0 findings; every finding is in the snapshot).
Snapshot: setdrift-export/snapshots/2026-11-23T223643Z-east.pb, setdrift-export/snapshots/2026-11-23T223643Z-east.json
```

One cluster's two namespaces, into a directory of your choosing.

```sh
setdrift export --context prod -n payments -n checkout -o ./prod-export
```

Export again into the same repository, replacing only this cluster's directory and keeping its earlier snapshots.

```sh
setdrift export --context prod -o ./prod-export --force --yes
```

Only what nothing manages, to adopt into Git.

```sh
setdrift export --context prod --unmanaged-only -o ./adopt
```

On EKS, with the settings AWS holds for RESTORE.md.

```sh
setdrift export --context prod --aws
```

A directory of manifests, with no cluster.

```sh
setdrift export --from-dir ./manifests --cluster-name test -o ./out
```

## Flags

The flags come in five groups, what to read, where to write, how resources are classified, the ExternalSecrets, and AWS.

### `--context`

*text · default none · not with `--from-dir`*

The kubeconfig context to read. Without it the current context is used, the one `kubectl config current-context` prints. On EKS, a context named by its cluster ARN (`arn:aws:eks:us-east-1:111111111111:cluster/prod`) also gives the cluster's region and name.

```sh
setdrift export --context prod
```

### `--kubeconfig`

*text · default none · not with `--from-dir`*

The kubeconfig file the context comes from. Without it the `KUBECONFIG` variable is used, then `~/.kube/config`, and a path may start with `~`.

```sh
setdrift export --kubeconfig ~/.kube/prod --context prod
```

### `--namespace`, `-n`

*list · default none · not with `--from-dir`*

Reads only these namespaces, and leaves out cluster-scoped resources (ClusterRoles, CRDs, StorageClasses) except the Namespace objects themselves. Repeat it or separate names with commas, as in `-n payments -n checkout` or `-n payments,checkout`. Each one must exist and be readable, which is checked before anything is read, so a typo writes nothing. Without it, every namespace you can read is read, see [Permissions](../permissions/index.md) for what happens with access to some namespaces only.

```sh
setdrift export -n payments -n checkout
```

### `--from-dir`

*text · default none*

Reads the manifests in a directory instead of a cluster, `.yaml`, `.yml` and `.json` files in it and below it. No cluster is contacted, so the flags about reading a cluster can't be used with it, and the cluster is named `local` unless `--cluster-name` names it. A Kustomization that only lists its own files is read as those files, and one that changes them (patches, generators, a namespace) is refused with the command that renders it first. An export's own directory reads back too, without its snapshots, records and translations. A translation or a platform set is refused, see [Exports and translations](#exports-and-translations).

```sh
setdrift export --from-dir ./manifests --cluster-name test -o ./out
```

### `--full-list`

*on/off · default off · not with `--from-dir`*

Lists every resource in full. By default, high-volume types (Pods, ReplicaSets, Jobs, ControllerRevisions, EndpointSlices) are listed metadata-only first, and only those nothing collected owns are fetched in full, which is much lighter on a big cluster. With this flag every resource is read in one full list per type instead.

```sh
setdrift export --full-list
```

### `--cluster-name`

*text · default none*

The name the cluster gets in the export, its directory `clusters/<name>/` and its snapshots' names. Without it the name comes from the context, the cluster name in an EKS ARN or the EKS credentials, else the context's own name, and `local` for `--from-dir`.

```sh
setdrift export --context arn:aws:eks:us-east-1:111111111111:cluster/prod --cluster-name prod-us-east-1
```

### `--output`, `-o`

*text · default `setdrift-export`*

The directory to write the repository to. It is created if missing, and must be empty unless `--force` is given.

```sh
setdrift export -o ./prod-export
```

### `--force`

*on/off · default off*

Writes into a directory that isn't empty, replacing only `clusters/<cluster>/` in it, so resources gone from the cluster don't leave stale files. Everything else stays, other clusters' directories, earlier snapshots and verify reports, and the cluster's own `translations/` with the choices in their `translate-decisions.yaml`. It also replaces an existing snapshot file `--snapshot` names. It says what it will delete and asks first. When the directory holds the export of a different cluster under the same name, like two EKS clusters called `prod` in two regions, it says so before asking. It tells clusters apart by their EKS ARN, or else by their `kube-system` namespace, which stays the same for a cluster's whole life. To keep both exports, give one a name of its own with `--cluster-name`.

```sh
setdrift export -o ./prod-export --force
```

#### `--yes`, `-y`

*on/off · default off · needs `--force`*

Replaces without asking, for scripts and CI. Without a terminal to ask at, `--force` alone stops and says to pass `--yes`. It never replaces a different cluster's export, which needs a person to answer the question, so a script can't overwrite another cluster's export by accident.

```sh
setdrift export -o ./prod-export --force --yes
```

### `--snapshot`

*text · default none*

Where the protobuf snapshot goes, a file or a directory. Without it the snapshot goes to `<output>/snapshots/<time>-<cluster>.pb`, which the export's `.gitignore` keeps out of Git. A directory gets the default file name in it, and an existing file is replaced only with `--force`. See [Snapshots](../snapshots.md).

```sh
setdrift export -o ./out --snapshot ~/snapshots/
```

### `--snapshot-json`

*text · default none*

Where the same snapshot goes as readable JSON, a file or a directory, `<output>/snapshots/<time>-<cluster>.json` by default. [`diff`](diff.md) reads either form.

```sh
setdrift export -o ./out --snapshot-json ./review/prod.json
```

### `--no-snapshot`

*on/off · default off · not with `--snapshot`, `--snapshot-json`*

Writes no snapshot. The export has no record to compare later, so use it only when the repository is all you want.

```sh
setdrift export -o ./out --no-snapshot
```

### `--unmanaged-only`

*on/off · default off*

Writes only the resources nothing manages, no GitOps tool, Helm, operator or CI/CD pipeline, to adopt into Git. The snapshot still holds everything. Check what counts as applied by hand first, since a pipeline's `kubectl apply` looks the same unless it marks its resources, see [Source annotations](../pipeline-integration/source-annotations.md).

```sh
setdrift export --unmanaged-only -o ./adopt
```

### `--argocd-instance-label`

*text · default `app.kubernetes.io/instance`*

A custom label Argo CD's label tracking uses, for an Argo CD whose `argocd-cm` sets `application.instanceLabelKey`. Resources with that label count as managed by Argo CD.

Argo CD tracks by annotation (`argocd.argoproj.io/tracking-id`) by default since 3.0, which SetDrift reads without this flag. Label tracking with the default label, `app.kubernetes.io/instance`, isn't read at all, even when this flag names it. Every Helm chart sets that label to its release name, so it can't tell Argo CD's resources from a chart's, and SetDrift doesn't guess. An Argo CD that still tracks by the default label can switch to annotation tracking (`application.resourceTrackingMethod: annotation` in `argocd-cm`) or to a custom label, and its resources count as Argo CD's after its next sync.

```sh
setdrift export --argocd-instance-label argocd.argoproj.io/instance
```

### `--keep-source-defaults`

*on/off · default off*

Keeps the values this cluster's own defaults set that another cluster may lack. The main one is a claim's storage class taken from this cluster's default class, normally left out so the claim takes the target's default, and kept with this flag so the claim asks for the same class by name anywhere. RESTORE.md then creates each class the claims use when the target lacks it.

```sh
setdrift export --keep-source-defaults
```

### `--helm-repos`

*text · default none*

A YAML file naming the chart repository of each Helm release, which Helm does not record, so RESTORE.md writes complete `helm install` commands. Each line is `<chart>: <url>` for every release of a chart, or `<namespace>/<release>: <url>` for one. RESTORE.md lists the charts still to fill in, and a line that matches no release is a warning. The [Helm post-renderer](https://github.com/setdrift/setdrift-engine/tree/main/contrib/helm) records the repository at install time instead.

```sh
setdrift export -o ./prod-export --helm-repos ./helm-repos.yaml
```

### `--external-secrets`

*text · default `auto`*

The secret manager the ExternalSecrets read from. For each Secret it redacts, the export writes an ExternalSecret, so External Secrets Operator can rebuild the Secret in a new cluster from your secret manager, where you store the values yourself.

| Value | Secret manager |
|---|---|
| `auto` | AWS Secrets Manager on a cluster on AWS, otherwise `generic` |
| `generic` | Any, with example stores for Azure Key Vault, Google Secret Manager, HashiCorp Vault and AWS |
| `aws-secrets-manager` | AWS Secrets Manager |
| `aws-parameter-store` | AWS Systems Manager Parameter Store |
| `none` | No ExternalSecrets are written |

[Secret managers](../secrets.md#secret-managers) has the details.

```sh
setdrift export --external-secrets aws-parameter-store
```

#### `--secret-store`

*text · default none*

The name of the External Secrets store the ExternalSecrets reference, `secret-store` when not given. Use the name of a store you already have.

```sh
setdrift export --secret-store vault-backend
```

#### `--secret-store-kind`

*text · default `ClusterSecretStore`*

The kind of that store, `ClusterSecretStore` for one store the whole cluster uses, or `SecretStore` for one in each namespace.

```sh
setdrift export --secret-store team-store --secret-store-kind SecretStore
```

#### `--secret-key-template`

*text · default none*

How each Secret's remote key is named, a Go template with `.Cluster`, `.Namespace` and `.Name`. The default is `{{.Cluster}}/{{.Namespace}}/{{.Name}}`, with a leading `/` for `aws-parameter-store`. It is checked before anything is read. Azure Key Vault and Google Secret Manager don't allow `/` in names, so use dashes there.

```sh
setdrift export --external-secrets generic --secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'
```

### `--aws`

*on/off · default off · not with `--from-dir`*

On EKS, also reads the settings AWS holds about the cluster, read-only and with your AWS credentials. That's Pod Identity associations, the trust of IRSA roles, access entries, add-ons, EBS volumes, and the VPC and subnets for the cluster profile. RESTORE.md then covers recreating them. Every AWS call it makes is in [AWS permissions](../permissions/aws.md). The cluster's name and region come from the context, or from the two flags below.

```sh
setdrift export --context prod --aws
```

#### `--aws-cluster`

*text · default none · needs `--aws` · not with `--from-dir`*

The EKS cluster's name, when the context doesn't show it, like a context named `prod` whose credentials come from a profile.

```sh
setdrift export --context prod --aws --aws-cluster prod-cluster
```

#### `--aws-region`

*text · default none · needs `--aws` · not with `--from-dir`*

The AWS region, when the context doesn't show it.

```sh
setdrift export --context prod --aws --aws-region us-east-1
```

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

## Output

The terminal shows what was applied by hand, by namespace, then where the README, RESTORE.md and the snapshots are, with how many values were redacted and how many findings there are. The export holds these.

- **`clusters/<cluster>/` -** The repository, with a `kustomization.yaml` that applies everything in order, CRDs first, then cluster-scoped resources, then each namespace. Platform-specific resources go in `platform/<cloud>/<distribution>/`.
- **`README.md` and `RESTORE.md` -** What the export found and who manages it, and the playbook for recreating the cluster.
- **`redacted/` and `external-secrets/` -** Secrets with their values replaced, outside every kustomization, and the ExternalSecrets that rebuild them.
- **`cluster_profile/` -** The cluster as infrastructure, its networks, DNS, storage and components.
- **`snapshots/` -** What the export read, kept out of Git.

[Output](../output.md) has the full layout.

## Permissions

`get` and `list` on what it exports, and optionally `list` on Secrets, namespaces and nodes. With `--aws`, the AWS calls in [AWS permissions](../permissions/aws.md). Every verb is in [Kubernetes permissions](../permissions/kubernetes.md).

## See also

- [Understanding your export](../understanding-your-export.md), a walkthrough of one export, file by file, on a cluster you can build locally
- [Output](../output.md), everything the export writes
- [Handle secrets](../secrets.md), how secret values are dropped and how ExternalSecrets rebuild them
- [Snapshots](../snapshots.md)
- [`verify`](verify.md), whether the export applies cleanly to a cluster
- [`diff`](diff.md), comparing exports and clusters
