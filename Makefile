BIN := $(CURDIR)/bin
## override set version e.g. make build VERSION=v0.1.0
## empty before the first tag: the binary then uses the version Go records
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
## flags passed to go linker
LDFLAGS := -s -w \
	-X setdrift.io/setdrift-engine/internal/version.Version=$(VERSION) \
	-X setdrift.io/setdrift-engine/internal/version.Commit=$(COMMIT)

BUF_VERSION           := v1.75.0
PROTOC_GEN_GO_VERSION := v1.36.12
GOVULNCHECK_VERSION   := v1.10.0
# release binaries may not grow past this (MiB): see `make size`
MAX_SIZE_MIB          := 28

## indicates target are command names not files
.PHONY: build plugin helm-plugin test golden scan hooks generate notices lint vulncheck size tools clean release-snapshot e2e-setup e2e-up e2e-ready e2e-populate e2e-restore e2e-status e2e-down

build: ## Build bin/setdrift
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/setdrift ./cmd/setdrift

plugin: build ## Build bin/kubectl-setdrift (runs as `kubectl setdrift`)
	cp $(BIN)/setdrift $(BIN)/kubectl-setdrift

helm-plugin: ## Package the Helm post-renderer plugin as bin/setdrift-chart-repo.tgz (helm plugin install <file or URL>)
	mkdir -p $(BIN)
	COPYFILE_DISABLE=1 tar -C contrib/helm -czf $(BIN)/setdrift-chart-repo.tgz setdrift-chart-repo

test: ## Run all tests
	go test ./...

scan: ## Scan the history and working tree for credentials, personal information and cloud identifiers (scripts/secret-scan.sh)
	scripts/secret-scan.sh all

hooks: ## Turn on the git hooks in .githooks, the secret scan before every commit and push
	git config core.hooksPath .githooks

golden: ## Regenerate golden files after an intended output change; review the diff
	go test ./pkg/pipeline -run TestGolden -update

generate: tools ## Regenerate Go code from proto/
	PATH="$(BIN):$$PATH" $(BIN)/buf generate

notices: ## Regenerate THIRD_PARTY_NOTICES after a dependency change
	scripts/third-party-notices.sh

lint: tools ## Lint Go and proto, and check license footers
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	$(BIN)/buf lint
	bash scripts/check-license-footers.sh

vulncheck: $(BIN)/govulncheck ## Scan the built CLI for known vulnerabilities in Go and its dependencies
	go build -o $(BIN)/setdrift-vulncheck ./cmd/setdrift
	@# binary mode: govulncheck's source mode can't yet load Go 1.27's standard library
	$(BIN)/govulncheck -mode=binary $(BIN)/setdrift-vulncheck

size: ## Check every release binary in dist/ is under MAX_SIZE_MIB (run make release-snapshot first)
	@# A call that makes Go's linker keep every exported method (such as running a
	@# text/template) about doubles the binary; this catches it in the change that adds it.
	@# It measures the binaries goreleaser built, so CI builds each platform once.
	@set -e; n=0; for f in dist/setdrift_*/setdrift dist/setdrift_*/setdrift.exe; do \
		[ -f "$$f" ] || continue; n=$$((n + 1)); \
		size=$$(wc -c < "$$f"); mib=$$((size / 1048576)); \
		echo "$${f#dist/}: $$mib MiB"; \
		if [ $$size -gt $$(( $(MAX_SIZE_MIB) * 1048576 )) ]; then \
			echo "$${f#dist/} is $$mib MiB, over the $(MAX_SIZE_MIB) MiB budget: see what grew with go tool nm -size -sort size" >&2; exit 1; \
		fi; \
	done; \
	if [ $$n -ne 6 ]; then echo "found $$n release binaries in dist/, expected 6: run make release-snapshot first" >&2; exit 1; fi

tools: $(BIN)/buf $(BIN)/protoc-gen-go

$(BIN)/govulncheck:
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

$(BIN)/buf:
	GOBIN=$(BIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(BIN)/protoc-gen-go:
	GOBIN=$(BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

e2e-setup: ## Deploy the e2e reaper (once per account and region)
	test/e2e/cluster.sh setup

e2e-up: ## Start creating the e2e cluster in the background
	test/e2e/cluster.sh up

e2e-ready: ## Create the e2e cluster if needed and wait until it is ready
	test/e2e/cluster.sh ensure

e2e-populate: e2e-ready ## Install Argo CD, Flux, ESO and the fixtures
	test/e2e/populate.sh

e2e-restore: ## Follow RESTORE.md into a second e2e cluster and check the result (~30 min)
	test/e2e/restore.sh

e2e-status: ## Show the e2e cluster's state and time until teardown
	test/e2e/cluster.sh status

e2e-down: ## Tear the e2e cluster down now
	test/e2e/cluster.sh down

release-snapshot: ## Build the release archives and Homebrew formula into dist/ without publishing (needs goreleaser)
	goreleaser release --snapshot --clean

clean:
	rm -rf $(BIN)

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
