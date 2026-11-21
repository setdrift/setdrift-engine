# Permissions

> **To revisit before the next push to GitHub and before launch:** this page
> describes the access rules added on 2026-11-20; check it against the final
> behaviour and the e2e results.

`export`, `diff` and `verify` follow the same rules, and every refusal names
who the cluster says you are (`system:serviceaccount:team:ci`, an IAM role
on EKS) and how to fix it. `-n` limits all three to some namespaces.

| Your access | Without `-n` | With `-n X` |
|---|---|---|
| The whole cluster | Everything is read | `X` is read |
| Some namespaces, and you can list namespaces | The namespaces you can read are read, and the rest are named as not readable | `X` is read if you can read it |
| Some namespaces, and you can't list namespaces | The kubeconfig context's namespace is read; with none set, you're asked for `-n` | `X` is read if you can read it |
| None | An error naming what you need | An error naming what you need |

- **`-n X` must exist and be readable,** checked before anything is read, so
  nothing is written on a typo. A namespace that does not exist gets the
  cluster's namespaces listed; one you can't read gets the RoleBinding that
  would grant it.
- **A type refused in one namespace** (a role that allows Pods but not
  ConfigMaps) is skipped there only, and listed in the summary.
- **`verify`** counts a resource you may not get, or may not patch for the
  dry-run, as not checked: out of the percentage, and the run fails for it,
  saying which permission is missing. It never reports a refusal as a
  difference. With `-n`, a namespace that does not exist in the target is
  not an error: its resources show as not in the cluster (before a restore).
- **`diff`** compares only what both sides read: a side that read some
  namespaces, or could not list a type, limits the other to the same, so
  nothing shows as in one side only for not having been read.
- **The kubeconfig itself:** a missing, empty, unreadable or malformed file,
  a directory instead of a file, a context that does not exist, an expired
  token, a missing credential plugin or an unreachable cluster each get
  their own message, within 15 seconds, before anything is written.
