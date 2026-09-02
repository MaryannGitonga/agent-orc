package sanitize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoWith creates a repository whose branch carries the given commit
// messages, and returns its path.
func repoWith(t *testing.T, messages ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate the child git processes from the developer's own config, so
	// these tests never reach for a signing key or a global hook.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "A Dev")
	t.Setenv("GIT_AUTHOR_EMAIL", "dev@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "A Dev")
	t.Setenv("GIT_COMMITTER_EMAIL", "dev@example.com")

	dir := t.TempDir()
	run(t, dir, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "--no-gpg-sign", "-m", "chore: base")
	run(t, dir, "checkout", "-q", "-b", "feat/x")

	for i, msg := range messages {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, dir, "add", ".")
		run(t, dir, "commit", "--no-gpg-sign", "-m", msg)
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=A Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=A Dev", "GIT_COMMITTER_EMAIL=dev@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

const dirty = "feat: do the thing\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"

func TestAmendHeadStripsAttribution(t *testing.T) {
	dir := repoWith(t, dirty)
	t.Setenv(EnvPatterns, strings.Join(DefaultPatterns, "\n"))
	t.Setenv(EnvSignOff, "")

	if err := AmendHead(dir); err != nil {
		t.Fatalf("AmendHead() = %v", err)
	}
	got := run(t, dir, "log", "-1", "--format=%B")
	if strings.Contains(got, "Claude") {
		t.Errorf("message = %q, want the attribution stripped", got)
	}
	if !strings.Contains(got, "feat: do the thing") {
		t.Errorf("message = %q, want the subject kept", got)
	}
}

func TestAmendHeadAddsASignOff(t *testing.T) {
	dir := repoWith(t, "feat: do the thing\n")
	t.Setenv(EnvPatterns, strings.Join(DefaultPatterns, "\n"))
	t.Setenv(EnvSignOff, "A Dev <dev@example.com>")

	if err := AmendHead(dir); err != nil {
		t.Fatalf("AmendHead() = %v", err)
	}
	if got := run(t, dir, "log", "-1", "--format=%B"); !strings.Contains(got, "Signed-off-by: A Dev <dev@example.com>") {
		t.Errorf("message = %q, want a sign-off", got)
	}
}

func TestAmendHeadLeavesACleanCommitUntouched(t *testing.T) {
	dir := repoWith(t, "feat: do the thing\n")
	t.Setenv(EnvPatterns, strings.Join(DefaultPatterns, "\n"))
	t.Setenv(EnvSignOff, "")

	before := strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
	if err := AmendHead(dir); err != nil {
		t.Fatalf("AmendHead() = %v", err)
	}
	if after := strings.TrimSpace(run(t, dir, "rev-parse", "HEAD")); after != before {
		t.Errorf("HEAD changed from %s to %s, want a clean commit left alone", before, after)
	}
}

func TestAmendHeadRejectsABadSignOff(t *testing.T) {
	dir := repoWith(t, "feat: x\n")
	t.Setenv(EnvPatterns, "")
	t.Setenv(EnvSignOff, "not an identity")

	if err := AmendHead(dir); err == nil {
		t.Error("AmendHead() with a malformed identity = nil, want an error")
	}
}

func TestRewriterReadsOnlyTheBranchesOwnCommits(t *testing.T) {
	dir := repoWith(t, "feat: first\n", "fix: second\n")
	r := Rewriter{Worktree: dir, Base: "main"}

	got, err := r.messages()
	if err != nil {
		t.Fatalf("messages() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("messages() returned %d, want the 2 commits ahead of main", len(got))
	}
	// Oldest first, so a rewrite replays them in order.
	if !strings.HasPrefix(got[0], "feat: first") {
		t.Errorf("messages()[0] = %q, want the oldest commit first", got[0])
	}
	for _, m := range got {
		if strings.Contains(m, "chore: base") {
			t.Error("messages() included a commit from the base branch")
		}
	}
}

func TestRewriterSkipsABranchThatIsAlreadyClean(t *testing.T) {
	dir := repoWith(t, "feat: first\n")
	policy, err := NewPolicy(DefaultPatterns, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))

	n, err := (Rewriter{Worktree: dir, Base: "main", Policy: policy, Patterns: DefaultPatterns, Self: "/nonexistent"}).Run()
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if n != 0 {
		t.Errorf("Run() rewrote %d commits, want 0", n)
	}
	// No rebase ran, so the SHAs are untouched, which is why Self could be a
	// path that does not exist.
	if after := strings.TrimSpace(run(t, dir, "rev-parse", "HEAD")); after != before {
		t.Errorf("HEAD changed from %s to %s, want an already-clean branch left alone", before, after)
	}
}

func TestRewriterOnABranchWithNoCommits(t *testing.T) {
	dir := repoWith(t)
	n, err := (Rewriter{Worktree: dir, Base: "main"}).Run()
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if n != 0 {
		t.Errorf("Run() = %d, want 0", n)
	}
}
