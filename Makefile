# agent-orc developer tasks.
#
# `make ci` runs the workflow checks that work on a working copy. Two do not:
# verify-clean needs a committed tree, and signature verification needs the
# GitHub API. Keep this file and .github/workflows in sync.

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
# gofmt directly rather than `go fmt ./...`: the latter works package by package
# and so skips files excluded by a build tag, which is every integration test.
# fmt-check looks at those files, so fmt has to as well or it cannot fix what
# the check reports.
fmt:
	@gofmt -w -l $(shell git ls-files '*.go')

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

## print-lint-version: print the pinned golangci-lint version, for CI to install
print-lint-version:
	@echo $(GOLANGCI_VERSION)

## build: compile the binary into bin/
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/agent-orc

## test-unit: run unit tests with the race detector
test-unit:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKG)

## test: alias for test-unit
test: test-unit

## coverage: total coverage across unit and integration tests
coverage:
	@rm -rf covdata && mkdir -p covdata
	@$(GO) test -covermode=atomic -coverprofile=unit.out $(PKG) >/dev/null
	@# The integration tests drive a separately built binary, so their coverage
	@# lives in that subprocess. GOCOVERDIR makes the binary write it out.
	@GOCOVERDIR=$(CURDIR)/covdata $(GO) test -count=1 -tags=integration ./test/... >/dev/null
	@$(GO) tool covdata textfmt -i=covdata -o=integration.out
	@{ echo 'mode: atomic'; \
	   tail -q -n +2 unit.out integration.out \
	   | awk '{ if (!($$1 in c) || $$3+0 > c[$$1]+0) { c[$$1]=$$3; n[$$1]=$$2 } } \
	          END { for (k in c) print k, n[k], c[k] }'; \
	 } > coverage.out
	@$(GO) tool cover -func=coverage.out | tail -1 | awk '{print "total coverage: " $$3}'

## test-integration: run tests that shell out to real git/gh
test-integration:
	$(GO) test -race -tags=integration ./test/...

## tidy: sync go.mod/go.sum with the source
tidy:
	$(GO) mod tidy

## verify-clean: fail if fmt/tidy would change or add any file
verify-clean: fmt tidy
	@dirty=$$(git status --porcelain); \
	if [ -n "$$dirty" ]; then \
		echo "working tree is dirty after 'make fmt tidy':"; \
		echo "$$dirty"; \
		echo "commit the result of 'make fmt tidy'"; exit 1; \
	fi

## clean: remove build and test artifacts
clean:
	$(GO) clean -testcache
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out unit.out integration.out covdata

## commit-check: enforce the commit policy over RANGE (default origin/main..HEAD)
commit-check:
	@./scripts/check-commits.sh $(RANGE)

## ci: the ci and commit-policy checks that run locally; do this before pushing
ci: fmt-check vet lint build test-unit test-integration commit-check

.PHONY: help fmt fmt-check vet lint lint-install print-lint-version build test-unit test test-integration coverage tidy verify-clean clean commit-check ci
