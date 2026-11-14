# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 5 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| batch | CronJob nightly; Deployment api; Job backfill, reindex; Pod toolbox |

**Cleanup suggestions** (SetDrift changes nothing in the cluster):

- CronJob batch/nightly: keeps 10 successful and 1 failed Jobs, and their Pods (defaults are 3 and 1)

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .
