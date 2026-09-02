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
