# agent-orc

A single installable CLI that takes one or more tasks — JIRA tickets, GitHub
issues, or raw prompts — and for each one creates an isolated git worktree and
branch, launches a configured agentic CLI (Claude Code, Copilot CLI, Codex)
inside it, tracks spend against a per-task budget, and opens a draft PR when
the agent finishes.

It is a thin dispatcher and tracker, not a new agent runtime. It never talks to
a model directly — it only shells out to CLIs that already exist. Worktrees do
the isolation, the OS does the concurrency, files do the state keeping.

## Status

Early. Built in phases:

| Phase | Scope | State |
| ----- | ----- | ----- |
| 0 | `run` for a single task; worktree lifecycle; Claude adapter | planned |
| 1 | Multi-CLI adapters, YAML batch config, JIRA/GitHub source fetching | planned |
| 2 | Subagent seeding, budget caps, `status` | planned |
| 3 | Draft PR chain, commit sanitization, `cleanup`, `logs` | planned |
| 4 | Agentic review with a capped worker↔reviewer loop | planned |

## Development

```sh
make help    # list targets
make ci      # the CI checks that run locally — do this before pushing
make build   # compile to bin/agent-orc
```

Requires Go 1.22+ and `golangci-lint` (`make lint-install` fetches the pinned
version).

### Commit policy

Every commit must:

- carry a **one-line conventional subject** (`feat:`, `fix:`, `ci:`, `docs:`,
  `test:`, `refactor:`, `chore:`, `perf:`, `build:`, `revert:`), at most 72
  characters, with no prose body — only trailers below it;
- be **DCO signed off** (`git commit -s`);
- be **GPG signed** with a key registered on GitHub;
- contain **no AI-attribution trailers** — no `Co-authored-by:` naming an
  agent or a `[bot]`, no `Claude-Session:`, no "Generated with" footer.

`make commit-check` enforces all of this locally over `origin/main..HEAD`; the
`commit-policy` workflow enforces it again on every PR, and additionally asks
the GitHub API whether each signature is trusted.

## License

MIT — see [LICENSE](LICENSE).
