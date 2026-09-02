// Package adapter turns a task into the exact argv that launches an agentic
// CLI. It is the only place that knows what any given CLI's flags look like.
//
// Phase 0 supports Claude Code only; the interface that generalises this
// arrives with the other CLIs.
package adapter

import "github.com/MaryannGitonga/agent-orc/internal/task"

// ClaudeCommand returns the argv that runs t under Claude Code in
// non-interactive print mode. Print mode is what makes the run scriptable: it
// executes the prompt to completion and exits, emitting a JSON result the
// later phases parse usage and session ID out of.
func ClaudeCommand(t task.Task) []string {
	argv := []string{"claude", "-p", t.Render(), "--output-format", "json"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	return argv
}
