# Many teams, one cluster

In the real world, repositories and clusters aren't one to one. A single cluster usually runs what dozens of repositories deploy, each owned by a different team with its own pipeline. For SetDrift to show where every resource came from, each of those repositories has to mark what it deploys, and you can't change them all at once.

There are three ways to roll that out.

| Approach | Who does the work | Best when |
|---|---|---|
| [Each team commits annotations](#each-team-commits-annotations) | Each team, once, in its own repository | Teams deploy with kustomize and want no pipeline change |
| [Each team adds the shared action](#each-team-adds-the-shared-action) | Each team, one line in its own workflow | Teams own their deploy workflows |
| [The platform team adds it to a shared workflow](#the-platform-team-adds-it-to-a-shared-workflow) | The platform team, once | Teams already deploy through one reusable workflow |

The approaches mix. Some teams can commit annotations while others use the action, and SetDrift reads them the same way.

## Each team commits annotations

Each team adds the annotations to its own manifests, as in [Annotations in Git](annotations-in-git.md). Nothing in any pipeline changes, and each repository names itself.

## Each team adds the shared action

Each team adds one step to its deploy workflow and pipes its deploy through it, as in [Script Annotations](script-annotations.md). Every team uses the same action from SetDrift's repository, pinned to a release, so there's one script to keep up, not one per repository.

```yaml
- uses: setdrift/setdrift-engine/scripts/ci/setdrift-annotate@v0.2.0
```

## The platform team adds it to a shared workflow

Many organizations already have teams deploy through one reusable workflow that the platform team owns. Add the action and the pipe to that workflow once, and every repository that calls it is covered at its next deploy, with no change in the teams' repositories.

```yaml
# in each team's repository, unchanged
jobs:
  deploy:
    uses: acme/platform-workflows/.github/workflows/deploy.yml@v1
```

A reusable workflow runs with the calling repository's details, so the annotations still name each team's own repository, not the platform team's.

## Find the repositories without annotations

Export only the unmanaged resources.

```sh
setdrift export --context prod -o ./unmanaged --unmanaged-only
```

The summary lists them by namespace, which usually maps to a team. Run it again as teams roll out, until the list is empty.
