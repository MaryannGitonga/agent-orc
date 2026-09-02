//go:build integration

// Package integration holds tests that shell out to real external tools
// (git, gh). They are excluded from the default build via the `integration`
// tag and run with `make test-integration`.
package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir and fails the test if it errors.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=agent-orc test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=agent-orc test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		// os.DevNull rather than a literal, so the isolation works wherever
		// the tests are run from.
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestGitWorktreeRoundTrip exercises the primitive the whole orchestrator is
// built on: adding a worktree on a new branch, working in it independently of
// the parent checkout, then removing it cleanly.
func TestGitWorktreeRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	git(t, repo, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--no-gpg-sign", "-m", "chore: base")

	wt := filepath.Join(t.TempDir(), "task-1")
	git(t, repo, "worktree", "add", wt, "-b", "feat/task-1", "main")

	// The worktree is a real, independent checkout on its own branch.
	if got := strings.TrimSpace(git(t, wt, "rev-parse", "--abbrev-ref", "HEAD")); got != "feat/task-1" {
		t.Errorf("worktree branch = %q, want %q", got, "feat/task-1")
	}

	// Work done in the worktree must not leak into the parent checkout.
	if err := os.WriteFile(filepath.Join(wt, "task.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
