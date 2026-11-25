# Output

What `setdrift export` writes. For a walkthrough of one export and what to take from each part, see [Understanding your export](understanding-your-export.md).

```
.gitignore                  keeps snapshots/ out of Git
snapshots/<time>-<cluster>.pb, .json   what each export read (not in Git)
verify/<time>-<cluster>-report.md, .json   each setdrift verify run
clusters/<cluster>/
  README.md                 what the export found, and how to apply it
  RESTORE.md                the restore playbook
  cluster_profile/README.md, profile.json   the cluster as infrastructure
  kustomization.yaml        everything that can be applied, in order
  crds/<name>.yaml
  cluster-scoped/<kind>[.<group>]/<name>.yaml
  namespaces/<ns>/<kind>[.<group>]/<name>.yaml
  redacted/namespaces/<ns>/<kind>[.<group>]/<name>.yaml.redacted
  external-secrets/namespaces/<ns>/externalsecret.external-secrets.io/<name>.yaml
  cloud-resources/namespaces/<ns>/<kind>.<group>/<name>.yaml   objects that manage real cloud resources (not applied)
  translations/<code>/      written by setdrift translate, e.g. translations/te2g for EKS to GKE (kept when exporting again)
  restore/volumes/<ns>/<claim>.yaml
  restore/storageclasses/<name>.yaml
  platform/<cloud>/<distribution>/   e.g. platform/aws/eks, platform/local/kind
  .setdrift-export          where and when the export was taken, and what enforced NetworkPolicies there
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

## Cluster profile

`cluster_profile/` describes the cluster as infrastructure, where
recreating it on another platform or cloud starts: README.md for people,
profile.json (the snapshot's `inventory.profile`) for tools. SetDrift reads
it from what the cluster already says about itself, so it needs no
permission beyond what export uses:

| Part | Read from |
|---|---|
| Cloud, account, region, zones | The platform classification; the nodes' provider IDs (GCP project, Azure subscription and resource group) and zones; on EKS the cluster ARN and, with `--aws`, the EKS API |
| Pod network | kubeadm's ClusterConfiguration, kube-proxy's `clusterCIDR`, cilium's cluster pool, calico's IPPools, flannel's net-conf.json, kindnet's `POD_SUBNET`; else the range holding the nodes' `spec.podCIDR`s |
| Service network | `ServiceCIDR` objects (Kubernetes 1.33 and later), kubeadm's ClusterConfiguration, the EKS API; the `kubernetes` Service's address shows where it starts |
| Node addresses | The smallest range holding the nodes' internal addresses, per zone: the subnet is that size or larger |
| VPC, subnets, security groups | EKS Auto Mode NodeClasses and Karpenter EC2NodeClasses (as resolved, with zones), VPC CNI ENIConfigs (pod subnets), the AWS Load Balancer Controller's `--aws-vpc-id`, TargetGroupBindings, load balancer annotations on Services and Ingresses, and with `--aws` the EKS API; with `--aws`, EC2 gives each subnet's range, zone and free addresses and the VPC's ranges |
| CNI, kube-proxy | The network plugin's workload and settings (VPC CNI prefix delegation, custom networking; cilium IPAM, routing, kube-proxy replacement; calico encapsulation), kube-proxy's mode |
| DNS | CoreDNS's Corefile (cluster domain, upstreams), the cluster DNS Service, external-dns's `--domain-filter`, the host names Ingresses and Gateway routes serve |
| Storage | StorageClasses, their settings and how many claims use each |
| Components | The images of Deployments, DaemonSets and StatefulSets: network plugins, ingress and load balancer controllers, service meshes, cert-manager, autoscalers, CSI drivers, secret operators, GitOps tools, observability and policy agents |

Each value is listed under "Where each value was found" with its source, so
two sources that disagree both show. A part with nothing found reads "not
found" rather than a guess; when the platform is known (medium or high
confidence), the README adds that platform's default, labelled as not read
from the cluster, such as EKS's service network. profile.json holds only
what was read. A subnet's range is held by the cloud, not the cluster: on
EKS's default VPC CNI the pods take their addresses from the node subnets, so
without `--aws` the profile can say only that the nodes' addresses fall
within a range. Like the rest of an export, it holds settings
only: it reads the objects after every secret value has been replaced. The
directory read (`diff ./export`, `export --from-dir`) leaves
`cluster_profile/` out, as it is not manifests. A directory of manifests
with no system components gets no profile.

## CRDs

CRDs are in `crds/`, first in the root `kustomization.yaml`. Argo CD and
Flux apply them before the resources that use them; with kubectl, apply
`crds/` first (the cluster's README.md has the commands), because kubectl
rejects a custom resource whose definition does not exist yet. Custom
resources whose definition the export lacks are listed in RESTORE.md, to
install first.

## Cloud resources

Some objects create and manage real cloud resources, like an S3 bucket for an ACK `Bucket`. These are the objects of AWS Controllers for Kubernetes (ACK), Azure Service Operator, Config Connector, and Crossplane's AWS, Azure and Google Cloud providers. Export writes them to `cloud-resources/`, which no kustomization lists, so `kubectl apply -k`, Argo CD and Flux never apply them, and `setdrift verify` doesn't check them.

They're held back because applied to a second cluster, its operator would manage the same resource as the source cluster's, and deleting the object from either cluster could delete the resource itself.

- **On the same cloud,** RESTORE.md's "Cloud resources" step moves each one to the new cluster one controller at a time. You set the operator's retain setting on the source cluster's object, delete that object (the resource stays), then apply the object on the new cluster, which takes the resource over.
- **On another cloud,** [`setdrift translate`](commands/translate.md) asks what to do with each one, keep it with the source cluster, leave it out, or for a bucket, write the target's equivalent.

## Values bound to a region or account

Some values work only in the cluster's own region or cloud account, and break when the cluster is rebuilt elsewhere. Export looks for these in every manifest it writes, Secrets and CRDs aside.

| Kind | Example | Bound to |
|---|---|---|
| ECR image | `111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4` | The region and account |
| AWS ARN (ACM, KMS, SQS, SNS, IAM and others) | `arn:aws:kms:us-east-2:111122223333:key/…` | The region and account, IAM's the account only |
| AWS subnet and security group IDs | `subnet-0a1b2c3d4e5f6a7b8`, `sg-0c3d4e5f6a7b8c9d0` | One VPC |
| EKS OIDC issuer | `oidc.eks.us-east-2.amazonaws.com/id/…` | One EKS cluster |
| Azure resource ID | `/subscriptions/…/resourceGroups/acme-shop/…` | The subscription and resource group |
| Azure Key Vault URL | `https://acme-shop-kv.vault.azure.net/` | One Key Vault |
| Artifact Registry image | `us-central1-docker.pkg.dev/acme-shop/acme/shop:1.4` | The region and project |

The cluster's README.md lists each value once under "Bound to one region or account", and RESTORE.md's "Values bound to a region or account" step lists every place one is, by file and line, to change before applying the export in another region, account or cloud. A role named in an IRSA annotation is in the IRSA step instead, with the trust policy to change.

[`setdrift diff`](commands/diff.md#what-counts-as-a-difference) between two clusters in different regions or accounts counts two values of the same kind, like each cluster's subnet IDs, as differing by design.

## NetworkPolicy enforcement

A NetworkPolicy applies on any cluster, but only does anything where a network policy engine enforces it, and whether one runs is a cluster setting. `.setdrift-export` records the engine that enforced them on the source, like Calico, Cilium or GKE Dataplane V2, or that none did. [`setdrift verify`](commands/verify.md) checks the cluster it runs against enforces them too.

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
