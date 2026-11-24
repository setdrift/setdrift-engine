# How SetDrift decides who manages a resource

For every resource it reads, SetDrift works out what deploys and manages it, so a change to the resource can go back to the right place. It only trusts markers a tool writes itself, never a guess. A resource with no marker is unmanaged, shown as applied by hand.

## The markers

SetDrift checks for these in order, and the first one found decides.

| Order | Manager | The marker | Shown as |
|---|---|---|---|
| 1 | The platform | Evidence its platform created the resource, like an EKS add-on or a kind or kubeadm component | `platform` (kept in the snapshot, not written to the export) |
| 2 | Flux | The label `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name` | `Flux <namespace>/<name>` |
| 3 | Argo CD | The annotation `argocd.argoproj.io/tracking-id`, when it names this resource | `Argo CD <app>` |
| 4 | Helm | The annotation `meta.helm.sh/release-name`, which `helm install` and `helm upgrade` write | `Helm <namespace>/<release>` |
| 5 | Argo CD | A custom tracking label, if you name one with [`--argocd-instance-label`](commands/export.md#--argocd-instance-label) | `Argo CD <app>` |
| 6 | An operator | An owner reference to a custom resource | `operator <Kind>/<name>` |
| 7 | A pipeline | The annotation `setdrift.io/source-repo`, see [Source annotations](pipeline-integration/source-annotations.md) | `CI/CD <repo>//<path>` |
| — | Nothing | None of the above | applied by hand |

A pipeline's annotations only decide when nothing above them does. On a resource Argo CD, Flux, Helm or an operator manages, SetDrift keeps them in the snapshot as extra evidence.

## What doesn't count

| Looks like a marker | Why SetDrift ignores it |
|---|---|
| `app.kubernetes.io/managed-by: Helm` | Chart templates set it on everything they render, whether or not Helm installed it |
| `app.kubernetes.io/instance` | Charts set it to the release name, and it's also the label Argo CD tracked with before 3.0, so it can't say which |
| An Argo CD tracking id that names a different resource | It was copied from that resource, so Argo CD doesn't manage this one. The export reports it. |
| kubectl's field manager | It says kubectl wrote the resource, the same for a person and a pipeline |

## When the manager is gone

SetDrift also checks that the manager a marker names still exists, the Argo CD Application, the Flux Kustomization or HelmRelease, or the Helm release. If it's gone, the resource is reported (`MANAGER_NOT_FOUND`) and counted as unmanaged, since nothing manages it now. When SetDrift can't see those objects at all, like an Argo CD in another cluster, it skips the check.

## Where it shows

| Where | What you see |
|---|---|
| The export's summary | A count for each manager, like `helm 3`, `ci/cd 6`, `unmanaged 4` |
| The export's README and summary | The "Applied by hand" list, by namespace |
| `export --unmanaged-only` | Only the applied-by-hand resources are written |
| `diff` | Who manages each resource on each side, like `shop/web Deployment.apps (applied by hand)` |
| The snapshot | Each resource's manager and the markers it was decided from |
