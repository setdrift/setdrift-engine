# SetDrift

SetDrift turns a running Kubernetes cluster into a Git-ready Kustomize
repository, and shows you what in it nothing manages. One read-only command,
no agent to install, and no secret values in the output.

```sh
brew install setdrift/tap/setdrift
setdrift export
```

In a few seconds you get:

- **What isn't in Git.** Every object is classified by who manages it: Argo
  CD, Flux, Helm, an operator, the platform, or nobody. The summary and the
  export's README list what was applied by hand, namespace by namespace:
  the drift to adopt into Git (`--unmanaged-only` writes just that).
- **A repository you can apply.** CRDs first, then cluster-scoped objects,
  then each namespace, with the server's defaults, status and controller
  state stripped, so it applies cleanly to another cluster or under Argo CD
  or Flux. What the platform creates in every cluster (EKS's or kind's own
  objects) is left out.
- **No secret values.** Secrets, and passwords sitting in other manifests,
  are written for reference only with placeholders, outside every
  kustomization, with an ExternalSecret for each Secret so External Secrets
  Operator can restore it from your secret manager.
- **Proof it matches.** `setdrift verify` dry-runs the export against the
  cluster and reports how much would change: nothing, if the export is
  right.
- **What differs between clusters.** `setdrift diff staging prod` compares
  two clusters (or a cluster with last week's snapshot) field by field,
  with who manages each resource on each side, the Helm chart versions, and
  the differences that are only each cluster's name, region or account set
  apart.
- **A restore playbook.** RESTORE.md is the checklist for recreating the
  cluster elsewhere, with the commands filled in: CRDs to create first, Helm
  releases to install with their chart versions, webhooks to wait for,
  secrets to supply, volumes to bring over, and on EKS the add-ons, access
  entries and IAM roles to recreate.

> **Status: early (v0.x).** Export, verify and the restore playbook are
> tested end to end on EKS 1.37 (Auto Mode) with Argo CD, Flux, External
> Secrets Operator and Helm releases, including a full delete-and-restore
> into a second cluster ([test/e2e](test/e2e)), and on kind. The output is
> generic, for any conformant cluster; tailored guidance is for EKS and AWS
> so far.

## Install

One command on each OS, for Intel/AMD and ARM:

| OS | Command |
|---|---|
| macOS, Linux (Homebrew) | `brew install setdrift/tap/setdrift` |
| macOS, Linux, WSL | `curl -fsSL https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.sh \| sh` |
| Windows (PowerShell) | `irm https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.ps1 \| iex` |
| Any, with Go 1.27+ | `go install setdrift.io/setdrift-engine/cmd/setdrift@latest` |

- **The scripts** download the latest release for your OS and CPU, check it
  against the release's `checksums.txt`, and install `setdrift`:
  - `install.sh` installs to `/usr/local/bin` when it can write there,
    else `~/.local/bin`;
  - `install.ps1` installs to `%LOCALAPPDATA%\Programs\setdrift` and adds it
    to your user PATH.

  `SETDRIFT_VERSION=v0.1.0` installs a given release, and
  `SETDRIFT_INSTALL_DIR` sets the directory. Neither script needs
  administrator rights.
- **By hand:** every archive is on the
  [releases](https://github.com/setdrift/setdrift-engine/releases) page: a
  `.tar.gz` for macOS and Linux, a `.zip` for Windows. Unpack it and put
  `setdrift` on your PATH.
- **Checking a release:** GitHub attests where each archive was built:
  `gh attestation verify <archive> --repo setdrift/setdrift-engine`.

Check it works with `setdrift version`.

## Quick start

```sh
setdrift export                   # the current kubeconfig context, into ./setdrift-export
setdrift verify setdrift-export   # dry-run the export against the cluster
```

Then open `setdrift-export/clusters/<cluster>/README.md` for what the export
found, and RESTORE.md for recreating the cluster.

Export reads with your kubeconfig credentials and changes nothing; it needs
list and get on what it exports (a cluster-wide reader is enough; with access
to some namespaces only, see below). Verify needs patch permission on the
exported types for its dry-run, which changes nothing either.

## Usage

```sh
# Another context, some namespaces, and keep the snapshot
setdrift export --context prod -n payments -n checkout -o ./out --snapshot snap.pb

# Only what no GitOps tool or Helm manages: the resources to adopt
setdrift export -o ./out --unmanaged-only

# One directory (and kustomization.yaml) per application in each namespace
setdrift export -o ./out --group-by app

# Also export Jobs that have finished
setdrift export -o ./out --include-finished-jobs

# Choose the secret manager the ExternalSecrets read from, the store's name
# and the remote key names
setdrift export -o ./out --external-secrets aws-parameter-store \
  --secret-store params --secret-key-template '/prod/{{.Namespace}}/{{.Name}}'

# On EKS, also read the settings held in AWS (Pod Identity, IRSA role trust,
# access entries, add-ons, EBS volume details) for RESTORE.md; read-only
setdrift export -o ./out --aws

# Offline: read manifests from a directory instead of a cluster
setdrift export --from-dir ./manifests --cluster-name test -o ./out

# Check the export: its platform set on a cluster of the same platform, else
# the generic set (--set generic|platform to choose)
setdrift verify ./out

# Compare two clusters, or a cluster with an earlier snapshot of itself:
# what differs, who manages it on each side, and which differences are only
# each cluster's own values (name, region, account)
setdrift diff staging prod
setdrift diff prod-monday.pb prod
setdrift diff staging prod --map-namespace shop-staging=shop --exit-code
setdrift diff prod prod --map-namespace shop=shop-canary   # two namespaces of one cluster

# Copy the redacted Secrets' values from the source cluster into AWS Secrets
# Manager or Parameter Store, for the ExternalSecrets (plan first, then confirm)
setdrift secrets export ./out

# Inspect a snapshot
setdrift snapshot json snap.pb
```

`export` writes into an empty directory. To export again into a repository,
pass `--force`: it replaces only `clusters/<cluster>/`, so resources gone
from the cluster don't leave stale files, and keeps everything else. It asks
first; `--yes` skips the question in scripts.

`setdrift` also runs as `kubectl setdrift` when installed as
`kubectl-setdrift` on the PATH (`make plugin`).

## Output

```
clusters/<cluster>/
  README.md                 what the export found, and how to apply it
  RESTORE.md                the restore playbook
  kustomization.yaml        everything that can be applied, in order
  crds/<name>.yaml
  cluster-scoped/<kind>[.<group>]/<name>.yaml
  namespaces/<ns>/<kind>[.<group>]/<name>.yaml
  redacted/namespaces/<ns>/<kind>[.<group>]/<name>.yaml.redacted
  external-secrets/namespaces/<ns>/externalsecret.external-secrets.io/<name>.yaml
  restore/volumes/<ns>/<claim>.yaml
  restore/storageclasses/<name>.yaml
  platform/<cloud>/<distribution>/   e.g. platform/aws/eks, platform/local/kind
  .setdrift-export          where and when the export was taken
```

With `--group-by app`, an `<app>/` directory sits between the namespace and
the kind.

**Generic, plus the platform's set.** The cluster's directory applies to any
Kubernetes cluster. SetDrift also works out the platform it ran on (EKS,
AKS, GKE, OpenShift, k3s, RKE2, kubeadm, kind, minikube, Docker Desktop, and
the cloud underneath) from the kubeconfig, the API server and the cluster's
objects, and acts only when the evidence is clear. Then
`platform/<cloud>/<distribution>/` (or `platform/<cloud>/vm/` for a
self-managed cluster on a cloud's VMs) is an overlay of the generic set plus
the objects only that platform serves, with its own README and, for AWS and
EKS, its own restore steps (`platform/aws/RESTORE.md`,
`platform/aws/eks/RESTORE.md`).

**CRDs** are in `crds/`, first in the root `kustomization.yaml`. Argo CD and
Flux apply them before the resources that use them; with kubectl, apply
`crds/` first (the cluster's README.md has the commands), because kubectl
rejects a custom resource whose definition does not exist yet. Custom
resources whose definition the export lacks are listed in RESTORE.md, to
install first.

**Helm.** Helm does not record which repository a chart came from. RESTORE.md
takes it from Argo CD or Flux installing the same chart, or from a file
passed with `--helm-repos`, and lists the charts left to fill in; see
[contrib/helm](contrib/helm), which also has a post-renderer that records
the repository at install time.

**Storage.** `--keep-source-defaults` keeps the storage class a claim got
from this cluster's default class (normally left out), so claims ask for the
same class by name in any cluster. RESTORE.md creates each class the claims
use when the target lacks it, from this cluster's definition, and says how
to point it at another platform's provisioner. Volumes another cluster can
reach (EBS, and other CSI volumes) get a PersistentVolume in
`restore/volumes/`, bound in advance to its claim; volumes on a node's own
disk (local, hostPath, kind's local-path) are reported as not portable.

## Secrets

Secret values never reach the export, the snapshot or Git.

- Secrets, and any other resource holding what looks like a secret value (a
  password in an env var or a config file), are written under `redacted/`
  for reference only, with the values replaced by `SETDRIFT_REDACTED`.
  Applying them would overwrite the real values, so no `kustomization.yaml`
  lists them, and the `.yaml.redacted` extension keeps `kubectl apply -R`,
  Argo CD and Flux from picking them up. The export's README names the
  resources other than Secrets held back this way, and where their values
  are: move those values into Secrets, and the next export includes them.
- Secret keys only a controller wrote (Argo CD's generated passwords, a
  webhook's certificate) are left out altogether: the controller writes
  them again in a new cluster.
- For each redacted Secret, `external-secrets/` has an ExternalSecret that
  has External Secrets Operator read the values from a secret manager,
  covering only the keys a person or tool wrote (merged into the Secret a
  controller creates, with `creationPolicy: Merge`). They reference a store
  named `secret-store`; its README has example stores for AWS, Azure Key Vault,
  Google Secret Manager and Vault.
  `--external-secrets auto` (the default) uses AWS Secrets Manager for a
  cluster on AWS and a generic store elsewhere; `none` writes none.
- `setdrift secrets export DIR` fills those remote secrets on AWS: it reads
  each Secret's values from the source cluster and writes them straight into
  Secrets Manager or Parameter Store. It is the only command that reads
  secret values, and it never writes them to disk or prints them. It shows a
  plan first, skips remote secrets that exist already (`--overwrite`
  replaces them), and refuses a cluster other than the export's source.

If your credentials can only read some namespaces, `export` without `-n`
exports the kubeconfig context's namespace and says what you can read there;
with no context namespace, it asks for `-n`.

## How it works

The CLI (and the in-cluster agent to come, `cmd/agent`) are thin shells
around one pipeline ([pkg/pipeline](pkg/pipeline/pipeline.go)):

```
collect -> redact -> (inventory) -> hygiene -> ownership -> lifecycle -> platform -> provenance -> normalize -> snapshot -> emit
```

| Package | Role |
|---|---|
| [pkg/collect](pkg/collect) | Lists every listable resource with each API group's preferred version, using paginated calls, several types at once. Skips runtime state (Nodes, Leases, metrics, CSRs and similar). High-volume child types (Pods, ReplicaSets, ControllerRevisions, Jobs, EndpointSlices) are listed metadata-only first; only those with no exported controller owner are fetched in full. Controller owners missing from the lists are looked up, to tell a rollout in progress from a deleted owner. An API error for one type skips that type with a `LIST_FAILED` finding; credential and connection errors abort. With `-n`, the Namespace objects are exported too. `FileSource` reads manifests from a directory (or a repository checkout) for tests and offline runs. |
| [pkg/redact](pkg/redact) | Replaces Secret values and strips `last-applied-configuration`. Image pull secrets get valid JSON placeholders that keep the registry names. Service account token data is removed, as the token controller writes it. Always runs first, enforced by `Pipeline`. The emitter keeps redacted resources out of the applied set. |
| [pkg/inventory](pkg/inventory) | Records what restoring the cluster needs that the manifests do not hold: each claim's volume (volume ID, driver, zone, reclaim policy, including StatefulSet claims) and the addresses load balancers were given. Runs right after redaction, before later stages drop those objects and status. |
| [pkg/awsinventory](pkg/awsinventory) | With `--aws`: reads the cluster's settings held in AWS (OIDC issuer, Pod Identity associations, IRSA role trust, access entries, add-ons, EBS volume details), read-only, for RESTORE.md. |
| [pkg/hygiene](pkg/hygiene) | Report-only cleanup suggestions: terminated Pods per namespace by what keeps them, standalone Jobs without `ttlSecondsAfterFinished`, CronJobs whose history limits exceed the defaults, and objects stuck in deletion behind a finalizer. Runs before ownership so it sees child objects. |
| [pkg/ownership](pkg/ownership) | Keeps top-level objects only, dropping anything controller-owned by another exported object, garbage whose owner was deleted, and objects the cluster generates itself (`kube-root-ca.crt`, default RBAC, Endpoints of Services with a selector, dynamically provisioned volumes and others). |
| [pkg/lifecycle](pkg/lifecycle) | Drops finished or temporary objects: objects being deleted (and everything in a Namespace being deleted), terminated Pods, finished Jobs and Helm or Argo CD hooks, with a per-namespace count in the snapshot findings. Flags running Pods that have no controller, and Jobs and run-once Pods that a GitOps tool would re-run. |
| [pkg/platform](pkg/platform) | Classifies the platform from weighed evidence (kubeconfig, API server, nodes, the cluster's objects, `--aws`) with a confidence; conflicting evidence means no classification. |
| [pkg/provenance](pkg/provenance) | Classifies each object as Argo CD, Flux, Helm, operator, platform or unmanaged, and flags objects whose manager is gone from the cluster. Drops Helm's release records. |
| [pkg/normalize](pkg/normalize) | Strips server-owned state so the export round-trips cleanly: fields only Kubernetes' controllers own (from managedFields), values equal to the API server's defaults, owner references, status and server-set metadata, `kubectl rollout restart` timestamps, and the markers Argo CD, Flux and Helm stamp on what they apply (the snapshot keeps them as provenance evidence). |
| [pkg/snapshot](pkg/snapshot) | The versioned snapshot ([proto](proto/setdrift/snapshot/v1alpha1/snapshot.proto)), the contract between the CLI, the agent and the control plane. Protobuf is canonical; JSON is for inspection. |
| [pkg/emit/kustomize](pkg/emit/kustomize) | Writes a snapshot as a Kustomize repository: the generic set, the platform's set, the README's overview, the ExternalSecrets and RESTORE.md. Resources with redacted values go to `redacted/`, outside every kustomization; objects the platform manages are left out. |
| [pkg/diff](pkg/diff) | Compares two snapshots: pairs resources by group, kind, namespace and name (with namespace mapping), compares the normalized manifests field by field (lists of named items, such as containers, by name), sets aside each cluster's own values (name, region, account, mapped namespaces), and gives what the snapshots tell about why: the manager on each side, the Helm chart versions, the API versions. Platform objects are not compared. |
| [pkg/verify](pkg/verify) | The acceptance test: a server-side dry-run apply of every exported resource, compared with the live object. Passes when at least 95% would not change and every remaining diff is explained; reports custom resources whose CRD the cluster lacks. |
| [pkg/secretexport](pkg/secretexport) | `setdrift secrets export`: plans and writes the remote secrets the ExternalSecrets read, from the source cluster's Secrets, through a provider (AWS Secrets Manager, Parameter Store). |

`pkg/` must not depend on CLI concerns; command-line code lives in `cmd/`.
The module path is `setdrift.io/setdrift-engine` ([docs/vanity-import.md](docs/vanity-import.md)).

## Development

```sh
make build             # bin/setdrift
make test              # unit and golden tests
make golden            # regenerate testdata/corpus/*/expected after an intended change
make generate          # regenerate Go code after editing proto/
make lint              # gofmt, go vet, buf lint
make release-snapshot  # the release archives and Homebrew formula in dist/, unpublished (needs goreleaser)
make e2e-populate      # an EKS test cluster, created on demand (test/e2e/README.md)
```

Golden tests: each `testdata/corpus/<case>/input` directory holds manifests as
read from a cluster, and `expected/` holds the snapshot JSON and emitted
repository. Add a case for every normalization rule or bug fix.

Race tests (`pkg/collect/race_test.go`) also serve every corpus through fake
cluster clients, and delete, recreate or deny access to each object the
collector fetches between its list and its fetch. Each result must match an
export of the cluster without those objects. New corpus cases are covered
automatically.

## Releasing

Push a version tag: `git tag v0.1.0 && git push origin v0.1.0`. The release
workflow ([.github/workflows/release.yaml](.github/workflows/release.yaml))
tests, then runs GoReleaser ([.goreleaser.yaml](.goreleaser.yaml)): archives
for macOS, Linux and Windows with `checksums.txt` on the GitHub release, the
formula in [setdrift/homebrew-tap](https://github.com/setdrift/homebrew-tap),
and build provenance attestations. CI builds the same archives on every
pull request and runs both installers on Linux, macOS and Windows, on
Intel/AMD and ARM. Run `make e2e-restore` before a release.

## Compatibility

- Built against client-go v0.37 (Kubernetes 1.37); for any conformant
  cluster.
- Tested end to end on EKS 1.37 (Auto Mode) and on kind. Supported: the
  current EKS version and the two before it (EKS n-2).

## Security

See [SECURITY.md](.github/SECURITY.md) to report a vulnerability. A secret
value reaching the output is one.

## License

[Apache-2.0](LICENSE)
