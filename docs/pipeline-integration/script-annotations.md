# Script Annotations

## What the script does

[`setdrift-annotate`](https://github.com/setdrift/setdrift-engine/tree/main/scripts/ci/setdrift-annotate) adds SetDrift's annotations to your manifests on their way to `kubectl apply`. It fills them from the variables GitHub sets on every run, so it records the repository, the path, and each deploy's branch, commit and workflow run.

| Annotation | What it holds |
|---|---|
| `setdrift.io/source-repo` | The repository |
| `setdrift.io/source-path` | The directory deployed, if you give one |
| `setdrift.io/source-branch` or `setdrift.io/source-tag` | The branch, or the tag on a tag push |
| `setdrift.io/source-revision` | The commit |
| `setdrift.io/deployed-by` | The workflow run |

It only changes the manifests passed to it, never the cluster, and never pod templates, so pods don't restart.

## Add it to your pipeline

**1. Add the action to the deploy job**, after checkout. There's nothing to install. GitHub downloads the action from SetDrift's public repository, at the release you name, when the job runs, and the action puts `setdrift-annotate` on the job's `PATH`.

```yaml
- uses: actions/checkout@v5
- uses: setdrift/setdrift-engine/scripts/ci/setdrift-annotate@v0.2.0
```

If your organization restricts which actions can run, an admin allows `setdrift/setdrift-engine/*` in the organization's **Settings ▸ Actions ▸ General**. If it only allows its own actions, copy [the folder](https://github.com/setdrift/setdrift-engine/tree/main/scripts/ci/setdrift-annotate) into one of your repositories and use it from there.

**2. Set `shell: bash`** at the top of the workflow, so a failure anywhere in a pipe fails the step.

```yaml
defaults:
  run:
    shell: bash
```

**3. Pipe each deploy step through it**, between rendering the manifests and `kubectl apply`. Give it the path you deploy from.

| How you deploy | Before | After |
|---|---|---|
| Kustomize | `kubectl apply -k deploy/overlays/prod` | `kubectl kustomize deploy/overlays/prod \| setdrift-annotate deploy/overlays/prod \| kubectl apply -f -` |
| Plain manifests | `kubectl apply -f deploy/prod` | `kubectl create -f deploy/prod --dry-run=client -o yaml \| setdrift-annotate deploy/prod \| kubectl apply -f -` |
| `helm template` | `helm template web ./charts/web \| kubectl apply -f -` | `helm template web ./charts/web --skip-tests \| setdrift-annotate charts/web \| kubectl apply -f -` |
| kubectl commands in a script | `kubectl create namespace shop` | `kubectl create namespace shop --dry-run=client -o yaml \| setdrift-annotate \| kubectl apply -f -` |

A deploy job with all three changes.

```yaml
defaults:
  run:
    shell: bash

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: setdrift/setdrift-engine/scripts/ci/setdrift-annotate@v0.2.0
      # your cluster login goes here
      - name: Deploy
        run: kubectl kustomize deploy/overlays/prod | setdrift-annotate deploy/overlays/prod | kubectl apply -f -
```

## Check that it worked

After the next deploy, the resources carry the annotations.

```sh
kubectl get deployment web -n shop -o jsonpath='{.metadata.annotations}'
```

Export the cluster, and the summary counts them under `ci/cd` instead of `unmanaged`.
