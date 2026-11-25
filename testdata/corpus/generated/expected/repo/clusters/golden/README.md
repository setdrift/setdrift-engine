# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 13 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| (cluster-scoped) | APIService v1beta1.metrics.k8s.io; ClusterRole shop-reader; FlowSchema workload-low; Namespace shop; PersistentVolume shared-efs |
| kube-system | ConfigMap aws-auth |
| shop | Endpoints legacy-db; PersistentVolumeClaim data-db-x, logs-db-0; Service legacy-db, web; ServiceAccount default; StatefulSet db |

**Bound to one region or account: 2 values.** They work here and break in
another region, account or cloud. RESTORE.md lists where each one is:

- `arn:aws:iam::111122223333:role/nodes`: AWS ARN (iam), bound to account 111122223333
- `arn:aws:iam::111122223333:role/shop`: AWS ARN (iam), bound to account 111122223333

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .

## Cluster profile

cluster_profile/README.md describes the cluster as infrastructure: its cloud, networks
(pod, service and node ranges, VPC, subnets), DNS, storage and system
components, each with where it was found. Start there to recreate the
cluster on another platform or cloud.
