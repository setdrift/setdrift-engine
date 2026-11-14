# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 4 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| (cluster-scoped) | ClusterRole system:aggregate-to-shop; Namespace shop |
| shop | Pod migrate-x7k2p; Secret db-creds |

**Secrets: 1 Secret whose values were left out.** RESTORE.md's secrets step
says how to supply them.

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .
