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
	// bypassPermissions because there is nobody to answer a prompt. Without it
	// Claude Code plans the edit, is denied, and exits zero having changed
	// nothing, so the task costs money and produces an empty branch. The
	// worktree is the safety boundary here, the same argument Copilot's
	// --allow-all-tools and Codex's --sandbox workspace-write rest on.
	argv := []string{"claude", "-p", t.Render(),
		"--permission-mode", "bypassPermissions", "--output-format", "json"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	argv = append(argv, c.AttributionArgs()...)
	budget, _ := c.BudgetArgs(t.Budget)
	return append(argv, budget...)
}

// AttributionArgs turns off Claude Code's commit and PR attribution trailers.
// Passing them as inline settings rather than seeding a settings file keeps
// agent-orc from writing anything into the checkout the agent commits from.
func (Claude) AttributionArgs() []string {
	return []string{"--settings", `{"attribution":{"commit":"","pr":""}}`}
}

// BudgetArgs caps the run with Claude Code's own dollar limit, which stops the
// session once spend crosses the figure, so no polling is needed on our side.
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

// WritesJSONResult is true: agent-orc runs Claude Code with --output-format
// json, so its answer arrives wrapped.
func (Claude) WritesJSONResult() bool { return true }

// ParseResult digs the agent's message out of Claude Code's JSON envelope.
// Output that holds no such envelope is returned as it came, which is what a
// run that failed before producing a result writes.
func (Claude) ParseResult(output string) string {
	if text := jsonResultField(output); text != "" {
		return text
	}
	return output
}

// SessionArgs pins the run to a session ID so it can be resumed later.
func (Claude) SessionArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--session-id", sessionID}
}

// ParseSessionID reads the session ID out of Claude Code's JSON result. It is
// only a fallback: agent-orc normally assigns the ID at launch.
func (Claude) ParseSessionID(logPath string) (string, error) {
	obj, err := lastJSONObject(logPath)
	if err != nil {
		return "", err
	}
	id, _ := obj["session_id"].(string)
	return id, nil
}

// ResumeCommand continues an existing Claude Code session.
func (Claude) ResumeCommand(sessionID, prompt, model string) ([]string, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("no session id recorded for this task; it cannot be resumed")
	}
	argv := []string{"claude", "--resume", sessionID, "-p", prompt, "--output-format", "json"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	return argv, nil
}
