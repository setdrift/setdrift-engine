# Output

What `setdrift export` writes.

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

## Platform identification

The cluster's directory applies to any
Kubernetes cluster. SetDrift also works out the platform it ran on (EKS,
AKS, GKE, OpenShift, k3s, RKE2, kubeadm, kind, minikube, Docker Desktop, and
the cloud underneath) from the kubeconfig, the API server and the cluster's
objects, and acts only when the evidence is clear. Then
`platform/<cloud>/<distribution>/` (or `platform/<cloud>/vm/` for a
self-managed cluster on a cloud's VMs) is an overlay of the generic set plus
the objects only that platform serves, with its own README and, for AWS and
EKS, its own restore steps (`platform/aws/RESTORE.md`,
`platform/aws/eks/RESTORE.md`).

## CRDs

CRDs are in `crds/`, first in the root `kustomization.yaml`. Argo CD and
Flux apply them before the resources that use them; with kubectl, apply
`crds/` first (the cluster's README.md has the commands), because kubectl
rejects a custom resource whose definition does not exist yet. Custom
resources whose definition the export lacks are listed in RESTORE.md, to
install first.

## Helm

Helm does not record which repository a chart came from. RESTORE.md
takes it from Argo CD or Flux installing the same chart, or from a file
passed with `--helm-repos`, and lists the charts left to fill in; see
[contrib/helm](../contrib/helm), which also has a post-renderer that records
the repository at install time (`setdrift.io/chart-repo`).

## Storage

`--keep-source-defaults` keeps the storage class a claim got
from this cluster's default class (normally left out), so claims ask for the
same class by name in any cluster. RESTORE.md creates each class the claims
use when the target lacks it, from this cluster's definition, and says how
to point it at another platform's provisioner. Volumes another cluster can
reach (EBS, and other CSI volumes) get a PersistentVolume in
`restore/volumes/`, bound in advance to its claim; volumes on a node's own
disk (local, hostPath, kind's local-path) are reported as not portable.

## Exporting again

`export` writes into an empty directory. To export again into a repository,
pass `--force`: it replaces only `clusters/<cluster>/`, so resources gone
from the cluster don't leave stale files, and keeps everything else,
including the earlier snapshots and verify reports. It asks first; `--yes`
skips the question in scripts.
