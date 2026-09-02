package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a repository with one commit on main and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "--no-gpg-sign", "-m", "chore: base")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestOpenReturnsTheTopLevel(t *testing.T) {
	dir := newRepo(t)
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := Open(sub)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	// macOS resolves temp dirs through a symlink, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(r.Dir)
	if got != want {
		t.Errorf("Dir = %q, want the repository top level %q", got, want)
	}
}

func TestOpenRejectsANonRepository(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("Open() outside a repository = nil, want an error")
	}
}

func TestDefaultBranchFallsBackToTheCheckedOutBranch(t *testing.T) {
	r := &Repo{Dir: newRepo(t)}
	got, err := r.DefaultBranch()
	if err != nil {
		t.Fatalf("DefaultBranch() = %v", err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch() = %q, want %q", got, "main")
	}
}

func TestDefaultBranchPrefersTheRemoteHead(t *testing.T) {
	dir := newRepo(t)
	r := &Repo{Dir: dir}
	mustGit(t, dir, "checkout", "-q", "-b", "scratch")
	mustGit(t, dir, "update-ref", "refs/remotes/origin/develop", "HEAD")
	mustGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")

	got, err := r.DefaultBranch()
	if err != nil {
		t.Fatalf("DefaultBranch() = %v", err)
	}
	if got != "develop" {
		t.Errorf("DefaultBranch() = %q, want the remote's HEAD %q", got, "develop")
	}
}

func TestDefaultBranchErrorsOnDetachedHead(t *testing.T) {
	dir := newRepo(t)
	mustGit(t, dir, "checkout", "-q", "--detach", "HEAD")

	_, err := (&Repo{Dir: dir}).DefaultBranch()
	if err == nil {
		t.Fatal("DefaultBranch() on a detached HEAD = nil, want an error")
	}
	if !strings.Contains(err.Error(), "--base-branch") {
		t.Errorf("error = %q, want it to suggest passing --base-branch", err)
	}
}

func TestRevAndBranchExistence(t *testing.T) {
	r := &Repo{Dir: newRepo(t)}
	if !r.RevExists("main") {
		t.Error("RevExists(main) = false, want true")
	}
	if r.RevExists("release/9.9") {
		t.Error("RevExists(release/9.9) = true, want false")
	}
	if !r.BranchExists("main") {
		t.Error("BranchExists(main) = false, want true")
	}
	if r.BranchExists("nope") {
		t.Error("BranchExists(nope) = true, want false")
	}
}

func TestWorktreeAddAndRemove(t *testing.T) {
	dir := newRepo(t)
	r := &Repo{Dir: dir}
	wt := filepath.Join(t.TempDir(), "task")

	if err := r.AddWorktree(wt, "feat/task", "main"); err != nil {
		t.Fatalf("AddWorktree() = %v", err)
	}
	if !r.BranchExists("feat/task") {
		t.Error("AddWorktree() did not create the branch")
	}

	// Without force, removal must refuse rather than discard uncommitted work.
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveWorktree(wt, false); err == nil {
		t.Error("RemoveWorktree() on a dirty worktree = nil, want an error")
	}
	if err := r.RemoveWorktree(wt, true); err != nil {
		t.Fatalf("RemoveWorktree(force) = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree directory still present after removal")
	}
	if !r.BranchExists("feat/task") {
		t.Error("RemoveWorktree() deleted the branch, want it kept")
	}
}

func TestAddWorktreeReportsGitStderr(t *testing.T) {
	r := &Repo{Dir: newRepo(t)}
	err := r.AddWorktree(filepath.Join(t.TempDir(), "task"), "feat/task", "release/9.9")
	if err == nil {
		t.Fatal("AddWorktree() from a missing base = nil, want an error")
	}
	if !strings.Contains(err.Error(), "release/9.9") {
		t.Errorf("error = %q, want it to name the missing base branch", err)
	}
}

func TestDeleteBranch(t *testing.T) {
	dir := newRepo(t)
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := r.AddWorktree(wt, "feat/gone", "main"); err != nil {
		t.Fatalf("AddWorktree() = %v", err)
	}
	if err := r.RemoveWorktree(wt, true); err != nil {
		t.Fatalf("RemoveWorktree() = %v", err)
	}
	if !r.BranchExists("feat/gone") {
		t.Fatal("BranchExists(feat/gone) = false before delete, want true")
	}
	if err := r.DeleteBranch("feat/gone"); err != nil {
		t.Fatalf("DeleteBranch() = %v", err)
	}
	if r.BranchExists("feat/gone") {
		t.Error("BranchExists(feat/gone) = true after delete, want false")
	}
	if err := r.DeleteBranch("never-existed"); err == nil {
		t.Error("DeleteBranch(never-existed) = nil, want an error")
	}
}
