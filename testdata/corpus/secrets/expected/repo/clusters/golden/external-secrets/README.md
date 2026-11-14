# External secrets

One ExternalSecret for each Secret under ../redacted/. External Secrets
Operator creates each Secret from your secret manager, so the values never go
into Git. SetDrift did not read or copy any value.

These are NOT applied: ../kustomization.yaml does not list this directory.
External Secrets Operator overwrites a Secret's data with what it reads from
the secret manager, so apply them only once every remote secret below holds
the right values.

1. Install External Secrets Operator, with access to your secret manager.
2. Create the store the ExternalSecrets use, a ClusterSecretStore named `secret-store`.
   One of the examples below, for your secret manager.
3. Create each remote secret below, as JSON with one property per key,
   copying the values from the live Secrets. A binary value (e.g. a
   keystore) can't go into JSON as is: store it base64-encoded and add
   `decodingStrategy: Base64` to its remoteRef in the ExternalSecret.
4. Add `- external-secrets` to ../kustomization.yaml.

| Remote secret | Properties | Replaces Secret |
|---|---|---|
| `golden/argocd/repo-platform` | `password`, `url` | argocd/repo-platform |
| `golden/shop/db-creds` | `password`, `username` | shop/db-creds |
| `golden/shop/ecr-pull` | `.dockerconfigjson` | shop/ecr-pull |

## Stores for common secret managers

The ExternalSecrets read from `secret-store`; create it for the secret manager
you use, and give the operator access to it.

**AWS Secrets Manager.** Remote keys as above.

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: secret-store
spec:
  provider:
    aws:
      service: SecretsManager
      region: <region>
```

**Azure Key Vault.** Secret names allow only letters, digits and dashes: export again
with `--secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'`.

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: secret-store
spec:
  provider:
    azurekv:
      vaultUrl: https://<vault-name>.vault.azure.net
      authType: WorkloadIdentity
      serviceAccountRef:
        name: external-secrets
        namespace: external-secrets
```

**Google Cloud Secret Manager.** Secret names allow letters, digits, dashes and underscores: export
again with `--secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'`.

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: secret-store
spec:
  provider:
    gcpsm:
      projectID: <project-id>
```

**HashiCorp Vault.** Remote keys as above, under the KV mount.

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: secret-store
spec:
  provider:
    vault:
      server: https://vault.example.com
      path: secret
      version: v2
      auth:
        kubernetes:
          mountPath: kubernetes
          role: external-secrets
```

