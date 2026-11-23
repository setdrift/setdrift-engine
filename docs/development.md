# Development

```sh
make build             # bin/setdrift
make test              # unit and golden tests
make golden            # regenerate testdata/corpus/*/expected after an intended change
make generate          # regenerate Go code after editing proto/
make lint              # gofmt, go vet, buf lint
make release-snapshot  # the release archives and Homebrew formula in dist/, unpublished (needs goreleaser)
make e2e-populate      # an EKS test cluster, created on demand (test/e2e/README.md)
make scan              # the secret scan, over the whole history
make hooks             # turn on the secret scan before every commit and push
```

Golden tests: each `testdata/corpus/<case>/input` directory holds manifests as
read from a cluster, and `expected/` holds the snapshot JSON and emitted
repository. Add a case for every normalization rule or bug fix.

Race tests (`pkg/collect/race_test.go`) also serve every corpus through fake
cluster clients, and delete, recreate or deny access to each object the
collector fetches between its list and its fetch. Each result must match an
export of the cluster without those objects. New corpus cases are covered
automatically.

## Secret scan

This repository is public, so credentials, personal information and cloud identifiers must never reach it. The secret scan ([scripts/secret-scan.sh](https://github.com/setdrift/setdrift-engine/blob/main/scripts/secret-scan.sh)) refuses them in three places.

- **Before every commit and push -** The git hooks in `.githooks`, once `make hooks` turns them on. The pre-commit hook scans what is staged, and the pre-push hook scans the commits being pushed. They need gitleaks (`brew install gitleaks`).
- **On every push and pull request -** The `secret-scan` workflow scans the whole history. A pull request with a finding can't merge, and a release doesn't publish.
- **At push time, on GitHub -** Push protection rejects known credential formats, like cloud keys and tokens, before they land.

It runs three checks.

- **gitleaks -** With the rules in [.gitleaks.toml](https://github.com/setdrift/setdrift-engine/blob/main/.gitleaks.toml), gitleaks' own credential rules plus email addresses, AWS account IDs, Azure subscription and tenant IDs, Google Cloud billing accounts and projects, and public IP addresses. The placeholders the docs and tests use, like `111122223333` and `example.com`, are allowed, and so are GitHub no-reply addresses.
- **A denylist of names and addresses -** Kept outside the repository, since listing them here would publish them. CI reads the `PII_DENYLIST` secret, and the hooks read `~/.config/setdrift/pii-denylist`, one entry a line. A match is reported by the entry's number, never its text, since CI logs on a public repository are public.
- **Commit addresses -** Every commit's author and committer address must be a GitHub no-reply address. Set yours with `git config user.email ID+username@users.noreply.github.com`, as GitHub's email settings show it.

When it refuses, remove what it found, from earlier commits too with `git commit --amend` or a rebase, since a commit that's pushed stays in the history. A fake secret a test needs belongs in one of the paths `.gitleaks.toml` allows for that.

