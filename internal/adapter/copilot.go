package adapter

import (
	"fmt"
	"strconv"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Copilot drives the GitHub Copilot CLI.
type Copilot struct{}

// Name identifies the CLI.
func (Copilot) Name() task.CLI { return task.CLICopilot }

// BuildCommand runs the task through Copilot's non-interactive mode.
//
// --allow-all-tools is what makes an unattended run possible at all: the CLI
// documents it as required for non-interactive use, because otherwise it stops
// to ask for approval on every tool call and there is no terminal to answer.
// The isolation that makes this acceptable is the worktree: the agent has its
// own checkout and its own branch.
func (c Copilot) BuildCommand(t task.Task) []string {
	argv := []string{"copilot", "-p", t.Render(), "--allow-all-tools"}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	budget, _ := c.BudgetArgs(t.Budget)
	return append(argv, budget...)
}

// BudgetArgs caps the run with Copilot's AI-credit limit. Copilot meters in
// credits rather than dollars, and agent-orc does not guess an exchange rate:
// a dollar budget is reported as unenforced for this CLI instead.
func (Copilot) BudgetArgs(b task.Budget) ([]string, string) {
	var args []string
	if b.Credits != nil {
		args = append(args, "--max-ai-credits", strconv.FormatFloat(*b.Credits, 'f', -1, 64))
	}
	if b.USD != nil {
		return args, "copilot caps spend in AI credits, not dollars; set budget_credits to cap this task"
	}
	return args, ""
}

// AttributionArgs returns nothing: the Copilot CLI has no documented setting
// that suppresses its Co-authored-by trailer, and prompt-level instructions
// are reported not to hold: it complies for one commit and adds the trailer
// again on the next. The sanitization pass before push is what actually
// removes it.
func (Copilot) AttributionArgs() []string { return nil }

// SubagentDir is where the Copilot CLI reads custom agent definitions from.
func (Copilot) SubagentDir() string { return ".github/agents" }

// ParseUsage reports nothing: the Copilot CLI does not write a machine-readable
// cost to its output, and a made-up number would be worse than none.
func (Copilot) ParseUsage(string) (*Usage, error) { return nil, ErrNoUsage }

// SessionArgs pins the run to a session ID so it can be resumed later.
func (Copilot) SessionArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--session-id", sessionID}
}

// ParseSessionID returns nothing: the Copilot CLI writes no machine-readable
// session marker. Resuming works because agent-orc assigned the ID at launch.
func (Copilot) ParseSessionID(string) (string, error) { return "", nil }

// ResumeCommand continues an existing Copilot session.
func (Copilot) ResumeCommand(sessionID, prompt, model string) ([]string, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("no session id recorded for this task; it cannot be resumed")
	}
	argv := []string{"copilot", "--resume", sessionID, "-p", prompt, "--allow-all-tools"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	return argv, nil
}
