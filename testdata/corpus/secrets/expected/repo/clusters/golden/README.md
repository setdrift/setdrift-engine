# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 8 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| (cluster-scoped) | Namespace shop |
| argocd | Secret repo-platform |
| shop | ConfigMap app-config; Deployment api, worker; Secret ci-token, db-creds, ecr-pull |

**Held back: 2 resources with secret values in the manifest.** They are in
redacted/, not in the kustomization, so applying this export leaves them out.
Move the values into Secrets and reference them, and the next export
includes them:

- ConfigMap shop/app-config: `data.application.properties`
- Deployment shop/api: `spec.template.spec.containers[api].env[DB_PASSWORD].value`

**Secrets: 3 Secrets whose values were left out.** RESTORE.md's secrets step
says how to supply them.

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

Point Argo CD or Flux at this directory, or:

    kubectl apply --server-side -k .
