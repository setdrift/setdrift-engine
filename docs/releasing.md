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
