# Secrets

Secret values never reach the export, the snapshot, Git or anywhere else: SetDrift never copies them.

- Secrets, and any other resource holding what looks like a secret value (a
  password in an env var or a config file), are written under `redacted/`
  for reference only, with the values replaced by `SETDRIFT_REDACTED`.
  Applying them would overwrite the real values, so no `kustomization.yaml`
  lists them, and the `.yaml.redacted` extension keeps `kubectl apply -R`,
  Argo CD and Flux from picking them up. The export's README names the
  resources other than Secrets held back this way, and where their values
  are: move those values into Secrets, and the next export includes them.
- Secret keys only a controller wrote (Argo CD's generated passwords, a
  webhook's certificate) are left out altogether: the controller writes
  them again in a new cluster.
- For each redacted Secret, `external-secrets/` has an ExternalSecret that
  has External Secrets Operator read the values from a secret manager,
  covering only the keys a person or tool wrote (merged into the Secret a
  controller creates, with `creationPolicy: Merge`). They reference a store
  named `secret-store`; its README has example stores for AWS, Azure Key Vault,
  Google Secret Manager and Vault. Which secret manager, and how remote keys
  are named: see [Secret managers](#secret-managers).
- SetDrift never copies the values: you put them in your secret manager
  yourself. RESTORE.md lists each remote secret and its keys, and on AWS
  gives the commands that create them, with each value left for you to fill in.

## Secret managers

The ExternalSecrets name a store (`secret-store`, or `--secret-store`), not
a provider, so they work with any secret manager External Secrets Operator
supports. `--external-secrets` sets the rest:

| Value | Secret manager | Remote keys |
|---|---|---|
| `auto` (default) | AWS Secrets Manager on a cluster on AWS or EKS, else `generic` | as below |
| `generic` | Any: the store README has examples for Azure Key Vault, Google Secret Manager, HashiCorp Vault and AWS | `<cluster>/<namespace>/<name>` |
| `aws-secrets-manager` | AWS Secrets Manager | `<cluster>/<namespace>/<name>` |
| `aws-parameter-store` | AWS Systems Manager Parameter Store | `/<cluster>/<namespace>/<name>` (parameter names are paths) |
| `none` | No ExternalSecrets are written | |

`--secret-key-template` names the remote keys: a Go template with
`.Cluster`, `.Namespace` and `.Name`, checked before anything is read.

> **Azure Key Vault and Google Secret Manager do not allow `/` in secret
> names**, so the default keys fail there. Use dashes:
>
> ```sh
> setdrift export --external-secrets generic --secret-key-template '{{.Cluster}}-{{.Namespace}}-{{.Name}}'
> ```
>
> Azure allows letters, digits and dashes only (no `_` or `.` in names);
> Google also allows underscores.
