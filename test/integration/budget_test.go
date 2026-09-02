//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunAppliesTheNativeBudgetCapAndRecordsSpend covers §9 end to end: the
// dollar cap reaches the CLI at launch, and what the run actually cost is read
// back out of its output afterwards.
func TestRunAppliesTheNativeBudgetCapAndRecordsSpend(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")

	// A stub that emits a Claude-shaped JSON result carrying its cost.
	stub := stubAgent(t, "claude", receipt,
		`printf '{"type":"result","total_cost_usd":0.75,"usage":{"input_tokens":100,"output_tokens":50}}\n'`)

	out, err := orcRun(t, home, stub, "run",
		"--id", "BUDGET-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--budget-usd", "2")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "BUDGET-1", "done", "failed")

	if got := readFile(t, receipt); !strings.Contains(got, "arg=--max-budget-usd") || !strings.Contains(got, "arg=2") {
		t.Errorf("receipt = %q, want the native dollar cap passed at launch", got)
	}

	var rec struct {
		SpentUSD *float64 `json:"spent_usd"`
		Tokens   *int     `json:"tokens"`
	}
	data := readFile(t, filepath.Join(home, "state", "BUDGET-1.json"))
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatalf("parsing state: %v\n%s", err, data)
	}
	if rec.SpentUSD == nil || *rec.SpentUSD != 0.75 {
		t.Errorf("spent_usd = %v, want 0.75", rec.SpentUSD)
	}
	if rec.Tokens == nil || *rec.Tokens != 150 {
		t.Errorf("tokens = %v, want 150", rec.Tokens)
	}
}

// TestRunWarnsWhenTheCLICannotEnforceTheBudget checks that an unenforceable
// budget is said out loud rather than silently ignored.
func TestRunWarnsWhenTheCLICannotEnforceTheBudget(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "copilot", receipt, "true")

	out, err := orcRun(t, home, stub, "run",
		"--id", "BUDGET-2", "--repo", repo, "--prompt", "do it",
		"--cli", "copilot", "--budget-usd", "2")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "credits") {
		t.Errorf("run output = %q, want a warning that copilot caps in credits", out)
	}
	waitForStatus(t, home, "BUDGET-2", "done", "failed")

	status, err := orcRun(t, home, stub, "status")
	if err != nil {
		t.Fatalf("agent-orc status = %v\n%s", err, status)
	}
	if !strings.Contains(status, "note") || !strings.Contains(status, "credits") {
		t.Errorf("status = %q, want the unenforced budget noted", status)
	}
}

// TestStatusShowsEveryTask checks the §11 status table.
func TestStatusShowsEveryTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		`printf '{"total_cost_usd":0.25}\n'`)

	for _, id := range []string{"S-1", "S-2"} {
		if out, err := orcRun(t, home, stub, "run",
			"--id", id, "--repo", repo, "--cli", "claude", "--prompt", "do it", "--budget-usd", "1"); err != nil {
			t.Fatalf("run %s = %v\n%s", id, err, out)
		}
		waitForStatus(t, home, id, "done", "failed")
	}

	out, err := orcRun(t, home, stub, "status")
	if err != nil {
		t.Fatalf("agent-orc status = %v\n%s", err, out)
	}
	for _, want := range []string{"ID", "SPEND", "S-1", "S-2", "done", "$0.25 / $1.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
}

// TestRunSeedsSubagentsFromTheLibrary covers §8 step 3: definitions are copied
// into the worktree, and excluded so the agent does not commit them.
func TestRunSeedsSubagentsFromTheLibrary(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()

	lib := filepath.Join(home, "agents", "claude")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lib, "reviewer.md"), "# reviewer\n")

	// The stub commits everything it can see, so anything seeded that is not
	// properly excluded would show up in the commit.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add -A && git commit --no-gpg-sign -m 'feat: work' >/dev/null")

	out, err := orcRun(t, home, stub, "run",
		"--id", "SEED-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--subagents")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "reviewer.md") {
		t.Errorf("run output = %q, want it to name the seeded definition", out)
	}

	rec := waitForStatus(t, home, "SEED-1", "done", "failed")
	if rec.Status != "done" {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if _, err := os.Stat(filepath.Join(rec.Worktree, ".claude/agents/reviewer.md")); err != nil {
		t.Errorf("the definition was not seeded into the worktree: %v", err)
	}

	files := git(t, repo, "show", "--name-only", "--format=", "agent-orc/seed-1")
	if strings.Contains(files, ".claude") {
		t.Errorf("the seeded definitions were committed by the agent:\n%s", files)
	}
	if !strings.Contains(files, "out.txt") {
		t.Errorf("the agent's own work is missing from the commit:\n%s", files)
	}
}

// TestRunRefusesSubagentsForACLIThatHasNone fails at launch rather than
// running a task configured differently from what was asked for.
func TestRunRefusesSubagentsForACLIThatHasNone(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "codex", filepath.Join(t.TempDir(), "receipt"), "true")

	out, err := orcRun(t, home, stub, "run",
		"--id", "SEED-2", "--repo", repo, "--prompt", "do it", "--cli", "codex", "--subagents")
	if err == nil {
		t.Fatalf("run succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "no subagent mechanism") {
		t.Errorf("error = %q, want it to explain codex has no subagents", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "SEED-2")); !os.IsNotExist(statErr) {
		t.Error("a worktree was left behind by a task that never launched")
	}
}

// TestStopKillsARunningTask covers §11's stop command against a real process.
func TestStopKillsARunningTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	// A stub that runs long enough to be stopped.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "sleep 120")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "STOP-1", "--repo", repo, "--cli", "claude", "--prompt", "take your time"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	running := waitForStatus(t, home, "STOP-1", "running")

	out, err := orcRun(t, home, stub, "stop", "STOP-1")
	if err != nil {
		t.Fatalf("agent-orc stop = %v\n%s", err, out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stop output = %q, want it to confirm the task stopped", out)
	}

	got := waitForStatus(t, home, "STOP-1", "stopped")
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	// The work is left where a human can look at it.
	if _, err := os.Stat(running.Worktree); err != nil {
		t.Errorf("worktree was removed by stop: %v", err)
	}
}

// TestBatchAppliesBudgetDefaultsAndOverrides checks the §5 defaults mechanism
// for the fields Phase 2 adds.
func TestBatchAppliesBudgetDefaultsAndOverrides(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receiptDir := t.TempDir()
	aReceipt := filepath.Join(receiptDir, "a")

	stub := stubAgent(t, "claude", aReceipt, "true")

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, fmt.Sprintf(`repo: %s
defaults:
  cli: claude
  budget_usd: 2.00
tasks:
  - id: B-1
    prompt: inherits the default budget
  - id: B-2
    prompt: tightens it
    budget_usd: 0.50
`, repo))

	if out, err := orcRun(t, home, stub, "run", batch); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	for _, id := range []string{"B-1", "B-2"} {
		waitForStatus(t, home, id, "done", "failed")
	}

	receipt := readFile(t, aReceipt)
	for _, want := range []string{"arg=2", "arg=0.5"} {
		if !strings.Contains(receipt, want) {
			t.Errorf("receipt is missing %q; want both the default and the override:\n%s", want, receipt)
		}
	}
}
