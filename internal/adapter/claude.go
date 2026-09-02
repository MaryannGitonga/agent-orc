package adapter

import "github.com/MaryannGitonga/agent-orc/internal/task"

// Claude drives Claude Code.
type Claude struct{}

// Name identifies the CLI.
func (Claude) Name() task.CLI { return task.CLIClaude }

// BuildCommand runs the task in Claude Code's print mode, which executes the
// prompt to completion and exits with a JSON result.
func (Claude) BuildCommand(t task.Task) []string {
	argv := []string{"claude", "-p", t.Render(), "--output-format", "json"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	return argv
}
