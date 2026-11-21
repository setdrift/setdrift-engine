# Secrets

Secret values never reach the export, the snapshot or Git.

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
- `setdrift secrets export --aws` fills those remote secrets on AWS, from
  the source cluster: see [Copying secret values](#copying-secret-values).

## Copying secret values

`setdrift secrets export --aws REDACTED_DIR` copies the values of the
Secrets in an export's `redacted/` folder (or a folder in it, such as
`redacted/namespaces/shop`) from the source cluster into AWS Secrets Manager
or Parameter Store, and writes the ExternalSecrets, and their store, that
read them back. It works with any export, however `--external-secrets` was
set: nothing is taken from the export, so every setting is given:

| Flag | What it sets |
|---|---|
| `--aws` | Required: AWS is the only secret manager supported so far |
| `--context` | The source cluster, whose Secrets are read |
| `--region` | The secret manager's AWS region |
| `--secret-manager` | `aws-secrets-manager` or `aws-parameter-store` |
| `--secret-key-template` | Each Secret's remote key: a Go template with `.Cluster`, `.Namespace` and `.Name` (both of the last two required, so no two Secrets share one) |
| `--store-kind`, `--store-name` | The store the ExternalSecrets read from: `ClusterSecretStore`, or `SecretStore` (one is written per namespace) |
| `--external-secrets-out` | Where to write the ExternalSecrets and the store (`--force` replaces a non-empty directory); not needed with `--check` |
| `--profile`, `--kms-key-id` | Optional: the AWS profile, and a KMS key for new secrets (default: the account's AWS managed key) |

Before reading a single value or writing anything, it checks, and prints
each result as ok, warning or failed:

1. **Inputs:** the settings are valid, and every remote key they make is a
   name the secret manager accepts.
2. **Folder:** it is in a SetDrift export, every file is one an export
   writes, and which are Secrets. Other redacted resources hold secret
   values in their manifests: move each value into a Secret and export again.
3. **Source:** the context answers, is the export's source cluster (by its
   kube-system UID; `--other-cluster` overrides), and may get the Secrets
   in each namespace.
4. **AWS identity:** whose credentials are used (STS).
5. **Store:** the secret manager answers in the region, and a `--kms-key-id`
   is enabled.
6. **Permissions:** IAM's policy simulator allows every action on every key
   (and the KMS key). Without access to the simulator
   (`iam:SimulatePrincipalPolicy`), read access to each key is probed and the
   result is a warning: the first refused write stops the run.

Any failed check stops the run with nothing read or written. It then reads
the Secrets' values into memory and prints the plan (create, overwrite, or
skip a remote secret that exists; which keys; binary values, stored base64;
Secrets a controller also writes, which the ExternalSecret merges into).
A Secret or key the source cluster lacks stops it there. After you confirm
(`--yes` in scripts), it writes, reads every remote secret back, and reports
for each whether it exists and holds the Secret's keys and values; then it
writes the ExternalSecrets. `--check` runs every check and that report, and
writes nothing, to confirm AWS holds what a restore needs. `--report FILE`
saves the checks, the plan and the report as JSON. Values are never written
to disk or printed, in any of them.

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

Whichever is set, `setdrift secrets export --aws` can copy the values into
AWS and write ExternalSecrets that match what it wrote: see
[Copying secret values](#copying-secret-values).

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
