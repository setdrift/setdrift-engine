# AWS Permissions

What SetDrift needs on AWS, at the level of each IAM action, with the feature each one feeds. There are two parts, the AWS API calls `export --aws` makes and how EKS gives the [Kubernetes permissions](kubernetes.md) every command needs.

## AWS API calls

Only `export --aws` calls AWS; no other command or flag does. It reads with your AWS credentials (the default chain, or the AWS profile the kubeconfig context uses), and every call is read-only.

| Action | Resource | Feeds | Without it |
|---|---|---|---|
| `eks:DescribeCluster` | the cluster | its ARN, version and OIDC issuer for RESTORE.md; the cluster profile's VPC, subnets, security groups, service network, IP family and API endpoint access | the export stops before reading the cluster, as nothing else makes sense without it |
| `eks:ListPodIdentityAssociations` | the cluster | the Pod Identity associations, for RESTORE.md's identity step | recorded, and RESTORE.md lists it as not read |
| `eks:DescribePodIdentityAssociation` | each Pod Identity association | the role each one gives a ServiceAccount | the same |
| `eks:ListAccessEntries` | the cluster | the access entries, for RESTORE.md | the same |
| `eks:DescribeAccessEntry` | each access entry | its type and Kubernetes groups | the same |
| `eks:ListAssociatedAccessPolicies` | each access entry | its access policies and their scope | the same |
| `eks:ListAddons` | the cluster | the add-ons, for RESTORE.md | the same |
| `eks:DescribeAddon` | each add-on | its version, its role, and whether it has configuration (the values are never kept) | the same |
| `iam:GetRole` | roles in the cluster's own account | the trust policy of each role an IRSA annotation or Pod Identity association names, for RESTORE.md | the same |
| `ec2:DescribeVolumes` | `*` (this action takes no resource) | the type, size, IOPS, encryption and zone of each EBS volume behind a PersistentVolume, for RESTORE.md's volumes step | the same |
| `ec2:DescribeSubnets` | `*` (this action takes no resource) | the range, zone and free addresses of each subnet the cluster profile names (the cluster's own, its nodes', pods' and load balancers') | the profile keeps the subnet IDs, and the nodes' addresses as a lower bound |
| `ec2:DescribeVpcs` | `*` (this action takes no resource) | the address ranges of the cluster's VPC, for the cluster profile | the profile keeps the VPC ID only |

`eks:DescribeCluster` is checked first, and without it, or with the wrong cluster name or region, the export stops before reading the cluster. Any other refused call is recorded once (not once per resource), listed in RESTORE.md's AWS settings, and the export goes on without that part. `iam:GetRole` is only called for roles in the cluster's own AWS account, so a ServiceAccount annotation can't make the export read any other role.

A policy with only those, for one cluster (replace `REGION`, `ACCOUNT` and `CLUSTER`). EC2's Describe actions can't be limited to resources, so they alone take `*`.

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

## Kubernetes access on EKS

EKS turns an IAM identity into a Kubernetes user through an access entry (or, on older clusters, the `aws-auth` ConfigMap). What that user may do is the [Kubernetes permissions](kubernetes.md), granted in one of two ways.

- **An access policy** on the access entry, cluster-wide or for some namespaces. EKS defines these, and their contents can't be changed and you can't make your own.

  | Access policy | Covers |
  |---|---|
  | `AmazonEKSViewPolicy` | `export` and `diff`, except Secrets and RBAC objects, which are skipped and named (like Kubernetes' `view`) |
  | `AmazonEKSAdminViewPolicy` | `export` and `diff`, everything including Secrets (their values are dropped as soon as they are read) |
  | `AmazonEKSSecretReaderPolicy` | Secrets only, so with `AmazonEKSViewPolicy` `export` reads Secrets too |
  | `AmazonEKSEditPolicy` | `verify` (it allows `patch`), except RBAC objects |

- **Kubernetes groups** on the access entry, bound with a RoleBinding or ClusterRoleBinding to a role of your own, such as `setdrift-export` or `setdrift-verify` from the [Kubernetes page](kubernetes.md). This is the way to grant exactly what SetDrift uses and nothing more.

Getting a kubeconfig with `aws eks update-kubeconfig` needs `eks:DescribeCluster` on the cluster, the same permission `export --aws` checks first. SetDrift then signs in with the kubeconfig's `aws eks get-token` credentials, which need no further IAM permission.

## Templates

In [deploy/permissions/aws/](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/aws).

| Template | Creates |
|---|---|
| [setdrift-export-iam-policy.json](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/aws/setdrift-export-iam-policy.json) | the policy above, with `REGION`, `ACCOUNT` and `CLUSTER` to replace |
| [setdrift-export.cfn.json](https://github.com/setdrift/setdrift-engine/blob/main/deploy/permissions/aws/setdrift-export.cfn.json) | a CloudFormation stack with the policy for one cluster, attached to a role if you name one, and optionally an EKS access entry for a principal with the `setdrift-export` or `setdrift-verify` Kubernetes group |

```sh
aws cloudformation deploy --stack-name setdrift-export --template-file setdrift-export.cfn.json \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides ClusterName=prod RoleName=platform-ci PrincipalArn=arn:aws:iam::111122223333:role/platform-ci
```

The access entry gives the group; apply the group's role in the cluster from the [Kubernetes templates](kubernetes.md#templates). A principal that already has an access entry can't get a second one, so add the group to its existing entry instead.
