# Google Cloud Permissions

SetDrift calls no Google Cloud API: on GKE it reads the cluster through the Kubernetes API only, with the kubeconfig you give it. What it needs on Google Cloud is a way to get that kubeconfig, and the [Kubernetes permissions](kubernetes.md), which GKE grants through Kubernetes RBAC, IAM, or both: a request is allowed when either allows it.

## Getting a kubeconfig

`gcloud container clusters get-credentials` needs, on the cluster's project:

| Permission | For |
|---|---|
| `container.clusters.get` | the cluster's endpoint and certificate for the kubeconfig |

The predefined role **Kubernetes Engine Cluster Viewer** (`roles/container.clusterViewer`) has it, and nothing inside the cluster.

## Kubernetes access on GKE

- **With Kubernetes RBAC (the narrowest) -** give the account only `roles/container.clusterViewer` in IAM, and bind it to `setdrift-export` or `setdrift-verify` from the [Kubernetes page](kubernetes.md) with a ClusterRoleBinding (the subject is the account's email, or a Google Group with Google Groups for RBAC).
- **With IAM -** GKE turns each Kubernetes request into an IAM permission named `container.<resource>.<verb>`. A `get` of one object needs `.get`, a `list` needs `.list`, and a `patch` needs `.update`. For what SetDrift uses:

| Permission | Kubernetes request | For |
|---|---|---|
| `container.<resource>.list`, for every type | `list` | `export` and `diff`: every object (`container.pods.list`, `container.deployments.list`, `container.configMaps.list`, ...) |
| `container.<resource>.get`, for every type | `get` | single objects: an owner that wasn't listed, a Pod nothing owns |
| `container.secrets.list` | `list` `secrets` | Secrets with key names only (each value is dropped as soon as it is read) and Helm's release records |
| `container.nodes.list` | `list` `nodes` | identifying the platform, the node summary and the cluster profile |
| `container.namespaces.list`, `container.namespaces.get` | `list`, `get` `namespaces` | finding the namespaces you can read; checking `-n` |
| `container.thirdPartyObjects.list`, `container.thirdPartyObjects.get` | `list`, `get` on custom resources | custom resources (CRD instances) |
| `container.selfSubjectAccessReviews.create`, `container.selfSubjectRulesReviews.create` | `create` | telling which namespaces you can read, and explaining a refusal (Kubernetes RBAC allows these to every user already) |
| `container.<resource>.update`, for what the export holds | `patch` | `verify`'s server-side apply with `dryRun=All`; GKE maps `patch` to `update`, and does not say whether a dry-run changes that, so assume it does not |

The predefined roles cover most of it:

- **Kubernetes Engine Viewer** (`roles/container.viewer`) covers `export` and `diff` except Secrets, which it does not include: they are skipped and named. Add `container.secrets.list` with a custom role to read them.
- **Kubernetes Engine Developer** (`roles/container.developer`) covers `verify`. It also allows real changes, so for a read-mostly account use Kubernetes RBAC as above.

## Templates

Custom role files for `gcloud iam roles create`, in [deploy/permissions/gcp/](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/gcp):

| Template | Holds | For |
|---|---|---|
| [setdrift-cluster-access-role.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/gcp/setdrift-cluster-access-role.yaml) | `container.clusters.get` | the kubeconfig, with the [Kubernetes templates](kubernetes.md#templates) applied in the cluster: the narrowest setup |
| [setdrift-secrets-reader-role.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/gcp/setdrift-secrets-reader-role.yaml) | `container.secrets.list` | with `roles/container.viewer`, `export` and `diff` through IAM, Secrets included |

```sh
gcloud iam roles create setdrift_cluster_access --project=PROJECT --file=setdrift-cluster-access-role.yaml
gcloud projects add-iam-policy-binding PROJECT --member=user:EMAIL --role=projects/PROJECT/roles/setdrift_cluster_access
```
