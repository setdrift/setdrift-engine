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
  the AWS profile the kubeconfig context uses), read-only:
  `eks:DescribeCluster`, `eks:ListPodIdentityAssociations`,
  `eks:DescribePodIdentityAssociation`, `eks:ListAccessEntries`,
  `eks:DescribeAccessEntry`, `eks:ListAssociatedAccessPolicies`,
  `eks:ListAddons`, `eks:DescribeAddon`, `iam:GetRole`,
  `ec2:DescribeVolumes`, `ec2:DescribeSubnets` and `ec2:DescribeVpcs`. `eks:DescribeCluster` is checked first: without it,
  or with the wrong cluster name or region, the export stops before reading
  the cluster. Any other refused call is recorded and listed in RESTORE.md's
  AWS settings, and the export goes on without that part. `iam:GetRole` is
  only called for roles in the cluster's own AWS account, so a ServiceAccount
  annotation can't make the export read any other role.

## Least-privilege permissions

What each command needs, and nothing more. Everything is read-only, except
`verify`'s dry-run apply, which changes nothing either.

### Kubernetes (RBAC)

Every signed-in user can already read the API's discovery documents and
version, and ask who they are and what they may do (SelfSubjectReview,
SelfSubjectAccessReview, SelfSubjectRulesReview: the built-in
`system:basic-user`, `system:discovery` and `system:public-info-viewer`
roles). SetDrift uses these to explain a refusal; they need no grant.

| Command | Verbs | Resources | For |
|---|---|---|---|
| `export`, and the live side of `diff` | `get`, `list` | every resource type to export, in the namespaces to read; cluster-wide for cluster-scoped types | reading the objects; `get` fetches single objects, like a Pod nothing owns or an owner that wasn't listed |
| | `get`, `list` | `secrets` | optional: only the key names are kept, each value is dropped as soon as it is read; without it, Secrets are skipped and named in the summary |
| | `list` | `namespaces`, cluster-wide | optional: finding the namespaces you can read; without it, the kubeconfig context's namespace or `-n` is read |
| | `get` | `namespaces`, each one given with `-n` | checking `-n` before anything is read |
| | `list` | `nodes` | optional: identifying the platform; without it, the platform is identified from less evidence |
| `verify` | `get` | every resource the export builds | comparing it with the live object |
| | `patch` | every resource the export builds | the server-side dry-run apply (`dryRun=All`, the cluster changes nothing) |
| | `get` | `namespaces`: `kube-system`, and each one given with `-n` | optional: telling whether this is the export's own cluster; checking `-n` |
| | `list` | `nodes` | optional: choosing the platform set with `--set auto` |
| `diff` of two snapshots or directories, `export --from-dir`, `snapshot json`, `helm-postrender` | none | | they read no cluster |

The built-in roles cover most of it:

- **`view`** is enough for `export` and `diff`, except Secrets and RBAC objects
  (Roles, RoleBindings, ClusterRoles), which it doesn't allow: those are
  skipped and named. On EKS, the access policy `AmazonEKSViewPolicy` is the
  same.
- **`edit`** is enough for `verify`, except RBAC objects, which only `admin`
  may patch.

A ClusterRole for `export` that reads everything, Secrets included (their
values are dropped as soon as they are read):

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

### AWS (IAM), for `export --aws`

Only `export --aws` calls AWS; no other command or flag does.

| Action | Resource | For |
|---|---|---|
| `eks:DescribeCluster` | the cluster | its ARN, version and OIDC issuer; checked first, the export stops without it |
| `eks:ListPodIdentityAssociations` | the cluster | the Pod Identity associations |
| `eks:DescribePodIdentityAssociation` | each Pod Identity association | the role each one gives a ServiceAccount |
| `eks:ListAccessEntries` | the cluster | the access entries |
| `eks:DescribeAccessEntry` | each access entry | its type and Kubernetes groups |
| `eks:ListAssociatedAccessPolicies` | each access entry | its access policies and their scope |
| `eks:ListAddons` | the cluster | the add-ons |
| `eks:DescribeAddon` | each add-on | its version, its role, and whether it has configuration (the values are never kept) |
| `iam:GetRole` | roles in the cluster's own account | the trust policy of each role an IRSA annotation or Pod Identity association names, for RESTORE.md |
| `ec2:DescribeVolumes` | `*` (this action takes no resource) | the type, size, IOPS, encryption and zone of each EBS volume behind a PersistentVolume |
| `ec2:DescribeSubnets` | `*` (this action takes no resource) | the range, zone and free addresses of each subnet the cluster profile names (the cluster's own, its nodes', pods' and load balancers') |
| `ec2:DescribeVpcs` | `*` (this action takes no resource) | the address ranges of the cluster's VPC, for the cluster profile |

A policy with only those, for one cluster (replace `REGION`, `ACCOUNT` and
`CLUSTER`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Cluster",
      "Effect": "Allow",
      "Action": ["eks:DescribeCluster", "eks:ListPodIdentityAssociations", "eks:ListAccessEntries", "eks:ListAddons"],
      "Resource": "arn:aws:eks:REGION:ACCOUNT:cluster/CLUSTER"
    },
    {
      "Sid": "ClusterParts",
      "Effect": "Allow",
      "Action": ["eks:DescribePodIdentityAssociation", "eks:DescribeAccessEntry", "eks:ListAssociatedAccessPolicies", "eks:DescribeAddon"],
      "Resource": [
        "arn:aws:eks:REGION:ACCOUNT:podidentityassociation/CLUSTER/*",
        "arn:aws:eks:REGION:ACCOUNT:access-entry/CLUSTER/*",
        "arn:aws:eks:REGION:ACCOUNT:addon/CLUSTER/*"
      ]
    },
    {
      "Sid": "Roles",
      "Effect": "Allow",
      "Action": "iam:GetRole",
      "Resource": "arn:aws:iam::ACCOUNT:role/*"
    },
    {
      "Sid": "Describe",
      "Effect": "Allow",
      "Action": ["ec2:DescribeSubnets", "ec2:DescribeVolumes", "ec2:DescribeVpcs"],
      "Resource": "*"
    }
  ]
}
```
