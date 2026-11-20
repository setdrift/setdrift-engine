# SetDrift

SetDrift is the open-source project used to build the platform over at SetDrift.io.
The CLI tool here converts running clusters into a git-ready kustomize repository
directly from the CLI, with no cluster install required, while protecting your secrets. 

It currently is only being tested against EKS and local clusters like Kind, with plans 
for other managed Kubernetes platforms in the future. 

Get running right away using brew:
```sh
brew install setdrift/tap/setdrift
setdrift export
```

What you get:

- **What isn't in Git** - Every object is classified by who manages it: Argo
  CD, Flux, Helm, an operator, the platform, or nobody. The summary and the
  export's README list what was applied by hand, namespace by namespace:
  the drift to adopt into Git (`--unmanaged-only`). 
- **A repository you can apply** - CRDs first, then cluster-scoped objects,
  then each namespace, with the server's defaults, status and controller
  state stripped, so it applies cleanly to another cluster or under Argo CD
  or Flux. What the platform creates in every cluster (EKS or kind) is left out.
- **No secret values** - Secrets, and passwords sitting in other manifests,
  are written for reference only with placeholders, outside every
  kustomization, with an ExternalSecret for each Secret so External Secrets
  Operator can restore it from your secret manager.
- **Proof it matches** - `setdrift verify` dry-runs the export against the
  cluster and reports how much would change: nothing, if the export is
  right. Differences are grouped by cause, with the cluster's and the
  export's values and who set them (`kubectl edit`, a scale, a deploy), and
  each run writes a report to `setdrift-export/verify/`.
- **Cluster Diff** - `setdrift diff` compares across place or across time:
  two clusters now (`setdrift diff staging prod`: how do they differ?), or
  one cluster with its own earlier snapshot (what changed since Monday, and
  who changed it?). Field by field, with who manages each resource on each
  side, the Helm chart versions, and the differences that are only each
  cluster's name, region or account set apart.
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

Check it works with `setdrift version`

## Quick start

```sh
setdrift export                   # the current kubeconfig context, into ./setdrift-export
setdrift verify setdrift-export   # dry-run the export against the cluster
```

Then open `setdrift-export/clusters/<cluster>/README.md` for what the export
found, and RESTORE.md for recreating the cluster. Each export also saves a
snapshot of everything it read to `setdrift-export/snapshots/`, kept out of
Git, so you can see later what changed: `setdrift diff setdrift-export/snapshots/<time>-<cluster>.pb <context>`.

Export reads with your kubeconfig credentials and changes nothing; it needs
get and list on what it exports (the built-in `view` ClusterRole is enough;
with access to some namespaces only, see [Permissions](#permissions)).
Verify needs patch permission too, for its dry-run, which changes nothing
either (the built-in `edit` role is enough).

## Usage

```sh

# using the default config and default context, export all namespaces into ./setdrift-export
setdrift export

# Using kubeconfig foo, use context prod, export namespaces payments and checkout only, into ./out
setdrift export --kubeconfig ~/foo --context prod -n payments -n checkout -o ./out

# Choose where the snapshot goes: a named file, or a directory 
# default format: ./setdrift-export/snapshots/<time>-<cluster>.pb and .json --no-snapshot for none)
setdrift export --snapshot ~/snapshots/ --snapshot-json prod.json

# Only export the resources that are not identified as being managed, e.g. no helm indicators
# Resolve false positives from CI/CD pipelines by stamping the setdrift.io/source-*
# annotations on what each pipeline deploys: see "Source annotations" below
# (#source-annotations), e.g. setdrift.io/source-repo: github.com/setdrift/setdrift-engine
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
# the generic set (--set generic|platform to choose). Writes a Markdown and
# JSON report to ./out/verify/ (-o for another directory, --no-report for none)
setdrift verify ./out
setdrift verify ./out -o ./verify-reports --threshold 100

# Compare two clusters now (place), or a cluster with an earlier snapshot of
# itself (time): what differs, who manages it on each side, and which
# differences are only each cluster's own values (name, region, account)
setdrift diff staging prod
setdrift diff ./out/snapshots/2026-11-16T090000Z-prod.pb prod   # what changed since Monday
setdrift diff staging prod --map-namespace shop-staging=shop --exit-code
setdrift diff prod prod --map-namespace shop=shop-canary   # two namespaces of one cluster

# Copy the redacted Secrets' values from the source cluster into AWS Secrets
# Manager or Parameter Store, for the ExternalSecrets (plan first, then confirm)
setdrift secrets export ./out

# Inspect a snapshot (the .json beside each .pb is the same, written by export)
setdrift snapshot json ./out/snapshots/2026-11-16T090000Z-prod.pb
```

**Which comparison?** A difference comes from time (a cluster changed) or
from place (two clusters are set up differently); each comparison holds one
fixed, so you can tell which:

| Command | Answers |
|---|---|
| `setdrift diff <snapshot> prod` | What changed in prod since the snapshot, and who changed it (drift, incidents) |
| `setdrift diff staging prod` | How two clusters differ right now (before a release, DR readiness, blue/green, regions) |
| `setdrift verify <export>` | Whether applying the export would change the cluster: is the export right, did the restore work |

Comparing an old export of one cluster with another cluster mixes the two:
each difference could be either. `diff` accepts any combination of contexts,
snapshots and directories; two contexts is the same as exporting both now
and comparing the snapshots.

Resources a CI/CD pipeline deploys look applied by hand unless the pipeline
says where they come from: see [Source annotations](#source-annotations).

`export` writes into an empty directory. To export again into a repository,
pass `--force`: it replaces only `clusters/<cluster>/`, so resources gone
from the cluster don't leave stale files, and keeps everything else,
including the earlier snapshots and verify reports. It asks first; `--yes`
skips the question in scripts.

**Snapshots.** Each export writes a snapshot of everything it read, as
protobuf (`.pb`, for `setdrift diff`) and JSON (for reading, or `jq`): every
resource, normalized and redacted, including what the repository leaves out
(the platform's own objects), who manages each, every finding, and the
inventory (volumes, Helm releases, load balancers, nodes, and with `--aws`
the AWS settings). They hold no secret values and are readable by you only.
Exports into the same directory build a history, one pair per export. The
export's `.gitignore` keeps `snapshots/` out of Git: they change with every
export, and the repository holds what is applied. `--snapshot` and
`--snapshot-json` take a file or a directory; an existing file is replaced
only with `--force`.

`setdrift` also runs as `kubectl setdrift` when installed as
`kubectl-setdrift` on the PATH (`make plugin`).

## Output

```
.gitignore                  keeps snapshots/ out of Git
snapshots/<time>-<cluster>.pb, .json   what each export read (not in Git)
verify/<time>-<cluster>-report.md, .json   each setdrift verify run
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
the repository at install time (`setdrift.io/chart-repo`).

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

## Permissions

`export`, `diff` and `verify` follow the same rules, and every refusal names
who the cluster says you are (`system:serviceaccount:team:ci`, an IAM role
on EKS) and how to fix it. `-n` limits all three to some namespaces.

| Your access | Without `-n` | With `-n X` |
|---|---|---|
| The whole cluster | Everything is read | `X` is read |
| Some namespaces, and you can list namespaces | The namespaces you can read are read, and the rest are named as not readable | `X` is read if you can read it |
| Some namespaces, and you can't list namespaces | The kubeconfig context's namespace is read; with none set, you're asked for `-n` | `X` is read if you can read it |
| None | An error naming what you need | An error naming what you need |

- **`-n X` must exist and be readable,** checked before anything is read, so
  nothing is written on a typo. A namespace that does not exist gets the
  cluster's namespaces listed; one you can't read gets the RoleBinding that
  would grant it.
- **A type refused in one namespace** (a role that allows Pods but not
  ConfigMaps) is skipped there only, and listed in the summary.
- **`verify`** counts a resource you may not get, or may not patch for the
  dry-run, as not checked: out of the percentage, and the run fails for it,
  saying which permission is missing. It never reports a refusal as a
  difference. With `-n`, a namespace that does not exist in the target is
  not an error: its resources show as not in the cluster (before a restore).
- **`diff`** compares only what both sides read: a side that read some
  namespaces, or could not list a type, limits the other to the same, so
  nothing shows as in one side only for not having been read.
- **The kubeconfig itself:** a missing, empty, unreadable or malformed file,
  a directory instead of a file, a context that does not exist, an expired
  token, a missing credential plugin or an unreachable cluster each get
  their own message, within 15 seconds, before anything is written.

## Source annotations

A pipeline that runs `kubectl apply` leaves no GitOps markers, so the
resources it deploys look applied by hand: false positives in
`--unmanaged-only` and in the summary's "Applied by hand" list, though they
live in Git. Have each pipeline stamp where they come from:

| Annotation | What it holds | GitHub Actions | GitLab CI | Required |
|---|---|---|---|---|
| `setdrift.io/source-repo` | The repository | `$GITHUB_SERVER_URL/$GITHUB_REPOSITORY` | `$CI_PROJECT_URL` | Yes |
| `setdrift.io/source-path` | The directory in it that is applied, e.g. `deploy/overlays/prod` | your path | your path | No |
| `setdrift.io/source-branch` | The branch deployed | `$GITHUB_REF_NAME` (`$GITHUB_HEAD_REF` on a pull request) | `$CI_COMMIT_BRANCH` | No |
| `setdrift.io/source-tag` | The tag deployed, when a release deploys a tag | `$GITHUB_REF_NAME` on a tag push | `$CI_COMMIT_TAG` | No |
| `setdrift.io/source-revision` | The commit deployed, in full | `$GITHUB_SHA` | `$CI_COMMIT_SHA` | No |
| `setdrift.io/deployed-by` | The pipeline run | `$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID` | `$CI_JOB_URL` | No |

`source-repo` alone ends the false positives. It is read in any common form:
`https://github.com/foo/bar`, `github.com/foo/bar`, `github/foo/bar`,
`https://github.com/foo/bar.git` and `git@github.com:foo/bar.git` are all
`https://github.com/foo/bar` (also `gitlab/` and `bitbucket/` shorthands),
and credentials in the URL are dropped. The others say what to compare and
where a fix goes:

- **`source-branch`** answers where a change belongs: on a cluster that
  feature branches deploy to, which branch each resource came from, even
  after the branch's commits are squashed or force-pushed away.
- **`source-tag`** is for releases deployed from a tag (`v1.4.2`); set it
  instead of, or with, the branch.
- **`source-revision`** pins the exact commit. A cluster promoted from
  another (pre-prod to prod) carries the same commit, so `setdrift diff
  preprod prod` can tell a cluster that is behind from one edited by hand.

For example, for `github.com/foo/bar`, where feature branches deploy to a
dev cluster and `main` deploys to pre-prod, then to prod after UAT:

| Cluster | `source-path` | `source-branch` | `source-revision` |
|---|---|---|---|
| dev | `deploy/overlays/dev` | `feature/login` | that branch's commit |
| pre-prod | `deploy/overlays/preprod` | `main` | the `main` commit |
| prod | `deploy/overlays/prod` | `main` | the same commit as pre-prod |

**What SetDrift does with them.** It shows such a resource as
`CI/CD <repo>//<path>`, leaves it out of `--unmanaged-only` and of "Applied
by hand", and keeps the annotations in the snapshot as evidence. The
exported manifests drop them: they change with every deploy and between
clusters. Argo CD, Flux and Helm markers still win where present (the
annotations are then kept as extra evidence), except Helm's label alone,
which `helm template | kubectl apply` leaves behind.

**Adding them.** With kustomize, in the pipeline step that deploys, before
`kubectl apply -k`:

```yaml
- name: Deploy
  env:
    OVERLAY: deploy/overlays/${{ inputs.environment }}   # dev, preprod or prod
  run: |
    cd "$OVERLAY"
    kustomize edit add annotation \
      "setdrift.io/source-repo:$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
      "setdrift.io/source-path:$OVERLAY" \
      "setdrift.io/source-branch:${GITHUB_HEAD_REF:-$GITHUB_REF_NAME}" \
      "setdrift.io/source-revision:$GITHUB_SHA" \
      "setdrift.io/deployed-by:$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
    kubectl apply -k .
```

With plain manifests:

```sh
kubectl annotate --local -f deploy/prod -o yaml \
  "setdrift.io/source-repo=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
  "setdrift.io/source-branch=$GITHUB_REF_NAME" \
  "setdrift.io/source-revision=$GITHUB_SHA" | kubectl apply -f -
```

For a release from a tag, replace `source-branch` with
`"setdrift.io/source-tag:$GITHUB_REF_NAME"` in the step that runs on tag
pushes.

**Without annotations,** a pipeline can name its kubectl field manager:
`kubectl apply --field-manager=github-actions` in the pipeline, and
`setdrift export --ci-field-manager github-actions` (also on `diff`). The
resources it wrote then count as CI/CD, with low confidence and no
repository. Helm releases need neither: Helm's own markers identify them.

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
| [pkg/provenance](pkg/provenance) | Classifies each object as Argo CD, Flux, Helm, CI/CD (the `setdrift.io/source-*` annotations, or `--ci-field-manager`), operator, platform or unmanaged, and flags objects whose manager is gone from the cluster. Drops Helm's release records. |
| [pkg/normalize](pkg/normalize) | Strips server-owned state so the export round-trips cleanly: fields only Kubernetes' controllers own (from managedFields), values equal to the API server's defaults, owner references, status and server-set metadata, `kubectl rollout restart` timestamps, and the markers Argo CD, Flux and Helm stamp on what they apply (the snapshot keeps them as provenance evidence). |
| [pkg/snapshot](pkg/snapshot) | The versioned snapshot ([proto](proto/setdrift/snapshot/v1alpha1/snapshot.proto)), the contract between the CLI, the agent and the control plane. Protobuf is canonical; JSON is for inspection. |
| [pkg/emit/kustomize](pkg/emit/kustomize) | Writes a snapshot as a Kustomize repository: the generic set, the platform's set, the README's overview, the ExternalSecrets and RESTORE.md. Resources with redacted values go to `redacted/`, outside every kustomization; objects the platform manages are left out. |
| [pkg/diff](pkg/diff) | Compares two snapshots: pairs resources by group, kind, namespace and name (with namespace mapping), compares the normalized manifests field by field (lists of named items, such as containers, by name), sets aside each cluster's own values (name, region, account, mapped namespaces), and gives what the snapshots tell about why: the manager on each side, the Helm chart versions, the API versions. Platform objects are not compared. |
| [pkg/verify](pkg/verify) | The acceptance test: a server-side dry-run apply of every exported resource, compared with the live object. Passes when at least 95% (`--threshold`) would not change and every remaining diff is explained. Groups the differences by cause, with values, who owns each differing field (managedFields) and who wrote the object after the export, and names the likely cause: the cluster changed since, a different cluster, one field set on many resources, or deleted resources. Reports custom resources whose CRD the cluster lacks. |
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
