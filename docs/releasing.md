# Releasing

Push a version tag: `git tag v0.1.0 && git push origin v0.1.0`. The release
workflow ([.github/workflows/release.yaml](../.github/workflows/release.yaml))
first runs the whole of CI and govulncheck on the tagged commit, and publishes
only when every job of both passes. It then runs GoReleaser
([.goreleaser.yaml](../.goreleaser.yaml)): archives for macOS, Linux and
Windows with `checksums.txt` on the GitHub release, the formula in
[setdrift/homebrew-tap](https://github.com/setdrift/homebrew-tap), and build
provenance attestations. CI builds the same archives on every pull request and
runs both installers on Linux, macOS and Windows, on Intel/AMD and ARM. Run
`make e2e-restore` before a release.

## Versions

SetDrift follows [semantic versioning](https://semver.org): `vMAJOR.MINOR.PATCH`.
What it promises not to break is its interface: the command-line flags, the
layout of an export (its directories, files and RESTORE.md's steps), and the
snapshot format (`proto/`, today `setdrift.snapshot.v1alpha1`).

| Release | When | Example |
|---|---|---|
| Patch: v0.1.0 to v0.1.1 | Fixes only. No new flag, no change to the export layout or the snapshot format. An export or snapshot from v0.1.0 reads and compares the same. | A wrong default removed, a crash fixed, a clearer error |
| Minor: v0.1.x to v0.2.0 | New features, or anything that changes a flag, the export layout or the snapshot format. Before v1.0.0, a minor release may break: its notes say what changed and what to do. | A new command or flag, a new file in the export, a field added to the snapshot |
| Major: v0.x to v1.0.0 | The interface is declared stable: from here, flags, the export layout and the snapshot format only change compatibly within a major version. | The snapshot schema leaves `v1alpha1` for `v1`, and `buf breaking` runs in CI to hold it there |
| Major after v1: v1.x to v2.0.0 | A breaking change to the stable interface. | A flag removed or renamed, an export layout another tool can no longer read |

A security fix is released as a patch on the newest release line, even when
it changes behavior to close the hole; its notes say so. Before v1.0.0, only
the newest release gets fixes (see [SECURITY.md](../.github/SECURITY.md)).

A tag with a pre-release suffix (`v0.2.0-rc.1`) is published as a pre-release
(GoReleaser's `prerelease: auto`) and does not update the Homebrew formula's
stable version.

## Docs fixes between releases

setdrift.io shows each release line's docs as released, so a docs fix on main
waits for the next release. To publish one sooner, put it on the line's
release branch too, `release-v0.2` for v0.2:

1. Merge the fix to main as usual.
2. If the branch doesn't exist yet, start it at the newest release:
   `git push origin v0.2.0^{commit}:refs/heads/release-v0.2`. v0.2.0 is older
   than the workflows below, so v0.2's branch also takes main's
   `.github/workflows/release-docs.yaml` and `docs.yaml`; later releases hold
   them already.
3. Cherry-pick the fix onto a branch from `release-v0.2`, named outside the
   `release-v*` pattern (`upgrade-docs-v0.2`), and open a pull request into
   `release-v0.2`.

The `release-docs` check refuses anything outside `docs/`, `README.md` and
`.github/workflows/` since the release, so the branch never holds code. Once
merged, setdrift.io rebuilds with the fix. Only fixes that are true of the released version go
there: a page for a feature on main waits for its release.

The next release on the line, like v0.2.1, is tagged from main as always, and
holds the fix already. setdrift.io uses a release branch only while it holds
the line's newest tag, so after v0.2.1 it shows v0.2.1's docs until the branch
is moved up: `git push --force origin v0.2.1^{commit}:refs/heads/release-v0.2`.
