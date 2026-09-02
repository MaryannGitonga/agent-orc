package adapter

import "github.com/MaryannGitonga/agent-orc/internal/task"

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

// SubagentDir is empty: Codex has no subagent definitions to seed.
func (Codex) SubagentDir() string { return "" }

// ParseUsage reports nothing: Codex writes no machine-readable cost to its
// output.
func (Codex) ParseUsage(string) (*Usage, error) { return nil, ErrNoUsage }
