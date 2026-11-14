# Redacted resources

These resources are exported for reference only. Secret values in them were
replaced with SETDRIFT_REDACTED before they left the cluster: Secrets, and
other resources holding what looks like a secret (a password in an env var or
a config file; see their SECRET_IN_MANIFEST finding in the snapshot). So they
are not listed in any kustomization.yaml, and use the .yaml.redacted
extension, which kubectl, Argo CD and Flux ignore when reading a directory.

Do not apply them: applying a resource from this directory would overwrite
the real values in the cluster with the placeholder.

- Secrets: ../external-secrets/ has an ExternalSecret for each, which reads
  the values from a secret manager instead; its README says how to set it up.
- Other resources: move the value into a Secret, reference it (env
  valueFrom.secretKeyRef, or a Secret volume), and export again.
