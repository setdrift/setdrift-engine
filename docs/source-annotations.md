# Source Annotations

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
`https://github.com/foo/bar`, `github.com/foo/bar`, `github/foo/bar`,
`https://github.com/foo/bar.git` and `git@github.com:foo/bar.git` are all
`https://github.com/foo/bar` (also `gitlab/` and `bitbucket/` shorthands),
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

For example, for `github.com/foo/bar`, where feature branches deploy to a
dev cluster and `main` deploys to pre-prod, then to prod after UAT:

| Cluster | `source-path` | `source-branch` | `source-revision` |
|---|---|---|---|
| dev | `deploy/overlays/dev` | `feature/login` | that branch's commit |
| pre-prod | `deploy/overlays/preprod` | `main` | the `main` commit |
| prod | `deploy/overlays/prod` | `main` | the same commit as pre-prod |

## What SetDrift does with them

It shows such a resource as
`CI/CD <repo>//<path>`, leaves it out of `--unmanaged-only` and of "Applied
by hand", and keeps the annotations in the snapshot as evidence. The
exported manifests drop them: they change with every deploy and between
clusters. Argo CD, Flux and Helm markers still win where present (the
annotations are then kept as extra evidence), except Helm's label alone,
which `helm template | kubectl apply` leaves behind.

## Adding them

With kustomize, in the pipeline step that deploys, before
`kubectl apply -k`:

```yaml
- name: Deploy
  env:
    OVERLAY: deploy/overlays/${{ inputs.environment }}   # dev, preprod or prod
  run: |
    cd "$OVERLAY"
    kustomize edit add annotation \
      "setdrift.io/source-repo:$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
      "setdrift.io/source-path:$OVERLAY" \
      "setdrift.io/source-branch:${GITHUB_HEAD_REF:-$GITHUB_REF_NAME}" \
      "setdrift.io/source-revision:$GITHUB_SHA" \
      "setdrift.io/deployed-by:$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
    kubectl apply -k .
```

With plain manifests:

```sh
kubectl annotate --local -f deploy/prod -o yaml \
  "setdrift.io/source-repo=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
  "setdrift.io/source-branch=$GITHUB_REF_NAME" \
  "setdrift.io/source-revision=$GITHUB_SHA" | kubectl apply -f -
```

For a release from a tag, replace `source-branch` with
`"setdrift.io/source-tag:$GITHUB_REF_NAME"` in the step that runs on tag
pushes.

## Without annotations

A pipeline can name its kubectl field manager:
`kubectl apply --field-manager=github-actions` in the pipeline, and
`setdrift export --ci-field-manager github-actions` (also on `diff`). The
resources it wrote then count as CI/CD, with low confidence and no
repository. Helm releases need neither: Helm's own markers identify them.
