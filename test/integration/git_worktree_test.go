//go:build integration

// Package integration holds tests that shell out to real git or to a built
// agent-orc binary. The `integration` tag keeps them out of the default build.
package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitWorktreeRoundTrip exercises the primitive the orchestrator is built
// on: add a worktree on a new branch, work in it, remove it cleanly.
func TestGitWorktreeRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "task-1")
	git(t, repo, "worktree", "add", wt, "-b", "feat/task-1", "main")

	// The worktree is a real, independent checkout on its own branch.
	if got := strings.TrimSpace(git(t, wt, "rev-parse", "--abbrev-ref", "HEAD")); got != "feat/task-1" {
		t.Errorf("worktree branch = %q, want %q", got, "feat/task-1")
	}

	// Work done in the worktree must not leak into the parent checkout.
	write(t, filepath.Join(wt, "task.txt"), "work\n")
	git(t, wt, "add", ".")
	git(t, wt, "commit", "--no-gpg-sign", "-m", "feat: task work")

	if _, err := os.Stat(filepath.Join(repo, "task.txt")); !os.IsNotExist(err) {
		t.Errorf("task.txt leaked into the parent checkout, want it isolated to the worktree")
	}
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "--abbrev-ref", "HEAD")); got != "main" {
		t.Errorf("parent checkout branch = %q, want it left on %q", got, "main")
	}

	// The branch survives removal of the worktree; the directory does not.
	git(t, repo, "worktree", "remove", wt)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree directory still present after removal")
	}
	if !strings.Contains(git(t, repo, "branch", "--list", "feat/task-1"), "feat/task-1") {
		t.Errorf("branch feat/task-1 was dropped with the worktree, want it kept")
	}
}
