package adapter

import (
	"slices"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// argAfter returns the argument following flag, or "" if it is absent.
func argAfter(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i == -1 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func TestForReturnsAnAdapterPerKnownCLI(t *testing.T) {
	for _, cli := range task.KnownCLIs {
		a, err := For(cli)
		if err != nil {
			t.Errorf("For(%q) = %v, want an adapter", cli, err)
			continue
		}
		if a.Name() != cli {
			t.Errorf("For(%q).Name() = %q, want %q", cli, a.Name(), cli)
		}
	}
}

func TestForRejectsAnUnknownCLI(t *testing.T) {
	_, err := For("gemini")
	if err == nil {
		t.Fatal("For() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error = %q, want it to list the supported CLIs", err)
	}
}

func TestSupportedIsStablyOrdered(t *testing.T) {
	want := []string{"claude", "codex", "copilot"}
	if got := Supported(); !slices.Equal(got, want) {
		t.Errorf("Supported() = %v, want %v", got, want)
	}
}

// TestEveryAdapterCarriesThePromptAndTheModel is the contract every adapter
// has to meet, whatever its own flag names are.
func TestEveryAdapterCarriesThePromptAndTheModel(t *testing.T) {
	tk := task.Task{Prompt: "fix the retry handler", Model: "some-model"}
	for _, cli := range task.KnownCLIs {
		t.Run(string(cli), func(t *testing.T) {
			a, err := For(cli)
			if err != nil {
				t.Fatal(err)
			}
			argv := a.BuildCommand(tk)

			if argv[0] != string(cli) {
				t.Errorf("argv[0] = %q, want %q", argv[0], cli)
			}
			joined := strings.Join(argv, "\x00")
			if !strings.Contains(joined, "fix the retry handler") {
				t.Errorf("argv = %q, want it to carry the prompt", argv)
			}
			if !strings.Contains(joined, "Do NOT push") {
				t.Errorf("argv = %q, want it to carry the rendered operating rules", argv)
			}
			if got := argAfter(argv, "--model"); got != "some-model" {
				t.Errorf("--model = %q, want %q", got, "some-model")
			}
		})
	}
}

// TestEveryAdapterOmitsAnUnsetModel keeps the CLI's own default in play
// instead of passing an empty flag value.
func TestEveryAdapterOmitsAnUnsetModel(t *testing.T) {
	for _, cli := range task.KnownCLIs {
		t.Run(string(cli), func(t *testing.T) {
			a, err := For(cli)
			if err != nil {
				t.Fatal(err)
			}
			if argv := a.BuildCommand(task.Task{Prompt: "x"}); slices.Contains(argv, "--model") {
				t.Errorf("argv = %q, want no --model flag when the task sets none", argv)
			}
		})
	}
}

func TestClaudeUsesPrintModeWithJSONOutput(t *testing.T) {
	argv := Claude{}.BuildCommand(task.Task{Prompt: "x"})
	if argAfter(argv, "--output-format") != "json" {
		t.Errorf("argv = %q, want --output-format json for machine-readable usage", argv)
	}
	if !slices.Contains(argv, "-p") {
		t.Errorf("argv = %q, want print mode", argv)
	}
}

func TestCopilotRunsUnattended(t *testing.T) {
	argv := Copilot{}.BuildCommand(task.Task{Prompt: "x"})
	if !slices.Contains(argv, "--allow-all-tools") {
		t.Errorf("argv = %q, want --allow-all-tools; there is no terminal to approve tool calls", argv)
	}
}

func TestCodexRunsNonInteractivelyInASandbox(t *testing.T) {
	argv := Codex{}.BuildCommand(task.Task{Prompt: "x"})
	if argv[1] != "exec" {
		t.Errorf("argv = %q, want the non-interactive 'codex exec'", argv)
	}
	if argAfter(argv, "--sandbox") != "workspace-write" {
		t.Errorf("argv = %q, want --sandbox workspace-write", argv)
	}
	// Codex takes the prompt as a positional argument, so it must come last.
	if !strings.HasPrefix(argv[len(argv)-1], "x") {
		t.Errorf("argv = %q, want the prompt as the final positional argument", argv)
	}
}

func f64(v float64) *float64 { return &v }

func TestClaudeCapsSpendInDollars(t *testing.T) {
	args, note := Claude{}.BudgetArgs(task.Budget{USD: f64(2)})
	if argAfter(args, "--max-budget-usd") != "2" {
		t.Errorf("args = %v, want --max-budget-usd 2", args)
	}
	if note != "" {
		t.Errorf("note = %q, want none; the dollar budget is enforced natively", note)
	}
}

func TestClaudeSaysSoWhenItCannotEnforceCredits(t *testing.T) {
	_, note := Claude{}.BudgetArgs(task.Budget{Credits: f64(50)})
	if !strings.Contains(note, "budget_credits") {
		t.Errorf("note = %q, want it to say credits are not enforced", note)
	}
}

func TestCopilotCapsSpendInCredits(t *testing.T) {
	args, note := Copilot{}.BudgetArgs(task.Budget{Credits: f64(50)})
	if argAfter(args, "--max-ai-credits") != "50" {
		t.Errorf("args = %v, want --max-ai-credits 50", args)
	}
	if note != "" {
		t.Errorf("note = %q, want none", note)
	}
}

func TestCopilotSaysSoWhenGivenADollarBudget(t *testing.T) {
	// Converting dollars to credits would be a guess, so agent-orc refuses to
	// make one and says the budget is unenforced instead.
	args, note := Copilot{}.BudgetArgs(task.Budget{USD: f64(2)})
	if len(args) != 0 {
		t.Errorf("args = %v, want none; copilot has no dollar cap", args)
	}
	if !strings.Contains(note, "credits") {
		t.Errorf("note = %q, want it to point at budget_credits", note)
	}
}

func TestCodexReportsThatItEnforcesNothing(t *testing.T) {
	if args, note := (Codex{}).BudgetArgs(task.Budget{}); len(args) != 0 || note != "" {
		t.Errorf("BudgetArgs(no budget) = %v, %q, want nothing to report", args, note)
	}
	_, note := Codex{}.BudgetArgs(task.Budget{USD: f64(2)})
	if !strings.Contains(note, "not enforced") {
		t.Errorf("note = %q, want it to say the budget is not enforced", note)
	}
}

func TestBudgetArgsReachTheCommandLine(t *testing.T) {
	claude := Claude{}.BuildCommand(task.Task{Prompt: "x", Budget: task.Budget{USD: f64(1.5)}})
	if argAfter(claude, "--max-budget-usd") != "1.5" {
		t.Errorf("claude argv = %v, want the budget flag", claude)
	}
	copilot := Copilot{}.BuildCommand(task.Task{Prompt: "x", Budget: task.Budget{Credits: f64(20)}})
	if argAfter(copilot, "--max-ai-credits") != "20" {
		t.Errorf("copilot argv = %v, want the credits flag", copilot)
	}
}

func TestSubagentDirs(t *testing.T) {
	if got := (Claude{}).SubagentDir(); got != ".claude/agents" {
		t.Errorf("claude SubagentDir() = %q, want %q", got, ".claude/agents")
	}
	if got := (Copilot{}).SubagentDir(); got != ".github/agents" {
		t.Errorf("copilot SubagentDir() = %q, want %q", got, ".github/agents")
	}
	// Codex has no subagent mechanism; an empty string is how that is said,
	// so the dispatcher can fail loudly instead of seeding nothing silently.
	if got := (Codex{}).SubagentDir(); got != "" {
		t.Errorf("codex SubagentDir() = %q, want it empty", got)
	}
}

func TestNewSessionIDLooksLikeAUUIDAndIsUnique(t *testing.T) {
	first, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID() = %v", err)
	}
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Errorf("NewSessionID() = %q, want a 36-character UUID", first)
	}
	// Version 4, variant 10: Claude Code rejects anything that is not a
	// well-formed UUID.
	if first[14] != '4' {
		t.Errorf("NewSessionID() = %q, want version 4", first)
	}
	if !strings.ContainsRune("89ab", rune(first[19])) {
		t.Errorf("NewSessionID() = %q, want the RFC 4122 variant", first)
	}

	second, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("NewSessionID() returned the same id twice")
	}
}

func TestSessionArgsWhereTheCLIAcceptsOne(t *testing.T) {
	for name, a := range map[string]Adapter{"claude": Claude{}, "copilot": Copilot{}} {
		t.Run(name, func(t *testing.T) {
			if got := argAfter(a.SessionArgs("abc"), "--session-id"); got != "abc" {
				t.Errorf("SessionArgs() = %v, want --session-id abc", a.SessionArgs("abc"))
			}
			if got := a.SessionArgs(""); len(got) != 0 {
				t.Errorf("SessionArgs(\"\") = %v, want nothing", got)
			}
		})
	}
	// Codex takes no session ID, which is how the review loop knows it cannot
	// hand feedback back to a Codex worker.
	if got := (Codex{}).SessionArgs("abc"); len(got) != 0 {
		t.Errorf("codex SessionArgs() = %v, want nothing", got)
	}
}

func TestResumeCommandCarriesTheFeedback(t *testing.T) {
	for name, a := range map[string]Adapter{"claude": Claude{}, "copilot": Copilot{}} {
		t.Run(name, func(t *testing.T) {
			argv, err := a.ResumeCommand("sess-1", "please fix the backoff", "some-model")
			if err != nil {
				t.Fatalf("ResumeCommand() = %v", err)
			}
			joined := strings.Join(argv, "\x00")
			if !strings.Contains(joined, "sess-1") {
				t.Errorf("argv = %v, want the session id", argv)
			}
			if !strings.Contains(joined, "please fix the backoff") {
				t.Errorf("argv = %v, want the feedback prompt", argv)
			}
			if argAfter(argv, "--model") != "some-model" {
				t.Errorf("argv = %v, want the model", argv)
			}
		})
	}
}

func TestResumeCommandNeedsASessionID(t *testing.T) {
	if _, err := (Claude{}).ResumeCommand("", "x", ""); err == nil {
		t.Error("ResumeCommand() without a session id = nil, want an error")
	}
}

func TestCodexCannotBeResumed(t *testing.T) {
	_, err := (Codex{}).ResumeCommand("sess-1", "x", "")
	if err == nil {
		t.Fatal("ResumeCommand() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "review feedback") {
		t.Errorf("error = %q, want it to explain the consequence", err)
	}
}

func TestClaudeParsesTheSessionIDFromItsOutput(t *testing.T) {
	path := logWith(t, `{"type":"result","session_id":"abc-123","total_cost_usd":0.1}`)
	got, err := Claude{}.ParseSessionID(path)
	if err != nil {
		t.Fatalf("ParseSessionID() = %v", err)
	}
	if got != "abc-123" {
		t.Errorf("ParseSessionID() = %q, want %q", got, "abc-123")
	}
}
