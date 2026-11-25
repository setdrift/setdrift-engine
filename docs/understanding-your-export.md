# Understanding your export

An export is a lot of files at once. This page walks through one, from a small cluster you can build on your laptop, and says what each part is for and what to take from it, so you know where to look in your own.

## Follow along

You need Docker, kubectl, `setdrift` (see [Install](install.md)) and kind, which runs a Kubernetes cluster in Docker.

<!-- tabs -->
#### macOS
```sh
brew install kind
```
#### Linux
```sh
curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.30.0/kind-linux-amd64 && chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind
```
#### Windows
```powershell
winget install Kubernetes.kind
```
#### Go
```sh
go install sigs.k8s.io/kind@latest
```
<!-- /tabs -->

Then build the example cluster and export it. [shop.yaml](https://github.com/setdrift/setdrift-engine/blob/main/examples/tour/shop.yaml) is a small shop app, a ConfigMap, a Secret, a claim and a one-off Job that writes data to it. The `scale` line is a change made by hand after the apply, like the ones every cluster collects.

```sh
kind create cluster --name setdrift-tour
kubectl apply -f https://raw.githubusercontent.com/setdrift/setdrift-engine/main/examples/tour/shop.yaml
kubectl -n shop scale deployment web --replicas=3
kubectl -n shop wait --for=condition=complete job/seed --timeout=120s
setdrift export --context kind-setdrift-tour -o ./tour-export
```

If you have Helm, add a Helm release before exporting, to see what an export says about one.

```sh
helm install podinfo oci://ghcr.io/stefanprodan/charts/podinfo --version 6.9.2 -n shop
```

## The 30-second tour

`export` prints what it found.

```
Exported 52 resources from kind-setdrift-tour to ./tour-export
Platform: kind, high confidence. The export is generic (any cluster); platform/local/kind/ is the set for the same platform.
  unmanaged  11
  platform   41  (created by kind and kubeadm; in the snapshot, not written)

Applied by hand, so this export is their only record (no GitOps tool, Helm or operator manages them): 6
  (cluster-scoped)       1  Namespace shop
  shop                   5  ConfigMap web-config; Deployment web; PersistentVolumeClaim orders-data; Secret db; Servi…

1 resource with redacted values was written to redacted/ for reference only.

To recreate this cluster elsewhere, follow tour-export/clusters/kind-setdrift-tour/RESTORE.md: 1 resource to supply secret values for, 1 volume to bring over.
```

Everything for the cluster is in `tour-export/clusters/kind-setdrift-tour/`. Read it in this order.

| Where | What it is | Read it when |
|---|---|---|
| `README.md` | What the export found, who manages what | First |
| `kustomization.yaml`, `namespaces/`, `cluster-scoped/` | The cluster's configuration, as files you can apply | You want it in Git, or on another cluster |
| `redacted/` | Resources whose secret values were taken out | You'll recreate the cluster |
| `RESTORE.md` | The checklist for recreating the cluster somewhere else | You'll recreate the cluster |
| `cluster_profile/` | The cluster as infrastructure, networks, nodes, components | You'll build a new cluster to restore to |
| `platform/local/kind/` | The export plus what only kind serves | You'll restore onto the same platform |
| `../../snapshots/` | Everything export read, for comparing later | You want to know what changed |

## README.md, what's in your cluster

The README sorts everything by who manages it.

| Namespace | Resources |
|---|---|
| (cluster-scoped) | Namespace shop |
| shop | ConfigMap web-config; Deployment web; PersistentVolumeClaim orders-data; Secret db; Service web |

**Takeaway.** All six were applied by hand, so nothing but this export records them. In your cluster, these are the resources to move into Git first, and `export --unmanaged-only` writes just them. Anything a GitOps tool or Helm manages is already recorded somewhere, and the README says which. [Who manages a resource](who-manages-a-resource.md) explains how SetDrift tells.

## The manifests

`kustomization.yaml` lists everything that can be applied, CRDs first, then cluster-scoped resources, then each namespace. The files are plain manifests, with what the cluster added on its own taken out, like status and defaults.

The Deployment says `replicas: 3`, not the 1 in `shop.yaml`, since the export records what the cluster runs, not what was first applied. The `seed` Job isn't there at all, because it had finished, and applying it would run it again.

**Takeaway.** This is your cluster as code. `kubectl apply --server-side -k .` recreates it, and it applies to any Kubernetes cluster, since what only kind serves is kept apart in `platform/local/kind/`.

## Secrets

SetDrift never writes a secret value. The `db` Secret is written to `redacted/` with each value replaced by a placeholder, in no `kustomization.yaml`, so applying the export can't overwrite real values with it.

```yaml
apiVersion: v1
data:
  password: U0VURFJJRlRfUkVEQUNURUQ=   # SETDRIFT_REDACTED
  username: U0VURFJJRlRfUkVEQUNURUQ=
kind: Secret
```

**Takeaway.** For each redacted Secret, you decide where its values come from. `external-secrets/` has an ExternalSecret for each, for when you keep them in a secret manager. [Handle secrets](secrets.md) has the options.

## RESTORE.md, recreating the cluster

RESTORE.md is the checklist for recreating the cluster somewhere else, for disaster recovery, a move to another cluster or cloud, or replacing a cluster to upgrade it. Its steps are numbered, with the commands to run, and it only lists what applies to this cluster.

| Step | In this example |
|---|---|
| What the platform creates | 41 objects kind makes in every cluster, left out, nothing to do |
| Storage classes | The `standard` class the claim asks for, created on the new cluster if it lacks it |
| Volumes | `orders-data` is on kind's local disk, which no other cluster can reach, so its data is copied by the app, or not at all |
| Apply the export | One command |
| Secrets | Supply the `db` Secret's values, copied from the source, filled in by hand, or from a secret manager |
| After applying | The `seed` Job ran once, run it again only if the new cluster needs its data |
| Check the result | `setdrift verify` |

With the Helm release, there's one more step before applying, installing podinfo with Helm, with its chart's repository to fill in, since Helm doesn't record where a chart came from.

**Takeaway.** In this example, two things are yours to decide, where the `db` Secret's values come from and whether the orders data needs to come along. Everything else comes over with one apply. In your cluster, RESTORE.md is that list, the decisions only you can make, then the commands.

## cluster_profile/, the cluster as infrastructure

The profile describes what the cluster runs on, as found in the cluster itself.

| | |
|---|---|
| Platform | kind, self-managed (high confidence) |
| Kubernetes | v1.34.0 |
| Pod network | 10.244.0.0/16 |
| Service network | 10.96.0.0/16 |

On a cloud, it has the account, region, zones, node types, VPC and subnets too.

**Takeaway.** Build the new cluster to match before restoring, the same Kubernetes version or newer, networks that don't overlap anything the apps reach, and nodes big enough. To move to another cloud, see [`translate`](commands/translate.md).

## platform/local/kind/, the platform's own

This directory is the export plus what only kind serves. Here that's nothing, so the two are the same. On EKS, it would hold what only EKS serves, like its node classes.

**Takeaway.** Restoring onto the same platform, apply this directory. Anywhere else, apply the cluster's directory.

## Snapshots, comparing later

`snapshots/`, beside `clusters/`, keeps everything export read, the same moment as the files, for [`diff`](commands/diff.md) to compare with the cluster later. Change something, then diff.

```sh
kubectl -n shop set image deployment/web web=public.ecr.aws/nginx/nginx:1.26
setdrift diff ./tour-export/snapshots/*.pb kind-setdrift-tour
```

```
Differ (1):
  shop/web Deployment.apps  (applied by hand)
      spec.template.spec.containers[web].image: "public.ecr.aws/nginx/nginx:1.27" -> "public.ecr.aws/nginx/nginx:1.26"
```

**Takeaway.** Every export is a baseline. `diff` against it shows what changed since, and who changed it.

## What next

| To | Run |
|---|---|
| Check the export applies cleanly | `setdrift verify --context kind-setdrift-tour ./tour-export` |
| See what changed since | `setdrift diff ./tour-export/snapshots/<file>.pb kind-setdrift-tour` |
| Move it to another cloud | [`setdrift translate`](commands/translate.md) |
| Keep it | Commit `tour-export/` to Git. Its `.gitignore` keeps the snapshots out |

On the example cluster, `verify` passes, 9 of 9 resources would not change, with the redacted Secret skipped on purpose. [Output](output.md) describes every file an export can write.

When you're done, delete the cluster.

```sh
kind delete cluster --name setdrift-tour
```
