# Security policy

SetDrift reads Kubernetes clusters, including their Secrets, and writes
manifests meant to be committed to Git. A bug that lets a secret value reach
that output is a vulnerability, so we treat reports as a priority.

## Reporting a vulnerability

Please report vulnerabilities privately, not in a public issue or pull
request: use **Report a vulnerability** on this repository's
[Security tab](https://github.com/setdrift/setdrift-engine/security/advisories/new)
(GitHub private vulnerability reporting).

Include what you ran (command and flags), the SetDrift version
(`setdrift version`), and the smallest input that shows the problem. If the
problem involves a secret value, use a made-up value, never a real one.

We make a best effort to acknowledge a report within five business days.
We then agree on a fix and a disclosure date with you, and credit you in the
advisory unless you would rather not be named.

## What counts

In scope:

- A secret value in the export, snapshot or restore guide: a Secret's data, a
  value redaction should have removed from another kind, or the
  `last-applied-configuration` annotation.
- Any change to a cluster. The CLI only reads clusters: `setdrift verify`
  applies with server-side dry-run, and export's one create call is a
  SelfSubjectRulesReview, a question about the caller's permissions that the
  API server doesn't store.
- `setdrift secrets export`, the one command that reads secret values:
  a value written to disk, printed or logged, or sent anywhere but the
  secret manager it names; a write it did not show in its plan, or made
  without confirmation (or `--yes`).
- Credentials (kubeconfig, AWS) logged, written to output, or sent anywhere.
- Code execution or path traversal through cluster data or `--from-dir`
  input, e.g. an object name that makes the exporter write outside its
  output directory.
- The Helm post-renderer altering manifests beyond the annotation it adds.

Not in scope: values a user commits by hand after exporting; secrets stored
outside Kubernetes Secrets in a form SetDrift can't recognize (report these as
ordinary issues, so detection can improve); and vulnerabilities in
dependencies with no effect on SetDrift (report those upstream).

## Supported versions

Before 1.0, only the latest release gets security fixes.
