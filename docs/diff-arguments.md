# What `diff A B` reads

Each of `A` and `B` is a kubeconfig context, a
snapshot file (`.pb` or `.json`), or a directory of manifests, in any
combination. `diff` decides which from the argument itself:

| Argument | Read as |
|---|---|
| `context:prod` | Always the kubeconfig context `prod`, read live (`context:arn:aws:eks:...` works too) |
| A path: contains `/` or `\`, or ends in `.pb`, `.json`, `.yaml` or `.yml` (`./prod`, `prod/`, `snap.pb`) | Always that file or directory; an error if it does not exist |
| A bare name (`prod`) that is a file or directory here | That file or directory |
| A bare name that is a kubeconfig context | The context, read live |
| A bare name that is **both** | An error: `prod is both a directory here and a kubeconfig context: write ./prod for the directory, or context:prod for the cluster` |
| A bare name that is neither | An error listing the kubeconfig's contexts |

Exports are often named after their cluster (`setdrift export --context
prod -o prod`), so in that directory `prod` would be ambiguous: rather than
pick one silently, `diff` asks. Both arguments are checked before either is
read, and every problem is reported at once. Contexts come from one
kubeconfig (`--kubeconfig`, `KUBECONFIG` or `~/.kube/config`); for contexts
in two files, merge them: `KUBECONFIG=~/.kube/config:~/.kube/other setdrift diff staging prod`.
