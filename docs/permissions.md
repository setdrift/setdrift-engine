# Permissions

`export`, `diff` and `verify` follow the same rules, and every refusal names
who the cluster says you are (`system:serviceaccount:team:ci`, an IAM role
on EKS) and how to fix it. `-n` limits all three to some namespaces.

| Your access | Without `-n` | With `-n X` |
|---|---|---|
| The whole cluster | Everything is read | `X` is read |
| Some namespaces, and you can list namespaces | The namespaces you can read are read, and the rest are named as not readable | `X` is read if you can read it |
| Some namespaces, and you can't list namespaces | The kubeconfig context's namespace is read; with none set, you're asked for `-n` | `X` is read if you can read it |
| None | An error naming what you need | An error naming what you need |

Reading only some namespaces, with `-n` or because that is all you can
read, leaves out cluster-scoped resources (ClusterRoles, CRDs,
StorageClasses), except the Namespace objects of the namespaces read.

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
  their own message, within 15 seconds, before anything is written. A
  context that turns off the API server's certificate check
  (`insecure-skip-tls-verify`) gets a warning: anything on the network path
  could answer in the cluster's place.
- **`export --aws`** reads with your AWS credentials (the default chain, or
  the AWS profile the kubeconfig context uses), read-only; every call is in
  [AWS](permissions-aws.md). `eks:DescribeCluster` is checked first: without
  it, or with the wrong cluster name or region, the export stops before
  reading the cluster. Any other refused call is recorded and listed in
  RESTORE.md's AWS settings, and the export goes on without that part.

## Least-privilege permissions

Every permission SetDrift uses, each tied to the command and the output
that needs it and to what happens without it, has its own page:

- **[Kubernetes](permissions-kubernetes.md):** each API group, resource and
  verb (`list` on every type, `get` for single objects, `list` `nodes`,
  `patch` for `verify`'s dry-run), on any cluster.
- **[AWS](permissions-aws.md):** each IAM action `export --aws` calls
  (`eks:DescribeCluster`, `ec2:DescribeSubnets`, ...), and how EKS access
  entries and access policies give the Kubernetes permissions.
- **[Azure](permissions-azure.md):** no Azure API is called; the
  kubeconfig actions, and the AKS data actions for a cluster with Azure RBAC
  for Kubernetes.
- **[Google Cloud](permissions-gcp.md):** no Google Cloud API is called;
  the kubeconfig permission, and the GKE IAM permissions that can grant the
  Kubernetes access.

Each page ends with templates to apply, in
[deploy/permissions/](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions): Kubernetes roles and
bindings, an IAM policy and a CloudFormation stack, an ARM template of Azure
custom roles, and Google Cloud custom roles.

In short:

| Command | Kubernetes | Cloud |
|---|---|---|
| `export` | `get`, `list` on what it exports; `list` on Secrets, namespaces and nodes, each optional | with `--aws`, the AWS actions; none otherwise |
| `diff` | the same as `export` for each live side; none for snapshots and directories | none |
| `verify` | `get` and `patch` on what the export holds | none |
| `export --from-dir`, `snapshot json`, `helm-postrender` | none | none |
