package adapter

import (
	"fmt"
	"strconv"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Claude drives Claude Code.
type Claude struct{}

// Name identifies the CLI.
func (Claude) Name() task.CLI { return task.CLIClaude }

// BuildCommand runs the task in Claude Code's print mode, which executes the
// prompt to completion and exits with a JSON result carrying the run's cost.
func (c Claude) BuildCommand(t task.Task) []string {
	argv := []string{"claude", "-p", t.Render(), "--output-format", "json"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	budget, _ := c.BudgetArgs(t.Budget)
	return append(argv, budget...)
}

// BudgetArgs caps the run with Claude Code's own dollar limit, which stops the
// session once spend crosses the figure — no polling needed on our side.
func (Claude) BudgetArgs(b task.Budget) ([]string, string) {
	var args []string
	if b.USD != nil {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(*b.USD, 'f', -1, 64))
	}
	if b.Credits != nil {
		return args, "claude caps spend in dollars; budget_credits is not enforced"
	}
	return args, ""
}

// SubagentDir is where Claude Code reads subagent definitions from.
func (Claude) SubagentDir() string { return ".claude/agents" }

// ParseUsage reads the cost out of Claude Code's final JSON result.
func (Claude) ParseUsage(logPath string) (*Usage, error) {
	obj, err := lastJSONObject(logPath)
	if err != nil {
		return nil, err
	}
	u := Usage{CostUSD: floatField(obj, "total_cost_usd")}
	if usage, ok := obj["usage"].(map[string]any); ok {
		var total int
		var any bool
		for _, key := range []string{
			"input_tokens", "output_tokens",
			"cache_creation_input_tokens", "cache_read_input_tokens",
		} {
			if n := intField(usage, key); n != nil {
				total += *n
				any = true
			}
		}
		if any {
			u.Tokens = &total
		}
	}
	if u.CostUSD == nil && u.Tokens == nil {
		return nil, fmt.Errorf("%s: %w", logPath, ErrNoUsage)
	}
	return &u, nil
}
