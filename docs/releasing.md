# Releasing

Push a version tag: `git tag v0.1.0 && git push origin v0.1.0`. The release
workflow ([.github/workflows/release.yaml](../.github/workflows/release.yaml))
tests, then runs GoReleaser ([.goreleaser.yaml](../.goreleaser.yaml)): archives
for macOS, Linux and Windows with `checksums.txt` on the GitHub release, the
formula in [setdrift/homebrew-tap](https://github.com/setdrift/homebrew-tap),
and build provenance attestations. CI builds the same archives on every
pull request and runs both installers on Linux, macOS and Windows, on
Intel/AMD and ARM. Run `make e2e-restore` before a release.
