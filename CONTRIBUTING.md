# Contributing to agent-orc

Thanks for taking a look. This file is the whole agreement: what CI enforces,
and the conventions it cannot check.

## Getting set up

Requires Go 1.22 or newer, and `golangci-lint` for the lint target:

```sh
make lint-install   # fetches the pinned version into GOPATH/bin
make build          # compiles to bin/agent-orc
make help           # lists every target
```

Running the tool for real also needs at least one agentic CLI on PATH
(`claude`, `copilot` or `codex`), plus `gh` for GitHub sources and draft PRs.
Neither is needed to build or to run the test suite, which stubs them.

## Before you push

```sh
make ci
```

That runs the same checks the workflows do: formatting, `go vet` under both
build tags, `golangci-lint`, the build, unit tests and integration tests under
the race detector, and the commit policy over `origin/main..HEAD`. Pushing on a
red local run just moves the failure to a slower machine.

Two checks cannot run against a working copy and only happen in CI:
`verify-clean`, which needs a committed tree, and signature verification, which
asks the GitHub API whether your key is trusted.

## Commits

Every commit must:

- have a **one-line subject** with a conventional prefix, lowercase and
  imperative: `feat:`, `fix:`, `ci:`, `docs:`, `test:`, `refactor:`, `chore:`,
  `perf:`, `build:` or `revert:`. At most 72 characters;
- have **no prose body**. Anything below the subject must be a trailer. If a
  change genuinely needs explaining, it belongs in the PR description or in a
  code comment next to the thing that is surprising;
- be **signed off** for DCO: commit with `-s`;
- be **GPG signed** with a key registered on GitHub;
- carry **no AI-attribution trailer**. No `Co-authored-by:` naming an agent or
  a `[bot]`, no `Claude-Session:`, no `Assisted-by:`, no "Generated with"
  footer. These are stripped from agent output by `internal/sanitize` and
  rejected outright by the `commit-policy` workflow.

A correct commit looks exactly like this:

```
feat: add jira source fetcher

Signed-off-by: Your Name <you@example.com>
```

Check your work with `make commit-check`, which runs `scripts/check-commits.sh`
over `origin/main..HEAD`. Override the range with `RANGE=...` if you need to.

## Pull requests

- **Open as a draft** unless you are asking for a merge right now.
- Title is the commit subject for a single-commit PR, otherwise a one-line
  summary with the same conventional prefix.
- Keep the description short: a sentence or two of summary, a terse list of
  what changed, and one line on how it was verified. No section templates, no
  restating the diff.
- If the description cannot fit in about fifteen lines, the PR is too big.
  Split it.

## Code

- **Standard library first.** Add a dependency when it removes real code, not
  when it saves a few lines.
- Match the surrounding style. `gofmt`-clean and `golangci-lint`-clean, both of
  which `make ci` checks.
- Comment the surprising part, not the obvious one. A comment that restates the
  code will be removed; one that records why an approach was rejected will not.
- No em or en dashes in prose, comments or program output. Rewrite the sentence
  or use a colon, a semicolon or parentheses.

## Tests

Unit tests live next to the code they cover, as `_test.go`. Tests that shell
out to real `git` or `gh`, or that drive a built `agent-orc` binary, go under
`test/integration` behind the `integration` build tag.

```sh
make test-unit          # unit tests, race detector, coverage profile
make test-integration   # the tests that shell out to real tools
```

Two things are worth knowing before you trust a local run:

- The integration tests build the `agent-orc` binary at runtime, so Go's test
  cache does not notice changes to non-test source. Use `-count=1` when
  checking that a fix actually changed an integration result.
- They isolate git from your own configuration, including config injected
  through `GIT_CONFIG_COUNT`. If you add a helper that shells out to git,
  reuse `gitEnv` rather than building the environment yourself.

When you fix a bug, add the test first and watch it fail. A test that passes
against the unfixed code is not testing the fix.

## Releasing

Tag it. Everything else is automatic:

```sh
git tag -a v0.2.0 -m v0.2.0
git push origin v0.2.0
```

The `release` workflow re-runs the checks, cross-compiles for linux and darwin
on amd64 and arm64, stamps the tag into `internal/version.Version`, writes
`checksums.txt`, and publishes a GitHub release with generated notes.

Two things to do by hand:

- bump the version badge at the top of `README.md`, which is static because a
  private repository cannot be read by shields.io;
- update the `VERSION=` line in the README's install snippet.

Use annotated tags (`-a`). The workflow passes `--verify-tag`, and
`git describe` needs one for `make build` to stamp a sensible version.
