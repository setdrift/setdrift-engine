# EKS to AKS

Moving production workloads from EKS on AWS to AKS on Azure means rebuilding everything AWS did for you, storage, load balancers, identity, network policy, secrets, images, and proving nothing changed on the way. This guide walks one production cluster through it with SetDrift, and says at each step what SetDrift does and what stays yours.

## The cluster

acme's shop runs in production on EKS (Auto Mode) in us-east-2, managed partly by Argo CD, partly by hand. Moving it to AKS without a rewrite means each of these.

| In the shop | Why it's hard | What SetDrift does |
|---|---|---|
| A gp3 StorageClass with IOPS and throughput set, and a claim with order data | AKS has no gp3, and its default disk is slower | Writes AKS's Premium SSD v2 under the same name, with the same IOPS and throughput, so claims need no change. The data is yours to copy, RESTORE.md says how |
| An internal NLB | AKS makes a load balancer public unless told otherwise | Writes an internal Azure load balancer, explicitly |
| `reports` reads S3 through IRSA | An AKS Pod has no AWS credentials | A decision. By default the AWS role is kept, and the AKS cluster federated to it, so the code runs unchanged |
| 12 NetworkPolicies enforced by the VPC CNI | An AKS cluster created without a policy engine applies them and enforces nothing | Requires enforcement. `verify` fails on an AKS cluster that doesn't enforce them |
| External Secrets reading AWS Secrets Manager | The store authenticates as the EKS cluster | A decision, keep reading AWS's store or move the secrets to Azure Key Vault yourself. Values never pass through SetDrift |
| An ACK `Bucket`, a real S3 bucket managed from the cluster | Applied to a second cluster, two operators would manage one bucket | Held back from every apply, and a decision, keep it with EKS, leave it out, or write Azure's equivalent |
| Argo CD and Flux apps, and hotfixes applied by hand | The hand-made ones exist nowhere but the cluster | Exports all of them, and names the hand-made ones |
| Images in a private ECR registry | AKS can't pull from ECR without AWS credentials | The commands that copy each image to Azure Container Registry, and the lines that point the manifests at the copies |
| 4 nodes of 2 vCPUs and 4 GiB, Services in 172.20.0.0/16 | The new cluster has to be built to match | An `az aks create` command written from that shape |
| ACM certificate, KMS key and SQS queue ARNs, subnet IDs | They exist only in that AWS account and region | Listed, with where each one is, to change before applying |

The rows through hotfixes were proven on real EKS and AKS clusters. The image commands and the `az aks create` command are written from the export and tested offline, not yet run in a live migration.

## Before you start

You need `setdrift` (see [Install](../install.md)), kubectl, the AWS CLI and the Azure CLI, and a kubeconfig context for each cluster. This guide calls them `eks-prod` and `aks-prod`.

```sh
aws eks update-kubeconfig --name prod --region us-east-2 --alias eks-prod
az aks get-credentials --resource-group shop --name prod --context aks-prod   # once the AKS cluster exists
```

Export reads EKS and changes nothing, see [Permissions](../permissions/index.md) for what each command needs.

## 1. Export from EKS

```sh
setdrift export --context eks-prod -o ./prod-export
```

The cluster's README.md lists what was applied by hand, and the values bound to us-east-2 and the AWS account, the ACM, KMS and SQS ARNs and the subnet IDs, each to change before the move. Secret values are never written.

## 2. Translate for AKS and make the decisions

```sh
setdrift translate ./prod-export --to aks
```

Translate writes the overlay to `prod-export/clusters/prod/translations/te2a` (EKS to AKS), with what has no 1:1 equivalent in `translate-decisions.yaml`. In this cluster that's four decisions.

| Decision | Options | What to choose in production |
|---|---|---|
| `identity/shop/reports` | `keep-source`, `leave-out` | `keep-source` while the app still calls S3. Moving it to Azure Storage is a change to the app, outside SetDrift |
| `secret-store/aws-secrets-manager` | `keep-source`, `equivalent` | `keep-source` to keep reading AWS's store during the move, `equivalent` once the secrets are in Key Vault |
| `cloud-resource/Bucket/shop/orders` | `keep-source`, `equivalent`, `leave-out` | `keep-source` keeps the bucket with EKS. `equivalent` creates a new, empty Azure container, the data is yours to copy |
| `network-policy-enforcement` | `require`, `accept-unenforced` | `require`. `accept-unenforced` is for an emergency, and verify says so every run |

Set `choose` for each and run translate again. The file stays with the export, so the next translation keeps your choices, and asks again if the source changes under one.

## 3. Create the AKS cluster

The translation's RESTORE.md starts with the command, written from the EKS cluster's shape.

```sh
az aks create --resource-group <resource group> --name <new cluster> --location <location> --tier standard \
  --kubernetes-version 1.37 \
  --node-count 4 --node-vm-size <a size with 2 vCPUs and 4 GiB> \
  --zones 1 2 3 \
  --network-plugin azure --network-plugin-mode overlay --service-cidr 172.20.0.0/16 --dns-service-ip 172.20.0.10 \
  --network-dataplane cilium --network-policy cilium \
  --enable-oidc-issuer --enable-workload-identity
```

Cilium enforces the NetworkPolicies, the OIDC issuer is what the kept AWS role trusts, and keeping the Service range means nothing that names a Service address changes. RESTORE.md has the command that lists Azure's VM sizes of 2 vCPUs and 4 GiB. Change anything you want different, SetDrift runs none of it.

## 4. Copy the images

RESTORE.md has an `az acr import` for each image in ECR, signed in with the AWS CLI, and the `images:` entries to add to the overlay's `kustomization.yaml` so the Pods pull the copies. Attach the registry to the cluster with `az aks update --attach-acr`. Images in public registries pull as they are.

## 5. Apply, supply the secrets, bring the data

Follow the translation's RESTORE.md in order. It trusts the new cluster's OIDC issuer in the `reports` role, installs the CRDs and controllers, applies the overlay, then has you supply the secret values and copy the claim's data with Velero or the app's own tools. The overlay is applied like any kustomization.

```sh
kubectl --context aks-prod apply --server-side -k ./prod-export/clusters/prod/translations/te2a
```

## 6. Prove it

`verify` checks that applying the translation again would change nothing on AKS, and that AKS enforces the NetworkPolicies.

```sh
setdrift verify --context aks-prod ./prod-export
```

```
PASSED: 205 of 207 resources would not change (99.0%; 95% required)
Skipped 10: redacted (Secret values were replaced; not applied on purpose)

Would change (2):
  Won't work on this platform (they name another platform's features) (2):
    ServiceAccount shop/reports
        explained: decided in translate-decisions.yaml: identity/shop/reports is keep-source (as chosen)
    external-secrets.io/ClusterSecretStore aws-secrets-manager
        explained: decided in translate-decisions.yaml: secret-store/aws-secrets-manager is keep-source (as chosen)
Network policies: 12 NetworkPolicies enforced here by Cilium.
```

Then `diff` compares the two live clusters through the translation, so what was translated on purpose or decided isn't counted, and anything else is.

```sh
setdrift diff eks-prod aks-prod --translation-dir ./prod-export/clusters/prod/translations/te2a
```

```
208 in both: 204 identical, 4 as translated, 0 differ
Only in eks-prod: 0   Only in aks-prod: 0
Translated on purpose (5), not counted:
  StorageClass gp3  (parameters.DiskIOPSReadWrite, parameters.DiskMBpsReadWrite, parameters.skuName, provisioner, ...)
  shop/reports Deployment  (spec.template.spec.containers[reports].env, ...)
  shop/web-lb Service  (metadata.annotations["service.beta.kubernetes.io/azure-load-balancer-internal"], ...)
  ...
Decided in translate-decisions.yaml (2), not counted:
  ClusterSecretStore aws-secrets-manager
  shop/reports ServiceAccount
```

`0 differ` and nothing in only one cluster is the proof the AKS cluster is the EKS one, translated. Don't take SetDrift's word alone. Compare `kubectl get` counts by kind on both, and check that a connection the NetworkPolicies block is blocked on both.

## 7. Cut over

SetDrift doesn't move traffic. A checklist for the cutover.

- **Keep EKS running -** Until the AKS cluster has taken traffic for a while, and `setdrift diff eks-prod aks-prod --translation-dir ... --exit-code` stays clean in CI.
- **Point DNS -** At the AKS load balancer's address once it's assigned. Clients of the internal one need it too.
- **Then retire EKS -** Before deleting it, set the retain setting on any cloud-resource object kept with EKS, like the ACK `Bucket`, or deleting the cluster deletes the bucket. RESTORE.md names the setting.

## What else can come up

| On EKS | On AKS |
|---|---|
| Karpenter or Auto Mode node pools, pinned by label | The pool name is kept and the label key rewritten, the node pools are created with the same names |
| Spot nodes | AKS's spot label and the toleration its spot taint needs |
| An ALB Ingress | An Ingress on AKS's controller, internal kept internal. Certificates, health checks and WAF settings are listed to set again |
| EKS Pod Identity | Its associations live in AWS, not in the cluster, so translate doesn't see them yet. An export with `--aws` lists them in its RESTORE.md, set up the AKS side by hand |
| Cilium or Calico policies (`CiliumNetworkPolicy`, `GlobalNetworkPolicy`), AWS's `ClusterNetworkPolicy` and `ApplicationNetworkPolicy` | Exported as they are, but only Kubernetes NetworkPolicies are checked for enforcement today. Check the AKS cluster runs an engine that enforces the others |

## What next

- [setdrift translate](../commands/translate.md), every rewrite and decision.
- [setdrift diff](../commands/diff.md#comparing-across-a-translation), comparing across a translation.
- [Understanding your export](../understanding-your-export.md), what each part of an export is for.
