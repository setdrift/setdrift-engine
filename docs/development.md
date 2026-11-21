# Development

```sh
make build             # bin/setdrift
make test              # unit and golden tests
make golden            # regenerate testdata/corpus/*/expected after an intended change
make generate          # regenerate Go code after editing proto/
make lint              # gofmt, go vet, buf lint
make release-snapshot  # the release archives and Homebrew formula in dist/, unpublished (needs goreleaser)
make e2e-populate      # an EKS test cluster, created on demand (test/e2e/README.md)
```

Golden tests: each `testdata/corpus/<case>/input` directory holds manifests as
read from a cluster, and `expected/` holds the snapshot JSON and emitted
repository. Add a case for every normalization rule or bug fix.

Race tests (`pkg/collect/race_test.go`) also serve every corpus through fake
cluster clients, and delete, recreate or deny access to each object the
collector fetches between its list and its fetch. Each result must match an
export of the cluster without those objects. New corpus cases are covered
automatically.
