//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// record mirrors the fields of the on-disk state file this test asserts on.
type record struct {
	ID          string `json:"id"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Worktree    string `json:"worktree"`
	LogPath     string `json:"log_path"`
	ExitCode    *int   `json:"exit_code"`
	PID         int    `json:"pid"`
	Error       string `json:"error"`
	CLI         string `json:"cli"`
	Model       string `json:"model"`
	DCOSignoff  bool   `json:"dco_signoff"`
	TestRuns    int    `json:"test_runs"`
	TestCommand string `json:"test_command"`
	TestsPassed *bool  `json:"tests_passed"`
	PRURL       string `json:"pr_url"`
	Review      struct {
		Enabled bool   `json:"enabled"`
		Auto    bool   `json:"auto"`
		CLI     string `json:"cli"`
		Model   string `json:"model"`
	} `json:"review"`
}

// orcRunWithPath runs agent-orc with PATH set to exactly pathDir.
func orcRunWithPath(t *testing.T, home, pathDir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Env = append(cmd.Env, "AGENT_ORC_HOME="+home, "PATH="+pathDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// orcRun runs the agent-orc binary with a stub CLI on PATH and an isolated
// AGENT_ORC_HOME, and returns its combined output.
func orcRun(t *testing.T, home, stubDir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Env = append(cmd.Env,
		"AGENT_ORC_HOME="+home,
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitForStatus polls a task's state file until it reaches one of want.
func waitForStatus(t *testing.T, home, id string, want ...string) record {
	t.Helper()
	path := filepath.Join(home, "state", id+".json")
	deadline := time.Now().Add(30 * time.Second)
	var last record
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(data, &last) == nil {
			for _, w := range want {
				if last.Status == w {
					return last
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %q never reached %v (last status %q, error %q)", id, want, last.Status, last.Error)
	return last
}

// TestRunDispatchesATaskEndToEnd covers the whole lifecycle: a worktree on a
// new branch, the agent run inside it, and the task recorded as done.
func TestRunDispatchesATaskEndToEnd(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")

	// The stub stands in for an agent that works and commits locally.
	stub := stubAgent(t, "claude", receipt,
		"echo 'agent output' \n"+
			"printf 'fixed\\n' > fix.txt\n"+
			"git add . && git commit --no-gpg-sign -m 'fix: stub work' >/dev/null")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-1", "--repo", repo, "--cli", "claude",
		"--prompt", "fix the retry handler", "--model", "opus-4-6")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "PROJ-1  started") {
		t.Errorf("run output = %q, want it to report the task started", out)
	}

	got := waitForStatus(t, home, "PROJ-1", "done", "failed")
	if got.Status != "done" {
		t.Fatalf("status = %q (error %q), want done\nsupervisor log:\n%s",
			got.Status, got.Error, readFile(t, filepath.Join(home, "logs", "PROJ-1.supervisor.log")))
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}
	if want := "agent-orc/proj-1"; got.Branch != want {
		t.Errorf("branch = %q, want the default %q", got.Branch, want)
	}

	// The agent ran in the task's worktree, on the task's branch.
	receiptText := readFile(t, receipt)
	if !strings.Contains(receiptText, "cwd="+got.Worktree) {
		t.Errorf("agent ran in the wrong directory; receipt:\n%s", receiptText)
	}
	for _, want := range []string{"arg=-p", "arg=--model", "arg=opus-4-6", "arg=--output-format", "arg=json"} {
		if !strings.Contains(receiptText, want) {
			t.Errorf("agent was not passed %q; receipt:\n%s", want, receiptText)
		}
	}
	if !strings.Contains(receiptText, "fix the retry handler") ||
		!strings.Contains(receiptText, "Do NOT push") {
		t.Errorf("prompt was not rendered with the operating rules; receipt:\n%s", receiptText)
	}

	// The agent's commit landed on the task branch and nowhere else.
	if !strings.Contains(git(t, repo, "log", "--oneline", "agent-orc/proj-1"), "fix: stub work") {
		t.Error("the agent's commit is missing from the task branch")
	}
	if strings.Contains(git(t, repo, "log", "--oneline", "main"), "fix: stub work") {
		t.Error("the agent's commit leaked onto main")
	}
	if _, err := os.Stat(filepath.Join(got.Worktree, "fix.txt")); err != nil {
		t.Errorf("the agent's file is missing from the worktree: %v", err)
	}

	// The agent's stdout was captured for 'agent-orc logs'.
	if !strings.Contains(readFile(t, got.LogPath), "agent output") {
		t.Errorf("agent output was not captured in %s", got.LogPath)
	}
}

// TestRunRecordsAFailingAgent: a non-zero exit is surfaced, and the worktree
// is left in place for inspection.
func TestRunRecordsAFailingAgent(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "echo 'boom' >&2\nexit 3")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-2", "--repo", repo, "--cli", "claude", "--prompt", "break things"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}

	got := waitForStatus(t, home, "PROJ-2", "done", "failed")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", got.ExitCode)
	}
	if _, err := os.Stat(got.Worktree); err != nil {
		t.Errorf("worktree was removed after a failure: %v; it should be left for inspection", err)
	}
}

// TestRunRejectsADuplicateTaskID: one ID owns one worktree, branch and log.
func TestRunRejectsADuplicateTaskID(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-3", "--repo", repo, "--cli", "claude", "--prompt", "first"); err != nil {
		t.Fatalf("first run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "PROJ-3", "done", "failed")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-3", "--repo", repo, "--cli", "claude", "--prompt", "second")
	if err == nil {
		t.Fatalf("second run with the same id succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("error = %q, want it to say the task already exists", out)
	}
}

// TestRunRejectsAnUnknownBaseBranch fails at launch rather than making a
// worktree from a branch that is not there.
func TestRunRejectsAnUnknownBaseBranch(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-4", "--repo", repo, "--cli", "claude", "--prompt", "x", "--base-branch", "release/9.9")
	if err == nil {
		t.Fatalf("run with a missing base branch succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "release/9.9") {
		t.Errorf("error = %q, want it to name the missing base branch", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "PROJ-4")); !os.IsNotExist(statErr) {
		t.Error("a worktree was created for a task that never launched")
	}
}

// TestRunRejectsAMissingCLIBinary fails at launch when the agent's own CLI is
// not installed, rather than leaving a worktree and branch behind for a task
// that could never have run.
func TestRunRejectsAMissingCLIBinary(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()

	out, err := orcRunWithPath(t, home, systemPath(t, "git"), "run",
		"--id", "PROJ-5", "--repo", repo, "--prompt", "x", "--cli", "claude")
	if err == nil {
		t.Fatalf("run without the CLI installed succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("error = %q, want it to name the missing CLI", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "PROJ-5")); !os.IsNotExist(statErr) {
		t.Error("a worktree was created for a task that never launched")
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "PROJ-5.json")); !os.IsNotExist(statErr) {
		t.Error("a state file was written for a task that never launched")
	}
	if branches := strings.TrimSpace(git(t, repo, "branch", "--list", "agent-orc/proj-5")); branches != "" {
		t.Errorf("branch %q was created for a task that never launched", branches)
	}
}

// TestRunRequiresTheCLIFlag refuses to guess which agentic CLI is installed.
func TestRunRequiresTheCLIFlag(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	out, err := orcRun(t, home, stub, "run", "--id", "PROJ-6", "--repo", repo, "--prompt", "x")
	if err == nil {
		t.Fatalf("run without --cli succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "--cli") {
		t.Errorf("error = %q, want it to name the missing flag", out)
	}
}

// TestRunRefusesAnUnreadableStateFile stops rather than overwriting a record it
// cannot parse, which would throw away whatever that task was tracking.
func TestRunRefusesAnUnreadableStateFile(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	const corruptJSON = `{"status": "run`
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(home, "state", "PROJ-7.json")
	write(t, corrupt, corruptJSON)

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-7", "--repo", repo, "--cli", "claude", "--prompt", "x")
	if err == nil {
		t.Fatalf("run over an unreadable state file succeeded, want an error\n%s", out)
	}
	if got := readFile(t, corrupt); got != corruptJSON {
		t.Errorf("the unreadable state file was overwritten: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "PROJ-7")); !os.IsNotExist(statErr) {
		t.Error("a worktree was created for a task that never launched")
	}
}

// TestRunCleansUpWhenTheStateWriteFails checks the rollback is complete: a
// launch that dies after the worktree exists must leave neither the worktree
// nor the branch, so the same id can simply be retried.
func TestRunCleansUpWhenTheStateWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions would not block the write")
	}
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	// A read-only state directory lets the layout check pass and the worktree
	// be created, then fails the state write, which is the path under test.
	stateDir := filepath.Join(home, "state")
	if err := os.MkdirAll(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-8", "--repo", repo, "--cli", "claude", "--prompt", "x")
	if err == nil {
		t.Fatalf("run with an unwritable state directory succeeded, want an error\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "PROJ-8")); !os.IsNotExist(statErr) {
		t.Error("the worktree was left behind after a failed launch")
	}
	if b := strings.TrimSpace(git(t, repo, "branch", "--list", "agent-orc/proj-8")); b != "" {
		t.Errorf("branch %q was left behind after a failed launch", b)
	}
}

// TestSuperviseRecordsAnAgentThatCannotStart covers the supervisor's own
// failure path. Run checks the CLI is on PATH before making anything, so this
// is reached when the binary goes away between dispatch and launch: the task
// has a record and a worktree, and nothing to run in it.
func TestSuperviseRecordsAnAgentThatCannotStart(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "NOSTART-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "NOSTART-1", "done", "failed")

	// Put the task back to pending and supervise it again with nothing on
	// PATH, which is what a CLI uninstalled mid-flight looks like.
	statePath := filepath.Join(home, "state", "NOSTART-1.json")
	var rec map[string]any
	if err := json.Unmarshal([]byte(readFile(t, statePath)), &rec); err != nil {
		t.Fatal(err)
	}
	rec["status"] = "pending"
	rec["pid"] = 0
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	write(t, statePath, string(data))

	out, err := orcRunWithPath(t, home, t.TempDir(), "supervise", "NOSTART-1")
	if err == nil {
		t.Fatalf("agent-orc supervise = nil, want the missing binary reported\n%s", out)
	}
	got := loadRecord(t, home, "NOSTART-1")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "starting claude") {
		t.Errorf("error = %q, want it to name the binary that would not start", got.Error)
	}
	if got.PID != 0 {
		t.Errorf("pid = %d, want none recorded for an agent that never ran", got.PID)
	}
}
