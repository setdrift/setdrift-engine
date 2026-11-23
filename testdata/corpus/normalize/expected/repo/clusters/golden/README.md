# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 17 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| (cluster-scoped) | StorageClass gp3 |
| shop | ConfigMap report-x7k2p; CronJob report; DaemonSet log-agent; Deployment api, restarted; Job migrate; NetworkPolicy allow-web, deny-egress; PersistentVolumeClaim data, scratch; Pod toolbox; Service api, db, edge, ingress; StatefulSet db |

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
