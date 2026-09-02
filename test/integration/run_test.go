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
	ID       string `json:"id"`
	Branch   string `json:"branch"`
	Status   string `json:"status"`
	Worktree string `json:"worktree"`
	LogPath  string `json:"log_path"`
	ExitCode *int   `json:"exit_code"`
	Error    string `json:"error"`
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

// TestRunDispatchesATaskEndToEnd covers the whole Phase 0 lifecycle: a
// worktree is created on a new branch, the agent runs inside it with the
// rendered prompt, and the task is recorded as done.
func TestRunDispatchesATaskEndToEnd(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")

	// The stub commits inside the worktree, standing in for an agent that
	// does its work and commits locally.
	stub := stubAgent(t, "claude", receipt,
		"echo 'agent output' \n"+
			"printf 'fixed\\n' > fix.txt\n"+
			"git add . && git commit --no-gpg-sign -m 'fix: stub work' >/dev/null")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-1", "--repo", repo,
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

// TestRunRecordsAFailingAgent checks that a non-zero exit is surfaced rather
// than swallowed, and that the worktree is left in place for inspection.
func TestRunRecordsAFailingAgent(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "echo 'boom' >&2\nexit 3")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-2", "--repo", repo, "--prompt", "break things"); err != nil {
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

// TestRunRejectsADuplicateTaskID guards the invariant that one ID owns one
// worktree, branch, log and state file.
func TestRunRejectsADuplicateTaskID(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-3", "--repo", repo, "--prompt", "first"); err != nil {
		t.Fatalf("first run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "PROJ-3", "done", "failed")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-3", "--repo", repo, "--prompt", "second")
	if err == nil {
		t.Fatalf("second run with the same id succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("error = %q, want it to say the task already exists", out)
	}
}

// TestRunRejectsAnUnknownBaseBranch fails at launch rather than creating a
// worktree from a branch that is not there.
func TestRunRejectsAnUnknownBaseBranch(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	out, err := orcRun(t, home, stub, "run",
		"--id", "PROJ-4", "--repo", repo, "--prompt", "x", "--base-branch", "release/9.9")
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
