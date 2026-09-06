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

// fullRecord is the on-disk state, with the Phase 3 fields.
type fullRecord struct {
	record
	PRURL            string `json:"pr_url"`
	RewrittenCommits int    `json:"rewritten_commits"`
}

func loadRecord(t *testing.T, home, id string) fullRecord {
	t.Helper()
	var got fullRecord
	data := readFile(t, filepath.Join(home, "state", id+".json"))
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("parsing state for %s: %v\n%s", id, err, data)
	}
	return got
}

// dirtyCommit is a stub agent that commits with exactly the trailers the
// sanitization pass exists to remove.
const dirtyCommit = `printf 'work\n' > out.txt
git add .
git commit --no-gpg-sign -F - <<'MSG' >/dev/null
feat: do the thing

Co-Authored-By: Claude <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_abc
MSG`

// TestPublishSanitizesThenPushesAndOpensADraft covers §12 and §13 end to end:
// attribution trailers are gone from the pushed branch, and the draft is
// opened only after the rewrite.
func TestPublishSanitizesThenPushesAndOpensADraft(t *testing.T) {
	repo, remote := initRepoWithRemote(t)
	home := t.TempDir()
	ghReceipt := filepath.Join(t.TempDir(), "gh")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)
	stubInto(t, stub, "gh", ghReceipt, `printf 'https://github.com/org/repo/pull/7\n'`)

	out, err := orcRun(t, home, stub, "run", "--id", "PUB-1", "--repo", repo, "--cli", "claude", "--prompt", "do it")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "PUB-1", "done", "failed", "publish_failed", "policy_violation")

	got := loadRecord(t, home, "PUB-1")
	if got.Status != "done" {
		t.Fatalf("status = %q (%s)\n%s", got.Status, got.Error,
			readFile(t, filepath.Join(home, "logs", "PUB-1.supervisor.log")))
	}
	if got.RewrittenCommits != 1 {
		t.Errorf("rewritten_commits = %d, want 1", got.RewrittenCommits)
	}

	// The branch that actually reached the remote is the sanitized one.
	pushed := git(t, remote, "log", "--format=%B", "main..agent-orc/pub-1")
	for _, unwanted := range []string{"Co-Authored-By", "Claude-Session"} {
		if strings.Contains(pushed, unwanted) {
			t.Errorf("the pushed branch still carries %q:\n%s", unwanted, pushed)
		}
	}
	if !strings.Contains(pushed, "feat: do the thing") {
		t.Errorf("the pushed branch lost its subject:\n%s", pushed)
	}

	// The draft was opened, with the right flags, and recorded.
	gh := readFile(t, ghReceipt)
	for _, want := range []string{"arg=pr", "arg=create", "arg=--draft", "arg=main", "arg=agent-orc/pub-1"} {
		if !strings.Contains(gh, want) {
			t.Errorf("gh receipt is missing %q:\n%s", want, gh)
		}
	}
	if got.PRURL != "https://github.com/org/repo/pull/7" {
		t.Errorf("pr_url = %q, want the URL gh printed", got.PRURL)
	}
}

// TestPublishAddsDCOSignOff covers the second half of §13's one pass.
func TestPublishAddsDCOSignOff(t *testing.T) {
	repo, remote := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/1\n'`)

	out, err := orcRun(t, home, stub, "run",
		"--id", "DCO-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--dco-signoff")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "DCO-1", "done", "failed", "publish_failed")

	pushed := git(t, remote, "log", "--format=%B", "main..agent-orc/dco-1")
	if !strings.Contains(pushed, "Signed-off-by:") {
		t.Errorf("the pushed branch has no sign-off:\n%s", pushed)
	}
	if strings.Contains(pushed, "Co-Authored-By") {
		t.Errorf("the pushed branch still carries attribution:\n%s", pushed)
	}
}

// TestNoAutoPRLeavesTheBranchLocal checks the opt-out, and that the manual
// command does the same three steps afterwards.
func TestNoAutoPRLeavesTheBranchLocal(t *testing.T) {
	repo, remote := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/2\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "NOPR-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "NOPR-1", "done", "failed")

	if refs := git(t, remote, "branch", "--list", "agent-orc/nopr-1"); strings.TrimSpace(refs) != "" {
		t.Errorf("the branch was pushed despite --no-auto-pr: %q", refs)
	}

	out, err := orcRun(t, home, stub, "pr", "NOPR-1")
	if err != nil {
		t.Fatalf("agent-orc pr = %v\n%s", err, out)
	}
	if !strings.Contains(git(t, remote, "branch", "--list", "agent-orc/nopr-1"), "agent-orc/nopr-1") {
		t.Error("agent-orc pr did not push the branch")
	}
	if got := loadRecord(t, home, "NOPR-1"); got.PRURL == "" {
		t.Error("agent-orc pr did not record the PR URL")
	}
}

// TestPublishFlagsAnAgentThatPushedItself covers §15's policy_violation: a
// branch already on the remote never went through sanitization.
func TestPublishFlagsAnAgentThatPushedItself(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()

	// A stub that ignores its instructions and pushes the branch itself.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		dirtyCommit+"\ngit push -q origin HEAD >/dev/null 2>&1")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/3\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "VIOL-1", "--repo", repo, "--cli", "claude", "--prompt", "do it"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}

	got := waitForStatus(t, home, "VIOL-1", "policy_violation", "done", "failed", "publish_failed")
	if got.Status != "policy_violation" {
		t.Errorf("status = %q, want policy_violation when the agent pushed its own branch", got.Status)
	}
	if !strings.Contains(got.Error, "pushed") {
		t.Errorf("error = %q, want it to explain what the agent did", got.Error)
	}
}

// TestCleanupRemovesTheWorktreeAndKeepsTheBranch covers §11's cleanup.
func TestCleanupRemovesTheWorktreeAndKeepsTheBranch(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "CLEAN-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	rec := waitForStatus(t, home, "CLEAN-1", "done", "failed")

	out, err := orcRun(t, home, stub, "cleanup", "CLEAN-1")
	if err != nil {
		t.Fatalf("agent-orc cleanup = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(rec.Worktree); !os.IsNotExist(statErr) {
		t.Error("the worktree is still present after cleanup")
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "CLEAN-1.json")); !os.IsNotExist(statErr) {
		t.Error("the state file survived cleanup")
	}
	// The work itself is not what cleanup throws away.
	if !strings.Contains(git(t, repo, "log", "--oneline", "agent-orc/clean-1"), "feat: work") {
		t.Error("cleanup destroyed the task's branch")
	}
}

// TestCleanupDeleteBranchFreesTheID covers the loop a reused id used to get
// stuck in: cleanup leaves the branch behind on purpose, and the next run with
// the same id then collides with it.
func TestCleanupDeleteBranchFreesTheID(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	run := func(id string) (string, error) {
		return orcRun(t, home, stub, "run",
			"--id", id, "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	}
	if out, err := run("REUSE-1"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "REUSE-1", "done", "failed")

	// Plain cleanup keeps the branch, so the id is still taken.
	if out, err := orcRun(t, home, stub, "cleanup", "REUSE-1"); err != nil {
		t.Fatalf("agent-orc cleanup = %v\n%s", err, out)
	}
	out, err := run("REUSE-1")
	if err == nil {
		t.Fatalf("agent-orc run = nil, want a refusal while the branch is still there\n%s", out)
	}
	// The refusal has to name a remedy that works. By this point the state
	// file is gone, so cleanup no longer knows the branch and cannot be it.
	if !strings.Contains(out, "branch -D -- agent-orc/reuse-1") {
		t.Errorf("run = %q, want it to name the command that clears the branch", out)
	}

	// With the branch deleted the id is free again.
	if out, err := orcRun(t, home, stub, "cleanup", "REUSE-1"); err == nil {
		t.Fatalf("agent-orc cleanup = nil, want it to refuse a task it no longer tracks\n%s", out)
	}
	git(t, repo, "branch", "-D", "agent-orc/reuse-1")
	if out, err := run("REUSE-1"); err != nil {
		t.Fatalf("agent-orc run after deleting the branch = %v\n%s", err, out)
	}
	waitForStatus(t, home, "REUSE-1", "done", "failed")

	// And the one-step form does both at once.
	if out, err := orcRun(t, home, stub, "cleanup", "REUSE-1", "--delete-branch"); err != nil {
		t.Fatalf("agent-orc cleanup --delete-branch = %v\n%s", err, out)
	} else if !strings.Contains(out, "deleted") {
		t.Errorf("cleanup = %q, want it to say the branch was deleted", out)
	}
	if strings.Contains(git(t, repo, "branch", "--list", "agent-orc/reuse-1"), "reuse-1") {
		t.Error("the branch survived --delete-branch")
	}
	if out, err := run("REUSE-1"); err != nil {
		t.Fatalf("agent-orc run after --delete-branch = %v\n%s", err, out)
	}
	waitForStatus(t, home, "REUSE-1", "done", "failed")
}

// TestCleanupDeleteBranchJudgesAgainstTheBase covers a branch that added
// nothing being deleted even when the main checkout is somewhere else.
//
// git's own safe delete asks whether a branch is merged into the current HEAD,
// which is the wrong question for a task branch: cut from a base that has
// diverged from whatever the repository is sitting on, a branch with no
// commits of its own is refused, and the only way past that refusal would be
// --force, which throws away branches that do hold work.
func TestCleanupDeleteBranchJudgesAgainstTheBase(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	// A base branch that has diverged from main, and a checkout left on main.
	git(t, repo, "checkout", "-q", "-b", "develop")
	git(t, repo, "commit", "--no-gpg-sign", "--allow-empty", "-m", "chore: develop only")
	git(t, repo, "checkout", "-q", "main")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "BASE-1", "--repo", repo, "--cli", "claude", "--prompt", "do it",
		"--base-branch", "develop", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "BASE-1", "done", "failed")

	// git branch -d would refuse this, since develop is not reachable from main.
	if out, err := orcRun(t, home, stub, "cleanup", "BASE-1", "--delete-branch"); err != nil {
		t.Fatalf("cleanup --delete-branch = %v\n%s", err, out)
	}
	if strings.Contains(git(t, repo, "branch", "--list", "agent-orc/base-1"), "base-1") {
		t.Error("a branch with no commits of its own survived --delete-branch")
	}
}

// TestCleanupRefusesAnUnreadableWorktree covers the difference between a
// worktree that is gone and one that cannot be looked at. Only the first means
// there is nothing left to remove; treating the second the same way deletes the
// record, and with --delete-branch the branch, while leaving a checkout on disk
// that nothing points at any more.
func TestCleanupRefusesAnUnreadableWorktree(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "STAT-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	rec := waitForStatus(t, home, "STAT-1", "done", "failed")

	// Make the worktree unstattable by closing its parent to searches, which is
	// the shape a permission or mount problem takes.
	parent := filepath.Dir(rec.Worktree)
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("closing %s: %v", parent, err)
	}
	defer func() { _ = os.Chmod(parent, 0o755) }()
	if _, err := os.Stat(rec.Worktree); err == nil || os.IsNotExist(err) {
		t.Skip("stat still succeeds here, so this cannot be exercised (running as root?)")
	}

	out, err := orcRun(t, home, stub, "cleanup", "STAT-1", "--delete-branch")
	if err == nil {
		t.Fatalf("cleanup = nil, want it to refuse a worktree it cannot inspect\n%s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("cleanup = %q, want it to name the way past", out)
	}
	// Nothing was half-done: the record and the branch both survive.
	if _, statErr := os.Stat(filepath.Join(home, "state", "STAT-1.json")); statErr != nil {
		t.Error("the state file was removed despite the refusal")
	}
	if !strings.Contains(git(t, repo, "branch", "--list", "agent-orc/stat-1"), "stat-1") {
		t.Error("the branch was deleted despite the refusal")
	}

	// --force is the way past, and says what it left behind.
	out, err = orcRun(t, home, stub, "cleanup", "STAT-1", "--force")
	if err != nil {
		t.Fatalf("cleanup --force = %v\n%s", err, out)
	}
	if !strings.Contains(out, "could not be inspected") {
		t.Errorf("cleanup --force = %q, want it to say the worktree was left in place", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "STAT-1.json")); !os.IsNotExist(statErr) {
		t.Error("the state file survived a forced cleanup")
	}
}

// TestCleanupDeleteBranchWhenTheBranchIsAlreadyGone covers cleanup finishing
// on a branch someone removed by hand, or one a previous cleanup deleted before
// failing to remove the state. A missing ref is not unpushed work, and treating
// it as such would refuse the second attempt and keep the id taken.
func TestCleanupDeleteBranchWhenTheBranchIsAlreadyGone(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "GONE-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	rec := waitForStatus(t, home, "GONE-1", "done", "failed")

	// Remove the worktree and the branch behind agent-orc's back.
	git(t, repo, "worktree", "remove", "--force", rec.Worktree)
	git(t, repo, "branch", "-D", "agent-orc/gone-1")

	out, err := orcRun(t, home, stub, "cleanup", "GONE-1", "--delete-branch")
	if err != nil {
		t.Fatalf("cleanup --delete-branch = %v, want an absent branch to be no obstacle\n%s", err, out)
	}
	if !strings.Contains(out, "already gone") {
		t.Errorf("cleanup = %q, want it to say the branch was already gone", out)
	}
	if _, statErr := os.Stat(filepath.Join(home, "state", "GONE-1.json")); !os.IsNotExist(statErr) {
		t.Error("the state file survived, so the id is still taken")
	}
}

// TestLogsLeaveProseCLIsAlone covers the summary being scoped to the CLIs that
// actually wrap their answer. A CLI that reports in prose may print JSON of its
// own, and rewriting that as though it were a result envelope would change the
// agent's output in the one place that records what it did.
func TestLogsLeaveProseCLIsAlone(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	// A prose CLI that prints a JSON object carrying a "result" key, which is
	// exactly the shape the summary looks for.
	const line = `{"result":"do not summarize me","subtype":"success"}`
	stub := stubAgent(t, "copilot", filepath.Join(t.TempDir(), "receipt"),
		"cat <<'JSON'\n"+line+"\nJSON")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PROSE-1", "--repo", repo, "--cli", "copilot", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "PROSE-1", "done", "failed")

	out, err := orcRun(t, home, stub, "logs", "PROSE-1")
	if err != nil {
		t.Fatalf("agent-orc logs = %v\n%s", err, out)
	}
	if !strings.Contains(out, line) {
		t.Errorf("logs = %q, want a prose CLI's output byte for byte", out)
	}
	if strings.Contains(out, "status  ") {
		t.Errorf("logs = %q, want no summary for a CLI that does not wrap its answer", out)
	}
}

// TestCleanupDeleteBranchKeepsUnmergedWork checks the guard on the flag: a
// branch holding commits that are nowhere else is work, and only --force says
// to throw it away.
func TestCleanupDeleteBranchKeepsUnmergedWork(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "KEEP-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "KEEP-1", "done", "failed")

	out, err := orcRun(t, home, stub, "cleanup", "KEEP-1", "--delete-branch")
	if err == nil {
		t.Fatalf("cleanup --delete-branch = nil, want it to refuse unmerged work\n%s", out)
	}
	if !strings.Contains(out, "were not pushed") {
		t.Errorf("cleanup = %q, want it to say why the branch is not safe to delete", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("cleanup = %q, want it to name --force", out)
	}
	if !strings.Contains(git(t, repo, "log", "--oneline", "agent-orc/keep-1"), "feat: work") {
		t.Fatal("the branch was deleted despite the refusal")
	}
	// The state has to survive the refusal too, or the task becomes
	// untrackable and the branch unreachable by name.
	if out, err := orcRun(t, home, stub, "cleanup", "KEEP-1", "--delete-branch", "--force"); err != nil {
		t.Fatalf("cleanup --delete-branch --force = %v\n%s", err, out)
	}
	if strings.Contains(git(t, repo, "branch", "--list", "agent-orc/keep-1"), "keep-1") {
		t.Error("--force did not delete the branch")
	}
}

// TestRunClearsAPreviousTaskLog covers a reused id starting clean: the log
// paths come from the id alone, so without this the first thing 'agent-orc
// logs' shows is the output of a task that was cleaned up.
func TestRunClearsAPreviousTaskLog(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	dir := t.TempDir()
	stubInto(t, dir, "claude", filepath.Join(t.TempDir(), "receipt"), "echo first run")

	run := func() (string, error) {
		return orcRun(t, home, dir, "run",
			"--id", "FRESH-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	}
	if out, err := run(); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "FRESH-1", "done", "failed")
	if out, err := orcRun(t, home, dir, "cleanup", "FRESH-1", "--delete-branch"); err != nil {
		t.Fatalf("agent-orc cleanup = %v\n%s", err, out)
	}

	stubInto(t, dir, "claude", filepath.Join(t.TempDir(), "receipt"), "echo second run")
	if out, err := run(); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "FRESH-1", "done", "failed")

	out, err := orcRun(t, home, dir, "logs", "FRESH-1")
	if err != nil {
		t.Fatalf("agent-orc logs = %v\n%s", err, out)
	}
	if strings.Contains(out, "first run") {
		t.Errorf("logs = %q, want the previous task's output gone", out)
	}
	if !strings.Contains(out, "second run") {
		t.Errorf("logs = %q, want the current task's output", out)
	}
}

// TestLogsPrintsTheAgentOutput covers §11's logs command.
func TestLogsPrintsTheAgentOutput(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "echo 'hello from the agent'")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "LOG-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "LOG-1", "done", "failed")

	out, err := orcRun(t, home, stub, "logs", "LOG-1")
	if err != nil {
		t.Fatalf("agent-orc logs = %v\n%s", err, out)
	}
	if !strings.Contains(out, "hello from the agent") {
		t.Errorf("logs = %q, want the agent's output", out)
	}
}

// TestLogsSummarizesAJSONResult covers the other half of the logs command: a
// CLI that reports its result as JSON gets summarized by default, and --raw
// still yields the exact bytes the agent wrote.
func TestLogsSummarizesAJSONResult(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	result := `{"subtype":"success","num_turns":3,"duration_ms":1500,` +
		`"total_cost_usd":0.25,"session_id":"sess-42","result":"Fixed the retry handler."}`
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"cat <<'JSON'\n"+result+"\nJSON")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "LOG-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "LOG-2", "done", "failed")

	out, err := orcRun(t, home, stub, "logs", "LOG-2")
	if err != nil {
		t.Fatalf("agent-orc logs = %v\n%s", err, out)
	}
	for _, want := range []string{
		"Fixed the retry handler.", "status   success, 3 turns, 1.5s",
		"cost     $0.2500", "session  sess-42",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "total_cost_usd") {
		t.Errorf("logs = %q, want the raw json summarized away", out)
	}

	// The flag comes after the id, which is the form the usage line advertises.
	raw, err := orcRun(t, home, stub, "logs", "LOG-2", "--raw")
	if err != nil {
		t.Fatalf("agent-orc logs --raw = %v\n%s", err, raw)
	}
	if !strings.Contains(raw, result) {
		t.Errorf("logs --raw = %q, want the agent's line verbatim", raw)
	}
}

// TestPublishRefusesABranchWithNoCommits keeps agent-orc from opening an empty
// PR when the agent did nothing.
func TestPublishRefusesABranchWithNoCommits(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/4\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "EMPTY-1", "--repo", repo, "--cli", "claude", "--prompt", "do nothing", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "EMPTY-1", "done", "failed")

	out, err := orcRun(t, home, stub, "pr", "EMPTY-1")
	if err == nil {
		t.Fatalf("agent-orc pr on an empty branch succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "committed nothing") {
		t.Errorf("error = %q, want it to say the agent committed nothing", out)
	}
}

// TestBatchDCOSignoffAppliesToEveryTask checks the file-level §5 setting.
func TestBatchDCOSignoffAppliesToEveryTask(t *testing.T) {
	repo, remote := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/5\n'`)

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, fmt.Sprintf("repo: %s\ndco_signoff: true\ndefaults:\n  cli: claude\ntasks:\n  - id: BDCO-1\n    prompt: do it\n", repo))

	if out, err := orcRun(t, home, stub, "run", batch); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "BDCO-1", "done", "failed", "publish_failed")

	if pushed := git(t, remote, "log", "--format=%B", "main..agent-orc/bdco-1"); !strings.Contains(pushed, "Signed-off-by:") {
		t.Errorf("the batch's dco_signoff was not applied:\n%s", pushed)
	}
}

// TestPRRetriesAfterAPushThatSucceeded covers the recovery agent-orc advertises
// for publish_failed. If the push lands and only the draft-open fails, the
// retry must open the draft, not mistake agent-orc's own pushed branch for the
// agent having pushed it.
func TestPRRetriesAfterAPushThatSucceeded(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()
	ghReceipt := filepath.Join(t.TempDir(), "gh")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)
	// The draft-open fails while the push succeeds: the retryable case.
	stubInto(t, stub, "gh", ghReceipt, "echo 'gh: not authenticated' >&2\nexit 1")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "RETRY-1", "--repo", repo, "--cli", "claude", "--prompt", "do it"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "RETRY-1", "publish_failed")

	// The branch is on the remote now, pushed by agent-orc itself.
	if remote := strings.TrimSpace(git(t, repo, "ls-remote", "--heads", "origin", "agent-orc/retry-1")); remote == "" {
		t.Fatal("the first attempt did not push, so this is not the case under test")
	}

	// gh works this time; the retry must finish the job.
	stubInto(t, stub, "gh", ghReceipt, `printf 'https://example.com/pr/9\n'`)
	out, err := orcRun(t, home, stub, "pr", "RETRY-1")
	if err != nil {
		t.Fatalf("agent-orc pr after a failed draft-open = %v\n%s", err, out)
	}
	got := loadRecord(t, home, "RETRY-1")
	if got.Status != "done" {
		t.Errorf("status = %q, want done after a successful retry", got.Status)
	}
	if got.PRURL == "" {
		t.Error("no pr_url recorded after the retry")
	}
}

// TestSanitizeRunsWithoutARemote covers history, not publishing. A repository
// with no origin still gets its commits rewritten: keeping the attribution
// would carry exactly what the pass exists to remove, and hand it to whoever
// adds a remote later.
func TestSanitizeRunsWithoutARemote(t *testing.T) {
	repo := initRepo(t) // deliberately no remote
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), dirtyCommit)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "LOCAL-1", "--repo", repo, "--cli", "claude", "--prompt", "do it"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	// Local-only work is a legitimate way to run, so it still ends done.
	got := waitForStatus(t, home, "LOCAL-1", "done", "failed", "publish_failed")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done for a repository with no remote", got.Status)
	}

	msg := git(t, repo, "log", "-1", "--format=%B", "agent-orc/local-1")
	for _, gone := range []string{"Co-Authored-By", "Claude-Session"} {
		if strings.Contains(msg, gone) {
			t.Errorf("%q survived in a repository with no remote:\n%s", gone, msg)
		}
	}
	if !strings.Contains(msg, "feat: do the thing") {
		t.Errorf("the subject did not survive the rewrite:\n%s", msg)
	}
	if loadRecord(t, home, "LOCAL-1").RewrittenCommits != 1 {
		t.Error("the rewrite was not recorded in the task state")
	}
}

// TestTaskThatCommitsNothingSaysSo covers the honest reporting of an agent that
// did no work. It is not a failure, but the run must not claim sanitized work
// on a branch that has none.
func TestTaskThatCommitsNothingSaysSo(t *testing.T) {
	repo := initRepo(t) // no remote
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "NOOP-1", "--repo", repo, "--cli", "claude", "--prompt", "do nothing"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "NOOP-1", "done", "failed", "publish_failed")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done; committing nothing is not a failure", got.Status)
	}
	if n := strings.TrimSpace(git(t, repo, "rev-list", "--count", "main..agent-orc/noop-1")); n != "0" {
		t.Fatalf("branch has %s commits, so this is not the case under test", n)
	}

	log := readFile(t, filepath.Join(home, "logs", "NOOP-1.supervisor.log"))
	if !strings.Contains(log, "committed nothing") {
		t.Errorf("the log does not say the agent committed nothing:\n%s", log)
	}
	if strings.Contains(log, "sanitized") {
		t.Errorf("the log claims sanitized work on an empty branch:\n%s", log)
	}
}

// TestPolicyViolationBeatsAnEmptyBranch keeps the trust check ahead of the
// commit count. An agent that pushed its own branch is a policy violation even
// when it left nothing committed locally, and that is the diagnosis that
// matters: "you committed nothing" would hide an unsanitized branch already
// sitting on the remote.
func TestPolicyViolationBeatsAnEmptyBranch(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()
	// Pushes the branch without committing anything to it.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"git push -q origin HEAD:agent-orc/pv-1 2>/dev/null")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/1\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "PV-1", "--repo", repo, "--cli", "claude", "--prompt", "do it"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "PV-1", "policy_violation", "done", "failed", "publish_failed")
	if got.Status != "policy_violation" {
		t.Errorf("status = %q, want policy_violation; the agent pushed its own branch", got.Status)
	}
}

// TestEmptyBranchWithARemoteIsNotAPublishFailure covers the wording. Nothing
// was attempted, so the log must not talk about a failed publish or offer a
// retry that cannot help.
func TestEmptyBranchWithARemoteIsNotAPublishFailure(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/1\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "NOPR-1", "--repo", repo, "--cli", "claude", "--prompt", "do nothing"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	// Not done: a PR someone is waiting for will never arrive.
	got := waitForStatus(t, home, "NOPR-1", "publish_failed", "done", "failed")
	if got.Status != "publish_failed" {
		t.Fatalf("status = %q, want publish_failed", got.Status)
	}

	log := readFile(t, filepath.Join(home, "logs", "NOPR-1.supervisor.log"))
	if !strings.Contains(log, "committed nothing") {
		t.Errorf("the log does not say the agent committed nothing:\n%s", log)
	}
	if strings.Contains(log, "retry with") {
		t.Errorf("the log offers a retry that cannot help:\n%s", log)
	}
	if strings.Contains(log, "the work is committed") {
		t.Errorf("the log claims committed work on an empty branch:\n%s", log)
	}
}
