package orc

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// TestTaskSpendAddsSessionsNotResumes covers how Claude reports cost: a
// resumed run's total already includes the session's earlier runs, so the
// worker's latest result is its whole spend, while each review run is a
// session of its own and is added.
func TestTaskSpendAddsSessionsNotResumes(t *testing.T) {
	layout := testLayout(t)
	record := state.Task{
		Task:    task.Task{ID: "S-1", CLI: task.CLIClaude, Review: task.Review{Enabled: true, CLI: task.CLIClaude}},
		LogPath: layout.LogFile("S-1"),
	}
	writeFile(t, record.LogPath,
		`{"type":"result","total_cost_usd":0.5}`+"\n"+`{"type":"result","total_cost_usd":1.25}`+"\n")
	writeFile(t, layout.ReviewLogFile("S-1"),
		`{"type":"result","total_cost_usd":0.25}`+"\n"+`{"type":"result","total_cost_usd":0.5}`+"\n")

	got := taskSpend(layout, record)
	if got == nil || got.CostUSD == nil || *got.CostUSD != 2.0 {
		t.Errorf("taskSpend() = %+v, want $1.25 from the worker plus $0.75 of reviews", got)
	}
}

func TestBudgetLeft(t *testing.T) {
	limit, credits := 1.0, 50.0
	spend := func(usd float64) *adapter.Usage { return &adapter.Usage{CostUSD: &usd} }

	left, err := budgetLeft(task.Budget{USD: &limit}, spend(0.75))
	if err != nil || left.USD == nil || *left.USD != 0.25 {
		t.Errorf("budgetLeft($1 cap, $0.75 spent) = %+v, %v, want $0.25", left, err)
	}
	if _, err := budgetLeft(task.Budget{USD: &limit}, spend(1.0)); !errors.Is(err, ErrBudgetSpent) {
		t.Errorf("budgetLeft at the cap = %v, want ErrBudgetSpent", err)
	}
	// Nothing reported, or a credit budget: passed on whole, never refused.
	if left, err := budgetLeft(task.Budget{USD: &limit}, nil); err != nil || *left.USD != 1.0 {
		t.Errorf("budgetLeft with no spend reported = %+v, %v, want the whole budget", left, err)
	}
	if left, err := budgetLeft(task.Budget{Credits: &credits}, spend(5)); err != nil || *left.Credits != 50 {
		t.Errorf("budgetLeft for credits = %+v, %v, want them passed on whole", left, err)
	}
}

func TestClaudeVersionWarnsBeforeCumulativeCosts(t *testing.T) {
	for out, want := range map[string]Level{
		"2.1.100 (Claude Code)": LevelWarn,
		"2.0.999 (Claude Code)": LevelWarn,
		"2.1.277 (Claude Code)": LevelOK,
		"2.1.330 (Claude Code)": LevelOK,
		"3.0.0 (Claude Code)":   LevelOK,
		"something else":        LevelOK,
	} {
		fakeCLI(t, "claude", "echo '"+out+"'")
		if got := checkClaudeVersion(); got.Level != want || (want == LevelWarn && !strings.Contains(got.Detail, "under-counted")) {
			t.Errorf("version %q = %+v, want %s", out, got, want)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
