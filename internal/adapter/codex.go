package adapter

import (
	"fmt"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Codex drives the OpenAI Codex CLI.
type Codex struct{}

// Name identifies the CLI.
func (Codex) Name() task.CLI { return task.CLICodex }

// BuildCommand runs the task through `codex exec`, Codex's non-interactive
// mode. The sandbox is set to workspace-write so the agent can edit and commit
// inside its own worktree without being able to reach the rest of the machine.
func (Codex) BuildCommand(t task.Task) []string {
	argv := []string{"codex", "exec", "--sandbox", "workspace-write"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	return append(argv, t.Render())
}

// BudgetArgs enforces nothing: Codex exposes no pre-set spend or turn cap for
// a non-interactive run. Saying so at launch is the honest answer: pretending
// to cap a run that is not capped would be worse than the warning.
func (Codex) BudgetArgs(b task.Budget) ([]string, string) {
	if b.IsZero() {
		return nil, ""
	}
	return nil, "codex exposes no native spend cap; this task's budget is reported, not enforced"
}

// AttributionArgs returns nothing. Codex's commit_attribution setting lives in
// the user's own ~/.codex/config.toml rather than in a per-run flag, so it is
// left to the user to set globally; the sanitization pass covers the run
// either way.
func (Codex) AttributionArgs() []string { return nil }

// SubagentDir is empty: Codex has no subagent definitions to seed.
func (Codex) SubagentDir() string { return "" }

// ParseUsage reports nothing: Codex writes no machine-readable cost to its
// output.
func (Codex) ParseUsage(string) (*Usage, error) { return nil, ErrNoUsage }

// SessionArgs returns nothing: Codex takes no session ID at launch.
func (Codex) SessionArgs(string) []string { return nil }

// ParseSessionID returns nothing: Codex writes no machine-readable session
// marker to its output.
func (Codex) ParseSessionID(string) (string, error) { return "", nil }

// ResumeCommand reports that Codex cannot be resumed by agent-orc. Saying so
// is the point: the review loop needs to hand feedback back to the original
// session, and a task run under Codex simply cannot do that.
func (Codex) ResumeCommand(string, string, string) ([]string, error) {
	return nil, fmt.Errorf("codex sessions cannot be resumed by agent-orc, so review feedback cannot be looped back")
}
