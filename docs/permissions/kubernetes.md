# Kubernetes permissions

Every Kubernetes API request SetDrift makes, by API group, resource and verb, with the command that makes it, what it feeds and what happens without it. This applies to any cluster; on EKS, AKS and GKE the cloud's own access controls can grant the same permissions (see [AWS](aws.md), [Azure](azure.md) and [Google Cloud](gcp.md)).

SetDrift only reads, except `verify`'s server-side apply with `dryRun=All`, which the API server checks and then discards, so the cluster changes nothing. No command uses `watch`, `create` (other than the self-review requests below, which store nothing), `update` or `delete`.

## Every command that reads a cluster

`export`, the live side of `diff`, and `verify`.

| API group | Resource or path | Verb | For | Without it |
|---|---|---|---|---|
| (none) | `/version` | `get` | the Kubernetes version; the first request, which also checks the kubeconfig works | the command stops, as the cluster can't be reached |
| (none) | `/api`, `/apis` | `get` | discovery, which resource types the cluster serves, read once as aggregated discovery | the command stops |
| `authentication.k8s.io` | `selfsubjectreviews` | `create` | who the cluster says you are, named in every refusal (Kubernetes 1.28 and later) | the name comes from the refusal message instead |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create` | telling which namespaces you can read, by asking whether you may list ConfigMaps, Pods or Deployments in each | an unanswered check counts as yes, and a refused list is reported |
| `authorization.k8s.io` | `selfsubjectrulesreviews` | `create` | saying what you may list in a namespace, when a type is refused there | the refusal is reported without the hint |

Every signed-in user already has these. The built-in `system:discovery`, `system:public-info-viewer` and `system:basic-user` ClusterRoles allow them, so they need no grant.

## `export`, and the live side of `diff`

| API group | Resource | Verb | Scope | For | Without it |
|---|---|---|---|---|---|
| every group | every resource type the cluster serves that can be listed, except the runtime state below | `list` | each namespace read; cluster-wide for cluster-scoped types | the objects themselves, for the export's manifests, the snapshot and the diff | that type is skipped where refused, and named in the summary |
| every group | the same | `get` | the same | fetching single objects, a controller owner that wasn't in the lists (up to 200), and a Pod, ReplicaSet, Job, ControllerRevision or EndpointSlice that nothing collected owns | the object is left out and named |
| (core) | `pods` | `list` | each namespace read | listed metadata-only first (names, owners, labels), so Pods a controller owns are never read in full | Pods are skipped and named |
| `apps` | `replicasets`, `controllerrevisions` | `list` | the same | the same, metadata-only first | skipped and named |
| `batch` | `jobs` | `list` | the same | the same, and a Job a CronJob owns is left out while one that ran once is marked | skipped and named |
| `discovery.k8s.io` | `endpointslices` | `list` | the same | the same, and those a Service owns are left out | skipped and named |
| (core) | `secrets` | `list` | each namespace read | Secrets with their key names only, every value dropped as soon as it is read; Helm's release records (type `helm.sh/release.v1`), read only for their chart name and version | Secrets are skipped and named; objects still show as Helm-managed by their labels, but the releases' chart versions are not read |
| (core) | `namespaces` | `list` | cluster-wide | finding the namespaces you can read, first with a one-item list | the kubeconfig context's namespace is read, or `-n` is asked for |
| (core) | `namespaces` | `get` | each one given with `-n` | checking `-n` before anything is read or written | the command stops, naming the RoleBinding that would grant it |
| (core) | `nodes` | `list` | cluster-wide | identifying the platform (a few nodes), and the node summary and cluster profile (count, instance types, zones, pools, capacity, pod ranges, address ranges, cloud account) | the platform is identified from less evidence; the node summary and those profile values are left out |
| (core) | `configmaps` | `list` | `kube-system`, `kube-flannel` | the cluster profile, from kubeadm's, kube-proxy's, cilium's, flannel's and CoreDNS's settings (networks, proxy mode, cluster domain) | those profile values read "not found" |
| (core) | `services` | `list` | `default`, `kube-system` | the cluster profile, the service network's start (`kubernetes`) and the cluster DNS address (`kube-dns`) | those profile values read "not found" |
| `networking.k8s.io` | `servicecidrs` | `list` | cluster-wide | the cluster profile's service network (Kubernetes 1.33 and later) | read from kubeadm or the EKS API instead, if there |
| `apps` | `deployments`, `daemonsets`, `statefulsets` | `list` | each namespace read | also the cluster profile's components and their settings (network plugin, ingress, DNS, autoscaling) | those components are not listed |

Types that hold runtime state rather than configuration are not listed at all, whatever the permissions. These are Events, Nodes as objects (read only for the summary above), CSINodes, VolumeAttachments, CSIStorageCapacities, ResourceSlices, Karpenter NodeClaims, CNINodes, NodeDiagnostics, CertificateSigningRequests, Leases, PodMetrics and NodeMetrics, IPAddresses and ComponentStatuses.

Reading only some namespaces, with `-n` or because that is all you can read, leaves out cluster-scoped types (ClusterRoles, CRDs, StorageClasses), except the Namespace objects of the namespaces read.

## `verify`

| API group | Resource | Verb | Scope | For | Without it |
|---|---|---|---|---|---|
| every group | every resource the export builds | `get` | its namespace, or cluster-wide | the live object, to compare; a missing one is reported as not in the cluster, without an apply | counted as not checked, and the run fails for it, naming the permission |
| every group | every resource the export builds | `patch` | the same | the server-side apply with `dryRun=All` and SetDrift's field manager, which gives what the object would become | the same |
| (core) | `namespaces` | `get` | `kube-system`, and each one given with `-n` | telling whether the cluster is the export's own (by `kube-system`'s uid); checking `-n` | the check is skipped; `-n` is reported |
| (core) | `nodes` | `list` | cluster-wide | choosing the platform set with `--set auto` | the generic set is verified |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create` | cluster-wide, stores nothing | asking before the dry-runs whether you may patch the export's types, so a run with no `patch` at all stops at once with the role to add | each resource is tried and counted as not checked |

`verify` never needs `create`, since a resource missing from the cluster is found with `get` and reported, not applied.

## Commands that read no cluster

`diff` of two snapshots or directories, `export --from-dir`, `snapshot json`, `helm-postrender` and `completion` make no Kubernetes requests and need no permissions.

## Built-in roles

- **`view`** covers `export` and `diff` except Secrets and RBAC objects (Roles, RoleBindings, ClusterRoles, ClusterRoleBindings), which it does not allow, so those are skipped and named, and Helm releases' chart versions are not read.
- **`edit`** covers `verify` except RBAC objects, which only `admin` may patch.

## A role with exactly these

For `export` and `diff`, cluster-wide, Secrets included (their values are dropped as soon as they are read).

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: setdrift-export
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["get", "list"]
```

For `verify`, add `patch` on what the export holds. The dry-run changes nothing, but the API server checks `patch` as it would for a real apply.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: setdrift-verify
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["get", "list", "patch"]
```

For one namespace, the same rules in a Role, with a RoleBinding in that namespace; cluster-scoped types are then not read.

## Templates

Ready to apply, each a role and a binding to the group of the same name (`setdrift-export` or `setdrift-verify`), in [deploy/permissions/kubernetes/](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/kubernetes).

| Template | Grants | For |
|---|---|---|
| [setdrift-export.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/kubernetes/setdrift-export.yaml) | `get`, `list` on everything, cluster-wide | `export`, `diff` |
| [setdrift-verify.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/kubernetes/setdrift-verify.yaml) | `get`, `list`, `patch` on everything, cluster-wide | `verify` (and `export`, `diff`) |
| [setdrift-export-namespace.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/kubernetes/setdrift-export-namespace.yaml) | the same as `setdrift-export`, in one namespace | `export -n`, `diff -n` |
| [setdrift-verify-namespace.yaml](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/kubernetes/setdrift-verify-namespace.yaml) | the same as `setdrift-verify`, in one namespace | `verify -n` |

```sh
kubectl apply -f https://raw.githubusercontent.com/setdrift/setdrift-engine/main/deploy/permissions/kubernetes/setdrift-export.yaml
kubectl apply -n shop -f https://raw.githubusercontent.com/setdrift/setdrift-engine/main/deploy/permissions/kubernetes/setdrift-export-namespace.yaml
```

Put the people or pipelines who run SetDrift in the group, or change the binding's subject to name them. On EKS, name the group in their access entry ([AWS](aws.md)); on GKE, use a Google Group or an email ([Google Cloud](gcp.md)); on AKS, an Entra ID group's object ID ([Azure](azure.md)).
