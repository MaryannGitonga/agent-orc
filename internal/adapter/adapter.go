// Package adapter turns a task into the exact argv that launches an agentic
// CLI. It is the only place that knows what any given CLI's flags look like,
// and the only real abstraction in the tool.
//
// Adding support for a new CLI means adding one file here. Nothing else
// changes.
package adapter

import (
	"fmt"
	"sort"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Adapter knows how to launch one agentic CLI.
type Adapter interface {
	// Name is the CLI this adapter drives.
	Name() task.CLI
	// BuildCommand returns the argv that runs the task to completion
	// non-interactively.
	BuildCommand(t task.Task) []string
}

// registry holds one adapter per supported CLI.
var registry = map[task.CLI]Adapter{
	task.CLIClaude:  Claude{},
	task.CLICopilot: Copilot{},
	task.CLICodex:   Codex{},
}

// For returns the adapter driving the named CLI.
func For(cli task.CLI) (Adapter, error) {
	a, ok := registry[cli]
	if !ok {
		return nil, fmt.Errorf("no adapter for cli %q, want one of %v", cli, Supported())
	}
	return a, nil
}

// Supported lists the CLIs that have an adapter, in a stable order.
func Supported() []string {
	names := make([]string, 0, len(registry))
	for cli := range registry {
		names = append(names, string(cli))
	}
	sort.Strings(names)
	return names
}
