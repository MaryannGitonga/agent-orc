# agent-orc

A single installable CLI that takes one or more tasks (JIRA tickets, GitHub
issues, or raw prompts) and, for each one, creates an isolated git worktree and
branch, launches a configured agentic CLI (Claude Code, Copilot CLI, Codex)
inside it, tracks spend against a per-task budget, and opens a draft PR when
the agent finishes.

It is a thin dispatcher and tracker, not a new agent runtime. It never talks to
a model directly; it only shells out to CLIs that already exist. Worktrees do
the isolation, the OS does the concurrency, files do the state keeping.

## Status

Early. Built in phases:

| Phase | Scope | State |
| ----- | ----- | ----- |
| 0 | `run` for a single task; worktree lifecycle; Claude adapter | done |
| 1 | Multi-CLI adapters, YAML batch config, JIRA/GitHub source fetching | planned |
| 2 | Subagent seeding, budget caps, `status` | planned |
| 3 | Draft PR chain, commit sanitization, `cleanup`, `logs` | planned |
| 4 | Agentic review with a capped worker↔reviewer loop | planned |

## Usage

```sh
agent-orc run --id PROJ-1234 --repo . --cli claude \
  --prompt "Fix the null-pointer in the FX sync retry handler" \
  --branch fix/proj-1234 --base-branch main --model opus-4-6
```

`run` returns as soon as the task is dispatched. It creates a worktree, starts
a detached supervisor that drives the agent inside it, and records everything
under `~/.agent-orc` (override with `AGENT_ORC_HOME`):

```
~/.agent-orc/
  state/PROJ-1234.json          status, branch, pid, exit code
  logs/PROJ-1234.log            the agent's own output
  logs/PROJ-1234.supervisor.log what agent-orc did around it
  worktrees/PROJ-1234/          the isolated checkout
```

`--id`, `--prompt` and `--cli` are required. There is no default CLI: the
tool dispatches to whichever agent you actually have installed, and it checks
that the binary is on PATH before creating a worktree or a branch. `--repo`
defaults to the current directory, `--base-branch` to the repository's default
branch, and `--branch` to `agent-orc/<id>`.

## Development

```sh
make help    # list targets
make ci      # the CI checks that run locally; do this before pushing
make build   # compile to bin/agent-orc
```

Requires Go 1.22+ and `golangci-lint` (`make lint-install` fetches the pinned
version).

### Commit policy

Every commit must:

- carry a **one-line conventional subject** (`feat:`, `fix:`, `ci:`, `docs:`,
  `test:`, `refactor:`, `chore:`, `perf:`, `build:`, `revert:`), at most 72
  characters, with no prose body, only trailers below it;
- be **DCO signed off** (`git commit -s`);
- be **GPG signed** with a key registered on GitHub;
- contain **no AI-attribution trailers**: no `Co-authored-by:` naming an
  agent or a `[bot]`, no `Claude-Session:`, no "Generated with" footer.

`make commit-check` enforces all of this locally over `origin/main..HEAD`; the
`commit-policy` workflow enforces it again on every PR, and additionally asks
the GitHub API whether each signature is trusted.

## License

MIT. See [LICENSE](LICENSE).
