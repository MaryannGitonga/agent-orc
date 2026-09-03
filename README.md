# agent-orc

[![ci](https://github.com/MaryannGitonga/agent-orc/actions/workflows/ci.yml/badge.svg)](https://github.com/MaryannGitonga/agent-orc/actions/workflows/ci.yml)
[![commit-policy](https://github.com/MaryannGitonga/agent-orc/actions/workflows/commit-policy.yml/badge.svg)](https://github.com/MaryannGitonga/agent-orc/actions/workflows/commit-policy.yml)
![go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![license](https://img.shields.io/badge/license-MIT-blue)

A single installable CLI that takes one or more tasks (JIRA tickets, GitHub
issues, or raw prompts) and, for each one, creates an isolated git worktree and
branch, launches a configured agentic CLI (Claude Code, Copilot CLI, Codex)
inside it, tracks spend against a per-task budget, and opens a draft PR when
the agent finishes.

It is a thin dispatcher and tracker, not a new agent runtime. It never talks to
a model directly; it only shells out to CLIs that already exist. Worktrees do
the isolation, the OS does the concurrency, files do the state keeping.

## Usage

```sh
agent-orc run --id PROJ-1234 --repo . --cli claude \
  --prompt "Fix the null-pointer in the FX sync retry handler" \
  --branch fix/proj-1234 --base-branch main --model opus-4-6
```

A ticket reference can stand in for the prompt. It is fetched once, at launch,
and the task's own prompt is layered on top as extra instructions:

```sh
agent-orc run --id PROJ-1240 --cli claude --source github://acme/data-mesh#87
agent-orc run --id PROJ-1234 --cli claude --source jira://PROJ-1234 --prompt "Only the retry handler"
```

JIRA needs `JIRA_BASE_URL` and `JIRA_API_TOKEN` in the environment (plus
`JIRA_USER_EMAIL` on Cloud, which authenticates with basic auth rather than a
bearer token). GitHub reuses the `gh` login you already have.

To dispatch many tasks at once, put them in a YAML file (see
[`examples/tasks.yaml`](examples/tasks.yaml)) and run:

```sh
agent-orc run tasks.yaml
```

Tasks in a batch are independent: each gets its own branch, worktree and
process, so one that cannot launch does not stop the others.

When the agent exits, the same per-task process sanitizes its commits, pushes
the branch and opens a **draft** PR. No daemon is involved, and nothing reaches
the remote unsanitized. `--no-auto-pr` holds off; `agent-orc pr <id>` runs the same three
steps by hand, and is also how you retry a task left at `publish_failed`: it
recognises a branch it pushed itself, so a retry finishes the job rather than
starting an argument about who pushed what.

`agent-orc status` prints one row per task, and `agent-orc stop <id>` kills a
running one, signalling the agent's whole process group so the compilers and
test runners it spawned go with it (its worktree is left for you to look at):

```
ID       CLI      MODEL      STATUS   SPEND          BRANCH          ELAPSED
PROJ-1   claude   opus-4-6   done     $0.42 / $2.00  fix/proj-1234   1m30s
PROJ-2   copilot  gpt-5.1    running  -              chore/proj-1240 12s
```

`pending` and `running` cover the agent itself; `publishing` is the sanitize,
push and draft-PR chain that follows it, and only then `done`. `publish_failed`
means the work is committed on its branch but that chain did not finish, which
`agent-orc pr <id>` retries. The rest are terminal: `failed` for a non-zero
exit, `stopped` for a run you killed, `reviewed` for a branch an agentic review
approved, and `policy_violation` for one the agent pushed itself.

### Budgets

Each CLI meters in its own unit, and agent-orc does not invent an exchange rate
between them. Set the budget in the unit your CLI understands and it is applied
as that CLI's own native cap at launch:

| CLI | Batch field | Flag | Native cap |
| --- | --- | --- | --- |
| claude | `budget_usd` | `--budget-usd` | `--max-budget-usd` |
| copilot | `budget_credits` | `--budget-credits` | `--max-ai-credits` |
| codex | none | none | none; the budget is reported, not enforced |

A budget in a unit the CLI cannot enforce is not silently dropped. It is
warned about at launch and noted under `agent-orc status`. Actual spend is read
back out of the CLI's own output after the run, where it reports one.

### Commit policy

Before anything is pushed, every commit the task added is rewritten in one pass
that strips what should not be there and adds what must be:

- **AI attribution trailers are removed**: `Co-authored-by: Claude/Copilot/Codex`,
  `Claude-Session:`, `Generated with [Claude Code]` and its robot-emoji variant,
  `Assisted-by:`, `Generated-with:`, and any `[bot]` co-author. Each CLI is
  also asked not to add them in the first place, but those settings are
  inconsistently honoured and an agent crafting a raw `git commit` bypasses
  them, so the rewrite never depends on them working. Add your own patterns in
  `~/.agent-orc/trailers.txt`, one regular expression per line.
- **`Signed-off-by:` is added** to commits missing one when `dco_signoff: true`
  (or `--dco-signoff`) is set. It is the one trailer that is never stripped.
- **GPG signing needs nothing new.** A worktree shares the parent repo's
  config, so if `commit.gpgsign=true` is set, the agent's commits and the
  rewrite are signed exactly as a human's would be.

Only commits unique to the task's own branch are touched, in the task's own
worktree, never the base branch and never anyone else's work. The pass rewrites
messages and nothing else: a branch that contains a merge keeps it, rather than
being quietly flattened. If the rewrite
cannot complete, it is aborted and the branch is left exactly as the agent made
it; a half-rewritten branch is never pushed.

If the agent pushes its own branch despite being told not to, that branch never
went through this pass, so the task is flagged `policy_violation` rather than
treated as if agent-orc had published it.

### Agentic review

Off by default and triggered by hand. With `review.enabled` set for a task,
`agent-orc review <id>` runs an independent review of the finished branch:

```sh
agent-orc run --id PROJ-1234 --cli claude --prompt "..." \
  --review --review-cli copilot --review-model gpt-5.1
agent-orc review PROJ-1234
```

The reviewer is a **fresh session in its own worktree**, never a resume of the
worker's. A reviewer that inherited the worker's conversation would inherit
its framing of the problem too. It sees the diff and the original task, the way
a human reviewer sees the PR and not the author's scratch work. Left
unconfigured, it runs on a *different* CLI from the worker, so the two are less
likely to share a blind spot.

It answers `LGTM` or a list of concrete comments. Comments are handed back to
the worker's own session (resumed by the session ID agent-orc assigned at
launch) to address on the same branch. Anything that is neither is an error:
the round stops and waits for a human rather than guessing.

The loop is sequential and hard-capped by `max_rounds`, default 1. The worker
and reviewer never run at the same time and never message each other. A review
round draws on the same per-task budget, not a separate pool. None of this
replaces the human "Ready for review" click.

Codex tasks cannot be reviewed this way: Codex sessions cannot be resumed, so
there is nowhere to send the feedback. agent-orc says so rather than silently
starting the worker over.

### Cleaning up

```sh
agent-orc logs <id> [-f]        # print, or follow until the task finishes
agent-orc cleanup <id>          # remove the worktree and state; keep the branch
agent-orc cleanup --all --force # everything, including uncommitted work and logs
```

### Subagents

`subagents: true` (or `--subagents`) copies subagent definitions into the
worktree before launch, into the directory that CLI already reads
(`.claude/agents` for Claude Code, `.github/agents` for Copilot). Definitions
come from `~/.agent-orc/agents/<cli>/`, falling back to whatever the repository
already ships. They are ignored inside the worktree so the agent does not commit
them.

That ignore covers untracked files, which is what a seeded definition normally
is. It cannot hide one that landed on a path the repository already tracks,
because git does not consult `.gitignore` for tracked files. agent-orc warns at
launch when that is the case rather than letting it pass quietly.

`run` returns as soon as the task is dispatched. It creates a worktree, starts
a detached supervisor that drives the agent inside it, and records everything
under `~/.agent-orc` (override with `AGENT_ORC_HOME`):

```
~/.agent-orc/
  state/PROJ-1234.json          status, branch, pid, exit code
  logs/PROJ-1234.log            the agent's own output
  logs/PROJ-1234.supervisor.log what agent-orc did around it
  worktrees/PROJ-1234/          the isolated checkout
  agents/claude/*.md            your subagent definitions, seeded on request
```

`--id`, `--cli` and one of `--prompt`/`--source` are required. There is no
default CLI: the tool dispatches to whichever agent you actually have
installed, and it checks the binary is on PATH before creating a worktree or a
branch. `--repo` defaults to the current directory, `--base-branch` to the
repository's default branch, and `--branch` to `agent-orc/<id>`.

## Development

```sh
make help             # list targets
make ci               # the CI checks that run locally; do this before pushing
make build            # compile to bin/agent-orc
make test-unit        # unit tests only
make test-integration # the tests that shell out to real git and gh
```

Requires Go 1.22+ and `golangci-lint` (`make lint-install` fetches the pinned
version).

### Contributing

Conventions for commits, pull requests and tests are in
[CONTRIBUTING.md](CONTRIBUTING.md). In short, every commit must:

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
