//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBatchDispatchesEveryTask checks the batch path end to end: defaults
// and overrides applied, each task on its own branch, in its own worktree,
// with its own CLI.
func TestRunBatchDispatchesEveryTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receiptDir := t.TempDir()

	claudeReceipt := filepath.Join(receiptDir, "claude")
	copilotReceipt := filepath.Join(receiptDir, "copilot")
	stub := stubAgent(t, "claude", claudeReceipt, "printf 'c\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: claude work' >/dev/null")
	stubInto(t, stub, "copilot", copilotReceipt, "printf 'p\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: copilot work' >/dev/null")

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, fmt.Sprintf(`repo: %s
base_branch: main

defaults:
  cli: claude
  model: sonnet-4-6

tasks:
  - id: TASK-A
    prompt: first job
    branch: feat/task-a

  - id: TASK-B
    prompt: second job
    branch: feat/task-b
    cli: copilot
    model: gpt-5.1
`, repo))

	out, err := orcRun(t, home, stub, "run", batch)
	if err != nil {
		t.Fatalf("agent-orc run %s = %v\n%s", batch, err, out)
	}
	if !strings.Contains(out, "2 of 2 tasks launched") {
		t.Errorf("output = %q, want it to report both tasks launched", out)
	}

	for id, branch := range map[string]string{"TASK-A": "feat/task-a", "TASK-B": "feat/task-b"} {
		got := waitForStatus(t, home, id, "done", "failed")
		if got.Status != "done" {
			t.Errorf("%s status = %q (error %q), want done", id, got.Status, got.Error)
		}
		if got.Branch != branch {
			t.Errorf("%s branch = %q, want %q", id, got.Branch, branch)
		}
	}

	// Each CLI was invoked, with the model the batch file gave it.
	if got := readFile(t, claudeReceipt); !strings.Contains(got, "arg=sonnet-4-6") {
		t.Errorf("claude receipt = %q, want the batch default model", got)
	}
	if got := readFile(t, copilotReceipt); !strings.Contains(got, "arg=gpt-5.1") {
		t.Errorf("copilot receipt = %q, want the per-task model override", got)
	}

	// Both agents wrote out.txt; the worktrees kept them from colliding.
	for _, branch := range []string{"feat/task-a", "feat/task-b"} {
		if !strings.Contains(git(t, repo, "log", "--oneline", branch), "work") {
			t.Errorf("branch %s has no commit from its agent", branch)
		}
	}
	if strings.Contains(git(t, repo, "log", "--oneline", "main"), "work") {
		t.Error("an agent's commit leaked onto main")
	}
}

// TestRunBatchKeepsGoingAfterOneBadTask checks that one unlaunchable task does
// not take the rest of an independent batch down with it.
func TestRunBatchKeepsGoingAfterOneBadTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, fmt.Sprintf(`repo: %s
defaults:
  cli: claude
tasks:
  - id: GOOD
    prompt: fine
  - id: BAD
    prompt: doomed
    base_branch: release/9.9
`, repo))

	out, _ := orcRun(t, home, stub, "run", batch)
	if !strings.Contains(out, "1 of 2 tasks launched") {
		t.Errorf("output = %q, want one launched and one reported as failed", out)
	}
	if !strings.Contains(out, "BAD  not launched") {
		t.Errorf("output = %q, want the bad task named", out)
	}
	waitForStatus(t, home, "GOOD", "done", "failed")
	if _, err := os.Stat(filepath.Join(home, "state", "BAD.json")); !os.IsNotExist(err) {
		t.Error("a state file was written for a task that never launched")
	}
}

// TestRunFetchesAGitHubSource checks the §6 fetch path with a stub gh on PATH,
// including that the task's own prompt is layered on rather than replaced.
func TestRunFetchesAGitHubSource(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	ghReceipt := filepath.Join(t.TempDir(), "gh")

	stub := stubAgent(t, "claude", receipt, "true")
	stubInto(t, stub, "gh", ghReceipt,
		`printf '{"title":"Retry loops forever","body":"The sync handler never gives up."}\n'`)

	out, err := orcRun(t, home, stub, "run",
		"--id", "GH-1", "--repo", repo, "--cli", "claude",
		"--source", "github://canonical/data-mesh#87",
		"--prompt", "Focus on the retry handler")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "GH-1", "done", "failed")

	// gh was asked for exactly that issue.
	gh := readFile(t, ghReceipt)
	for _, want := range []string{"arg=issue", "arg=view", "arg=87", "arg=canonical/data-mesh", "arg=title,body"} {
		if !strings.Contains(gh, want) {
			t.Errorf("gh receipt is missing %q:\n%s", want, gh)
		}
	}

	// The prompt the agent saw is the ticket plus the extra instructions.
	agent := readFile(t, receipt)
	for _, want := range []string{"Retry loops forever", "The sync handler never gives up.", "Additional instructions:", "Focus on the retry handler"} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent prompt is missing %q:\n%s", want, agent)
		}
	}
}

// TestRunFailsFastOnABadSourceFetch checks §15: a failed fetch stops the
// launch instead of guessing a prompt, and leaves nothing behind.
func TestRunFailsFastOnABadSourceFetch(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), "echo 'issue not found' >&2\nexit 1")

	out, err := orcRun(t, home, stub, "run",
		"--id", "GH-2", "--repo", repo, "--cli", "claude",
		"--source", "github://canonical/data-mesh#404")
	if err == nil {
		t.Fatalf("run with an unfetchable source succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "issue not found") {
		t.Errorf("error = %q, want the fetcher's own message", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees", "GH-2")); !os.IsNotExist(statErr) {
		t.Error("a worktree was created for a task whose source could not be fetched")
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "GH-2.json")); !os.IsNotExist(statErr) {
		t.Error("state was written for a task that never launched")
	}
}

// TestRunTreatsABlankSourceAsAbsent covers the agreement between ValidateSpec
// and the launch-time fetch: a task ValidateSpec accepts on its prompt alone
// must not then fail because its source is whitespace.
func TestRunTreatsABlankSourceAsAbsent(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")

	out, err := orcRun(t, home, stub, "run",
		"--id", "WS-1", "--repo", repo, "--cli", "claude",
		"--prompt", "do the thing", "--source", "   ")
	if err != nil {
		t.Fatalf("run with a blank source = %v\n%s", err, out)
	}
	waitForStatus(t, home, "WS-1", "done", "failed")
	if r := readFile(t, receipt); !strings.Contains(r, "do the thing") {
		t.Errorf("the agent did not get the prompt:\n%s", r)
	}
}

// TestStubReceiptPathWithSpaces guards the stub helper itself: an argv receipt
// written to a path containing a space must still be captured, so a TMPDIR with
// spaces cannot silently break every assertion built on receipts.
func TestStubReceiptPathWithSpaces(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "a dir with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, "re ceipt")
	stub := stubAgent(t, "claude", receipt, "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "SP-1", "--repo", repo, "--cli", "claude", "--prompt", "spaced"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "SP-1", "done", "failed")
	if r := readFile(t, receipt); !strings.Contains(r, "arg=") {
		t.Errorf("nothing was written to a receipt path containing spaces:\n%q", r)
	}
}
