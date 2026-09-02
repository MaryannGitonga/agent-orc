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
	// non-interactively, including any native budget cap.
	BuildCommand(t task.Task) []string
	// BudgetArgs returns the flags that cap the run, plus a note naming any
	// part of the budget this CLI cannot enforce. An empty note means the
	// whole budget is enforced natively.
	BudgetArgs(b task.Budget) (args []string, unenforced string)
	// SubagentDir is where this CLI reads subagent definitions from, relative
	// to the worktree root. An empty string means the CLI has no subagent
	// mechanism agent-orc can seed.
	SubagentDir() string
	// ParseUsage reads what the run actually cost out of its log. It returns
	// nil when the CLI reports nothing usable, because inventing a number
	// would be worse than admitting there isn't one.
	ParseUsage(logPath string) (*Usage, error)
}

// Usage is what a run actually consumed, in whichever units the CLI reports.
// Every field is optional: a nil field means "this CLI did not say".
type Usage struct {
	// CostUSD is the run's cost in dollars.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Tokens is the total tokens consumed.
	Tokens *int `json:"tokens,omitempty"`
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
