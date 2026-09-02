package adapter

import "github.com/MaryannGitonga/agent-orc/internal/task"

// Copilot drives the GitHub Copilot CLI.
type Copilot struct{}

// Name identifies the CLI.
func (Copilot) Name() task.CLI { return task.CLICopilot }

// BuildCommand runs the task through Copilot's programmatic mode.
//
// --allow-all-tools is what makes an unattended run possible at all: without
// it the CLI stops to ask for approval on every tool call, and there is no
// terminal attached to answer. The isolation that makes this acceptable is the
// worktree — the agent has its own checkout and its own branch.
func (Copilot) BuildCommand(t task.Task) []string {
	argv := []string{"copilot", "-p", t.Render(), "--allow-all-tools"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	return argv
}
