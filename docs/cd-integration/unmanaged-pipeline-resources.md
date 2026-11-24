# Pipeline resources showing as unmanaged

SetDrift may label resources your CI/CD pipeline deploys as unmanaged (or applied by hand), even though they come from Git.

- **Why -** Most pipelines deploy with `kubectl`, which leaves nothing on a resource to say a pipeline applied it, so SetDrift can't tell it from one a person applied by hand.
- **Our solution -** The annotation `setdrift.io/source-repo`, set to the Git repository the resource is deployed from. SetDrift then shows the resource as your pipeline's.
- **How to add it -** [Commit it in Git](annotations-in-git.md) with no pipeline change, or [add the script to your pipeline](script-annotations.md) to add it at every deploy.

## Common causes

SetDrift lists a resource as unmanaged when nothing on it says what deployed it. Resources a pipeline deploys usually end up there for one of these reasons.

| What the pipeline does | Why it shows as applied by hand |
|---|---|
| `kubectl apply -f` or `kubectl apply -k` | kubectl and kustomize leave nothing on the resource that says a pipeline applied it |
| `helm template` piped to `kubectl apply` | No Helm release is installed, so Helm's own marker is missing |
| kubectl commands in a script | They leave nothing on the resource either |

`helm install` and `helm upgrade` aren't on the list. Helm marks every resource it installs, and SetDrift shows them as `Helm <namespace>/<release>`.

If you've fixed your pipeline and a resource still shows as applied by hand, the pipeline hasn't deployed it since the fix. Run the pipeline once, then export again. If it's still listed, no pipeline deploys it anymore, so it really is applied by hand. Delete it, or add it back to your manifests.

## Why this happens

Argo CD, Flux, Helm and operators each leave a marker on the resources they manage, and SetDrift reads them.

| Tool | The marker it leaves on each resource | SetDrift shows |
|---|---|---|
| Argo CD | The annotation `argocd.argoproj.io/tracking-id`, naming its Application | `Argo CD <app>` |
| Flux | The label `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name`, naming its Kustomization or HelmRelease | `Flux <namespace>/<name>` |
| Helm | The annotation `meta.helm.sh/release-name`, naming its release | `Helm <namespace>/<release>` |
| An operator | An owner reference to the custom resource it runs | `operator <Kind>/<name>` |
| kubectl and kustomize | Nothing | applied by hand |

`kubectl` records only that kubectl wrote the resource, the same whether a person runs it from a laptop or a pipeline runs it, and kustomize adds nothing of its own. A resource with none of these indicators is indistinguishable from a resource applied manually.

## How to fix it

Give the resource a marker that SetDrift recognizes, the `setdrift.io/source-repo` annotation ([Source annotations](../source-annotations.md) lists it and the optional ones). There are two ways to add it.

| Fix | What you change | SetDrift shows |
|---|---|---|
| [Annotations in Git](annotations-in-git.md) | A file committed next to your manifests, no pipeline change | `CI/CD <repo>//<path>` |
| [Script Annotations](script-annotations.md) | One pipe in the deploy step | `CI/CD <repo>//<path>`, plus the branch, commit and run |

Teams can use different ones, SetDrift reads both. With many teams deploying to one cluster, see [Many teams, one cluster](many-teams.md).
