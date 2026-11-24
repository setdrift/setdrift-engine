# Azure permissions

SetDrift calls no Azure API: on AKS it reads the cluster through the Kubernetes API only, with the kubeconfig you give it. What it needs on Azure is a way to get that kubeconfig, and the [Kubernetes permissions](kubernetes.md), which AKS grants in one of two ways depending on how the cluster is set up.

## Getting a kubeconfig

`az aks get-credentials` needs, on the cluster:

| Action | For |
|---|---|
| `Microsoft.ContainerService/managedClusters/read` | reading the cluster |
| `Microsoft.ContainerService/managedClusters/listClusterUserCredential/action` | the user kubeconfig, with which you sign in as yourself through Microsoft Entra ID |

The built-in role **Azure Kubernetes Service Cluster User Role** has exactly these two.

## Kubernetes access on AKS

- **A cluster with Kubernetes RBAC (the default) -** bind your Entra ID user or group to `setdrift-export` or `setdrift-verify` from the [Kubernetes page](kubernetes.md) with a ClusterRoleBinding. Nothing else on Azure is needed.
- **A cluster with Azure RBAC for Kubernetes -** Azure decides each Kubernetes request as a data action, named `Microsoft.ContainerService/managedClusters/[<group>/]<resource>/<read|write>`. `read` is what a `get` or `list` needs, and `write` what a `patch` needs. For what SetDrift uses:

| Data action | Kubernetes request | For |
|---|---|---|
| `Microsoft.ContainerService/managedClusters/*/read` | `get`, `list` on every type | `export` and `diff`: every object; a narrower set, like the built-in reader's, skips and names the rest |
| `Microsoft.ContainerService/managedClusters/secrets/read` | `list` `secrets` | Secrets with key names only (each value is dropped as soon as it is read) and Helm's release records |
| `Microsoft.ContainerService/managedClusters/nodes/read` | `list` `nodes` | identifying the platform, the node summary and the cluster profile |
| `Microsoft.ContainerService/managedClusters/namespaces/read` | `list`, `get` `namespaces` | finding the namespaces you can read; checking `-n` |
| `Microsoft.ContainerService/managedClusters/customresources/read` | `get`, `list` on custom resources | custom resources (CRD instances) |
| `Microsoft.ContainerService/managedClusters/*/write` | `patch` on what the export holds | `verify`'s server-side apply with `dryRun=All`. Azure checks it as a write, though the dry-run changes nothing |

The built-in roles cover most of it:

- **Azure Kubernetes Service RBAC Reader** covers `export` and `diff` except Secrets and RBAC objects (Roles, RoleBindings), which it does not allow: those are skipped and named.
- **Azure Kubernetes Service RBAC Writer** covers `verify` except RBAC objects. It also allows real changes, so for a read-mostly account use a custom role.

Azure does not say whether it treats a dry-run `patch` differently from any other; assume it needs `write`, as above. The `*/read` and `*/write` forms are wildcards over every resource; Microsoft's own AKS roles list each data action instead (`.../pods/read`, `.../apps/deployments/read`, ...), and a custom role can do the same where wildcards are not wanted, at the cost of missing the types it doesn't name.

## Templates

[setdrift-roles.json](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/azure/setdrift-roles.json) is an ARM template for a subscription deployment. It defines three custom roles:

| Role | Actions | Data actions | For |
|---|---|---|---|
| SetDrift Cluster Access | the kubeconfig actions | none | clusters with Kubernetes RBAC, with the [Kubernetes templates](kubernetes.md#templates) applied in the cluster |
| SetDrift Export | the kubeconfig actions | `*/read` | `export` and `diff` on clusters with Azure RBAC for Kubernetes |
| SetDrift Verify | the kubeconfig actions | `*/read`, `*/write` | `verify` on clusters with Azure RBAC for Kubernetes |

```sh
az deployment sub create --location eastus --template-file setdrift-roles.json
az role assignment create --assignee <user, group or principal> --role "SetDrift Export" \
  --scope $(az aks show -g <resource group> -n <cluster> --query id -o tsv)
```
