# SetDrift
SetDrift is the premier platform for platform engineers. It seeks to solve the problems the problems that anyone who has worked with Kubernetes in production has faced. Problems like: Our clusters have become pets, We aren't sure how that got there, How to we make sure West and East are running the same things, How do we even check this cluster against that cluster, or the slack @all Who changed something just now everything is broken. 

# SetDrift-Engine
So what is this repo for? This is the open-source engine that enables the development of the SetDrift platform. 

The pkg/ folder contains all of the processing that has to happen to produce a secure, verifiable, Git compatible set of files that strip away the cluster specific details, identify the top level objects and who owns them. 

It applies this process as a cli tool, the setdrift cli - export your cluster config, verify it against your running cluster or just generalize a folder of manifests, run diffs on only meaningful differences between environments, and much much more. 

It currently is only being tested against EKS and local clusters like Kind, with plans 
for other managed Kubernetes platforms in the future. If anyone has cloud credits I can use to test against more platforms, reach out haha but actually. 

## Quick Start

Get started with three commands:
```sh
brew install setdrift/tap/setdrift
setdrift export                   # the current kubeconfig context, into ./setdrift-export
setdrift verify setdrift-export   # dry-run the export against the cluster
```
Open `setdrift-export/clusters/<cluster>/README.md` for what the export
found, and RESTORE.md for recreating the cluster. Each export also saves a
snapshot of everything it read to `setdrift-export/snapshots/`, kept out of
Git, so you can see later what changed: `setdrift diff setdrift-export/snapshots/<time>-<cluster>.pb <context>`.

Export reads with your kubeconfig credentials and changes nothing; it needs
get and list on what it exports (the built-in `view` ClusterRole is enough, except Secrets;
with access to some namespaces only, see [Permissions](https://github.com/setdrift/setdrift-engine/blob/main/docs/permissions.md)).
Verify needs patch permission too, for its dry-run, which changes nothing
either (the built-in `edit` role is enough).

For other install environments, see [Install](https://github.com/setdrift/setdrift-engine/blob/main/docs/install.md)


## Key Features:

- **What isn't in Git** - Every object is classified by who manages it: Argo
  CD, Flux, Helm, an operator, the platform, or nobody. The summary and the
  export's README list what was applied by hand, namespace by namespace:
  the drift to adopt into Git (`--unmanaged-only`). 
- **A repository you can apply** - CRDs first, then cluster-scoped objects,
  then each namespace, with the server's defaults, status and controller
  state stripped, so it applies cleanly to another cluster or under Argo CD
  or Flux. What the platform creates in every cluster (EKS or kind) is left out.
- **No secret values** - SetDrift never stores, saves, prints or moves a
  secret value. Every Secret value is replaced with a placeholder right after
  it is read, before anything else sees it; only the key names are kept. A
  password sitting in another manifest, like a connection string in an env
  var, is replaced the same way as soon as it is recognized. Secrets are
  written for reference only, outside every kustomization, with an
  ExternalSecret for each so External Secrets Operator can restore it from
  your secret manager, where you put the values yourself.
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
- **A restore playbook** - RESTORE.md is the checklist for recreating the
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

One command for each tool, for Intel/AMD and ARM:

| Tool | Command |
|---|---|
| Homebrew (macOS, Linux) | `brew install setdrift/tap/setdrift` |
| curl (macOS, Linux, WSL) | `curl -fsSL https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.sh \| sh` |
| PowerShell (Windows) | `irm https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.ps1 \| iex` |
| Go 1.27+ (any OS) | `go install setdrift.io/setdrift-engine/cmd/setdrift@latest` |

Where each one installs, installing a given version or by hand, checking a
release, and running as `kubectl setdrift`: see
[docs/install.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/install.md).

Check it works with `setdrift version`


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
# annotations on what each pipeline deploys: see
# https://github.com/setdrift/setdrift-engine/blob/main/docs/source-annotations.md, e.g. setdrift.io/source-repo: github.com/setdrift/setdrift-engine
setdrift export -o ./out --unmanaged-only

# Choose the secret manager the ExternalSecrets read from (auto, generic, aws-secrets-manager, aws-parameter-store or none), the store's name and the remote key names: see https://github.com/setdrift/setdrift-engine/blob/main/docs/secrets.md#secret-managers. On Azure Key Vault or Google Secret Manager, set a key template without slashes
setdrift export -o ./out --external-secrets aws-parameter-store \
  --secret-store params --secret-key-template '/prod/{{.Namespace}}/{{.Name}}'

# On EKS, also read the settings held in AWS (Pod Identity, IRSA role trust, access entries, add-ons, EBS volume details) for RESTORE.md; read-only
setdrift export -o ./out --aws

# Offline Mode: read manifests from a directory instead of a cluster
setdrift export --from-dir ./manifests --cluster-name test -o ./out

# Check the export: its platform set on a cluster of the same platform, else
# the generic set (--set generic|platform to choose). Writes a Markdown and
# JSON report to ./out/verify/ (-o for another directory, --no-report for none)
setdrift verify ./out
setdrift verify ./out -o ./verify-reports --threshold 100

# Compare two clusters, can be two context names for default kubeconfig. two directories 
# ./staging ./prod, or two snapshots *-prod.pb *-staging.pb
setdrift diff staging prod

# Compare a cluster with an earlier snapshot of itself 
setdrift diff ./out/snapshots/2026-11-16T090000Z-prod.pb prod   # what changed since Monday

# what differs, who manages it on each side 
setdrift diff staging prod --map-namespace shop-staging=shop --exit-code

# differences in a cluster's own values
setdrift diff prod prod --map-namespace shop=shop-canary   # two namespaces of one cluster

# Inspect a snapshot (the .json beside each .pb is the same, written by export)
setdrift snapshot json ./out/snapshots/2026-11-16T090000Z-prod.pb
```

## Further Reading

- **Which comparison?** When to compare a cluster with its own snapshot, two clusters with each other, or an export with a cluster. See [docs/which-comparison.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/which-comparison.md).
- **What `diff A B` reads.** How `diff` tells a kubeconfig context from a snapshot file or a directory, and what it does when a name is both. See [docs/diff-arguments.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/diff-arguments.md).
- **Snapshots.** What each export saves to `snapshots/`, why it stays out of Git, and how to compare against it later. See [docs/snapshots.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/snapshots.md).
- **Output.** The layout of an export, platform identification, CRDs, Helm, storage, and exporting again. See [docs/output.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/output.md).
- **Secrets.** How every secret value is dropped right after it is read and never stored, and the secret managers the ExternalSecrets can read from. See [docs/secrets.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/secrets.md).
- **Permissions.** What `export`, `diff` and `verify` do with limited access, and the errors they give. See [docs/permissions.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/permissions.md).
- **Source annotations.** The annotations a CI/CD pipeline stamps so its resources are not taken for ones applied by hand. See [docs/source-annotations.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/source-annotations.md).
- **How it works.** The pipeline from collect to emit, and what each package does. See [docs/how-it-works.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/how-it-works.md).
- **Development.** Building, testing, golden and race tests. See [docs/development.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/development.md).
- **Releasing.** Tagging a release, and what the release workflow publishes. See [docs/releasing.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/releasing.md).
- **Compatibility.** The Kubernetes versions and platforms SetDrift is built and tested against. See [docs/compatibility.md](https://github.com/setdrift/setdrift-engine/blob/main/docs/compatibility.md).
- **Security.** See [SECURITY.md](.github/SECURITY.md) to report a vulnerability. A secret value stored, printed or sent anywhere is one.
- **License.** [Apache-2.0](LICENSE)
