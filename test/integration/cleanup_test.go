//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitWork is a stub agent that leaves one commit on its branch.
const commitWork = "printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null"

// finishedTask dispatches id with a stub claude running body and waits for it
// to finish.
func finishedTask(t *testing.T, home, repo, id, body string) record {
	t.Helper()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), body)
	if out, err := orcRun(t, home, stub, "run",
		"--id", id, "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	return waitForStatus(t, home, id, "done", "failed")
}

// setPushedSHA records sha as what agent-orc last pushed for id, which is what
// publishing writes once the branch is on the remote.
func setPushedSHA(t *testing.T, home, id, sha string) {
	t.Helper()
	path := filepath.Join(home, "state", id+".json")
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &raw); err != nil {
		t.Fatal(err)
	}
	raw["pushed_sha"] = sha
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data))
}

// TestCleanupDeleteBranchTrustsAPushedBranch covers the other way a branch is
// safe to delete: its commits are not in the base, but they are on the remote,
// so the local copy holds nothing that would be lost. That only holds while
// the branch is where it was pushed.
func TestCleanupDeleteBranchTrustsAPushedBranch(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	finishedTask(t, home, repo, "PUSH-1", commitWork)
	branch := "agent-orc/push-1"

	// Pushed, but the branch has moved on since: the newer commit is nowhere
	// else.
	setPushedSHA(t, home, "PUSH-1", strings.TrimSpace(git(t, repo, "rev-parse", "main")))
	out, err := orcRun(t, home, t.TempDir(), "cleanup", "PUSH-1", "--delete-branch")
	if err == nil || !strings.Contains(out, "were not pushed") {
		t.Fatalf("cleanup --delete-branch = %v, want it to refuse a branch that moved since its push\n%s", err, out)
	}

	setPushedSHA(t, home, "PUSH-1", strings.TrimSpace(git(t, repo, "rev-parse", branch)))
	if out, err := orcRun(t, home, t.TempDir(), "cleanup", "PUSH-1", "--delete-branch"); err != nil {
		t.Fatalf("cleanup --delete-branch = %v, want a branch that is on the remote to go\n%s", err, out)
	}
	if strings.Contains(git(t, repo, "branch", "--list", branch), "push-1") {
		t.Error("the pushed branch is still there")
	}
}

// TestCleanupNamesUncommittedWorkAsTheObstacle covers the one refusal from git
// that --force is the answer to. Saying so is what tells the user that forcing
// discards something, rather than just getting past an error.
func TestCleanupNamesUncommittedWorkAsTheObstacle(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	rec := finishedTask(t, home, repo, "DIRTY-1", "true")
	write(t, filepath.Join(rec.Worktree, "notes.txt"), "not committed\n")

	out, err := orcRun(t, home, t.TempDir(), "cleanup", "DIRTY-1")
	if err == nil {
		t.Fatalf("cleanup = nil, want it to refuse a worktree with uncommitted changes\n%s", out)
	}
	if !strings.Contains(out, "uncommitted changes; pass --force to discard them") {
		t.Errorf("cleanup = %q, want it to say what is in the way and what --force does", out)
	}
	if _, statErr := os.Stat(filepath.Join(rec.Worktree, "notes.txt")); statErr != nil {
		t.Errorf("the uncommitted file is gone after a refusal: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "DIRTY-1.json")); statErr != nil {
		t.Errorf("the state file is gone after a refusal: %v", statErr)
	}
}

// TestCleanupRefusesARunningTask covers cleanup not tearing a worktree out
// from under an agent that is still working in it.
func TestCleanupRefusesARunningTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "sleep 30")
	if out, err := orcRun(t, home, stub, "run",
		"--id", "BUSY-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	rec := waitForStatus(t, home, "BUSY-1", "running")
	t.Cleanup(func() { _, _ = orcRun(t, home, stub, "stop", "BUSY-1") })

	out, err := orcRun(t, home, stub, "cleanup", "BUSY-1")
	if err == nil || !strings.Contains(out, "stop it first or pass --force") {
		t.Fatalf("cleanup = %v, want it to refuse a running task\n%s", err, out)
	}
	if _, statErr := os.Stat(rec.Worktree); statErr != nil {
		t.Errorf("the running task's worktree is gone: %v", statErr)
	}
}

// TestCleanupAllCarriesOnPastAFailure covers one task that cannot be cleaned
// not stopping the rest, while still failing the command and naming it.
func TestCleanupAllCarriesOnPastAFailure(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	rec := finishedTask(t, home, repo, "SOME-1", "true")
	finishedTask(t, home, repo, "SOME-2", "true")
	write(t, filepath.Join(rec.Worktree, "notes.txt"), "not committed\n")

	out, err := orcRun(t, home, t.TempDir(), "cleanup", "--all")
	if err == nil {
		t.Fatalf("cleanup --all = nil, want it to fail when a task was not cleaned\n%s", out)
	}
	if !strings.Contains(out, "SOME-1  not cleaned") {
		t.Errorf("cleanup --all = %q, want it to name the task it could not clean", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "SOME-2.json")); !os.IsNotExist(statErr) {
		t.Error("SOME-2 was not cleaned up after SOME-1 failed")
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "SOME-1.json")); statErr != nil {
		t.Errorf("SOME-1's state is gone although it was not cleaned: %v", statErr)
	}
}

func TestCleanupAllWithNothingToClean(t *testing.T) {
	out, err := orcRun(t, t.TempDir(), t.TempDir(), "cleanup", "--all")
	if err != nil {
		t.Fatalf("cleanup --all = %v\n%s", err, out)
	}
	if !strings.Contains(out, "no tasks to clean up") {
		t.Errorf("cleanup --all = %q, want it to say there was nothing to do", out)
	}
}
