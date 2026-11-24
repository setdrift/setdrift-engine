# Source annotations

> **To revisit:** the annotation set (repo, path, branch, tag, revision,
> deployed-by) and their examples were added on 2026-11-20 and are not yet
> used by the SaaS; review them before launch.

A pipeline that runs `kubectl apply` leaves no GitOps markers, so the
resources it deploys look applied by hand: false positives in
`--unmanaged-only` and in the summary's "Applied by hand" list, though they
live in Git. Have each pipeline stamp where they come from:

| Annotation | What it holds | Required |
|---|---|---|
| `setdrift.io/source-repo` | The repository | Yes |
| `setdrift.io/source-path` | The directory in it that is applied, e.g. `deploy/overlays/prod` | No |
| `setdrift.io/source-branch` | The branch deployed | No |
| `setdrift.io/source-tag` | The tag deployed, when a release deploys a tag | No |
| `setdrift.io/source-revision` | The commit deployed, in full | No |
| `setdrift.io/deployed-by` | The pipeline run | No |

The value to set each one to, in GitHub Actions and in GitLab CI:

| Annotation | GitHub Actions | GitLab CI |
|---|---|---|
| `source-repo` | `$GITHUB_SERVER_URL/$GITHUB_REPOSITORY` | `$CI_PROJECT_URL` |
| `source-path` | your path | your path |
| `source-branch` | `$GITHUB_REF_NAME`, or `$GITHUB_HEAD_REF` on a pull request | `$CI_COMMIT_BRANCH` |
| `source-tag` | `$GITHUB_REF_NAME` on a tag push | `$CI_COMMIT_TAG` |
| `source-revision` | `$GITHUB_SHA` | `$CI_COMMIT_SHA` |
| `deployed-by` | `$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID` | `$CI_JOB_URL` |

`source-repo` alone ends the false positives. It is read in any common form:
`https://github.com/acme/shop`, `github.com/acme/shop`, `github/acme/shop`,
`https://github.com/acme/shop.git` and `git@github.com:acme/shop.git` are all
`https://github.com/acme/shop` (also `gitlab/` and `bitbucket/` shorthands),
and credentials in the URL are dropped. The others say what to compare and
where a fix goes:

- **`source-branch`** answers where a change belongs: on a cluster that
  feature branches deploy to, which branch each resource came from, even
  after the branch's commits are squashed or force-pushed away.
- **`source-tag`** is for releases deployed from a tag (`v1.4.2`); set it
  instead of, or with, the branch.
- **`source-revision`** pins the exact commit. A cluster promoted from
  another (pre-prod to prod) carries the same commit, so `setdrift diff
  preprod prod` can tell a cluster that is behind from one edited by hand.

For example, for `github.com/acme/shop`, where feature branches deploy to a
dev cluster and `main` deploys to pre-prod, then to prod after UAT:

| Cluster | `source-path` | `source-branch` | `source-revision` |
|---|---|---|---|
| dev | `deploy/overlays/dev` | `feature/login` | that branch's commit |
| pre-prod | `deploy/overlays/preprod` | `main` | the `main` commit |
| prod | `deploy/overlays/prod` | `main` | the same commit as pre-prod |

## What SetDrift does with them

It shows such a resource as `CI/CD <repo>//<path>`, leaves it out of `--unmanaged-only` and of "Applied by hand", and keeps the annotations in the snapshot as evidence. The exported manifests drop them, since they change with every deploy and between clusters.

The markers a tool writes itself still win where present, Argo CD's tracking id, Flux's labels, Helm's `meta.helm.sh/release-name` annotation, and the annotations are then kept as extra evidence. The labels a chart's templates set count for nothing, `app.kubernetes.io/managed-by: Helm` and `app.kubernetes.io/instance`. They're on every object a chart renders, whoever applies it, so after `helm template | kubectl apply` the resources are applied by hand until the annotations say otherwise.

## Adding them

Pipe the rendered manifests through `kubectl annotate --local` before `kubectl apply`, so the annotations go on each object's own metadata. [Pipeline resources showing as unmanaged](unmanaged-pipeline-resources.md) has the ways to add them, committed in Git or in a GitHub Actions workflow.

```sh
kubectl kustomize deploy/overlays/prod | kubectl annotate --local --overwrite -f - -o yaml \
  "setdrift.io/source-repo=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
  "setdrift.io/source-path=deploy/overlays/prod" \
  "setdrift.io/source-revision=$GITHUB_SHA" | kubectl apply -f -
```

Don't add them with kustomize's `commonAnnotations` or `kustomize edit add annotation`. Kustomize copies those into every pod template, so `source-revision` and `deployed-by`, which change on every deploy, would restart every pod each time.
