// Package adapter turns a task into the argv that launches an agentic CLI. It
// is the only place that knows what a given CLI's flags look like.
package adapter

import "github.com/MaryannGitonga/agent-orc/internal/task"

// ClaudeCommand returns the argv that runs t under Claude Code in print mode,
// which executes the prompt to completion and exits with a JSON result.
func ClaudeCommand(t task.Task) []string {
	argv := []string{"claude", "-p", t.Render(), "--output-format", "json"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	return argv
}
