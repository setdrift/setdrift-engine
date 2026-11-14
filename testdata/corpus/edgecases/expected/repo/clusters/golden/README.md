# golden

Exported by SetDrift. kustomization.yaml includes everything that can be
applied: CRDs first, then cluster-scoped resources, then each namespace.
redacted/ and external-secrets/ are not included; see their READMEs.

## What SetDrift found

**Applied by hand: 7 resources.** No GitOps tool, Helm or operator manages these,
so this export is the only record of them. Adopt them into Git (export with
`--unmanaged-only` to write just these):

| Namespace | Resources |
|---|---|
| (cluster-scoped) | ClusterRole Admin, admin, ops:viewer, ops_viewer; CustomResourceDefinition widgets.example.com |
| shop | ConfigMap settings; Widget blue |

## Applying to a new cluster

RESTORE.md is the full checklist: secrets to fill in, volumes to copy,
controllers to install first, and what lives outside Kubernetes. In short:

Argo CD and Flux apply CustomResourceDefinitions before the resources that use
them, so point them at this directory. With kubectl, apply the CRDs first:
kubectl rejects a custom resource whose definition does not exist yet, even
when the definition is in the same apply.

    kubectl apply --server-side -k crds/
    kubectl wait --for condition=established --timeout=60s -k crds/
    kubectl apply --server-side -k .
