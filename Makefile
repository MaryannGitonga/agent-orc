# agent-orc — developer tasks.
# `make ci` runs exactly what GitHub Actions runs; keep the two in sync.

GO             ?= go
BIN_DIR        ?= bin
DIST_DIR       ?= dist
BINARY         := $(BIN_DIR)/agent-orc
PKG            := ./...
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS        := -X github.com/MaryannGitonga/agent-orc/internal/version.Version=$(VERSION)
GOLANGCI_LINT  ?= golangci-lint
GOLANGCI_VERSION ?= v1.64.8
RANGE          ?= origin/main..HEAD

.DEFAULT_GOAL := help

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## fmt: rewrite sources with gofmt
fmt:
	$(GO) fmt $(PKG)

## fmt-check: fail if any source is not gofmt-clean
fmt-check:
	@unformatted=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; \
		echo "run: make fmt"; exit 1; \
	fi

## vet: run go vet over all build tags
vet:
	$(GO) vet $(PKG)
	$(GO) vet -tags=integration $(PKG)

## lint: run golangci-lint
lint:
	$(GOLANGCI_LINT) run --timeout=5m

## lint-install: install the pinned golangci-lint into GOPATH/bin
lint-install:
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_VERSION)

## build: compile the binary into bin/
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/agent-orc

## test: run unit tests with the race detector
test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKG)

## test-integration: run tests that shell out to real git/gh
test-integration:
	$(GO) test -race -tags=integration ./test/...

## tidy: sync go.mod/go.sum with the source
tidy:
	$(GO) mod tidy

## verify-clean: fail if fmt/tidy would change tracked files
verify-clean: fmt tidy
	@if ! git diff --quiet --exit-code; then \
		echo "working tree is dirty after 'make fmt tidy':"; \
		git --no-pager diff --stat; \
		echo "commit the result of 'make fmt tidy'"; exit 1; \
	fi

## clean: remove build and test artifacts
clean:
	$(GO) clean -testcache
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

## commit-check: enforce the commit policy over RANGE (default origin/main..HEAD)
commit-check:
	@./scripts/check-commits.sh $(RANGE)

## ci: everything CI runs — do this before pushing
ci: fmt-check vet lint build test test-integration commit-check

.PHONY: help fmt fmt-check vet lint lint-install build test test-integration tidy verify-clean clean commit-check ci
