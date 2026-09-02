//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reviewRecord is the on-disk state with the Phase 4 fields.
type reviewRecord struct {
	record
	SessionID   string `json:"session_id"`
	ReviewRound int    `json:"review_round"`
}

func loadReviewRecord(t *testing.T, home, id string) reviewRecord {
	t.Helper()
	var got reviewRecord
	data := readFile(t, filepath.Join(home, "state", id+".json"))
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("parsing state for %s: %v\n%s", id, err, data)
	}
	return got
}

// workerCommit is a stub worker that does some work and commits it. It appends
// rather than overwrites so a resumed session produces a further commit, the
// way a worker addressing review comments would.
const workerCommit = `printf 'work\n' >> out.txt
git add .
git diff --cached --quiet || git commit --no-gpg-sign -m 'feat: do the thing' >/dev/null`

// runWorker dispatches a task with review enabled and waits for it to finish.
func runWorker(t *testing.T, home, stub, repo, id string, extra ...string) reviewRecord {
	t.Helper()
	args := append([]string{"run", "--id", id, "--repo", repo, "--cli", "claude",
		"--prompt", "do the thing", "--no-auto-pr", "--review", "--review-cli", "copilot"}, extra...)
	if out, err := orcRun(t, home, stub, args...); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, id, "done", "failed")
	return loadReviewRecord(t, home, id)
}

// TestRunRecordsAResumableSessionID checks that a session ID is assigned at
// launch and passed to the CLI, which is what makes the feedback loop possible.
func TestRunRecordsAResumableSessionID(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, workerCommit)

	got := runWorker(t, home, stub, repo, "SESS-1")
	if got.SessionID == "" {
		t.Fatal("no session_id was recorded; the worker could not be resumed")
	}
	if r := readFile(t, receipt); !strings.Contains(r, "arg=--session-id") || !strings.Contains(r, "arg="+got.SessionID) {
		t.Errorf("the session id was not passed to the CLI:\n%s", r)
	}
}

// TestReviewApprovesAndStops covers the short path in §14: LGTM, no loop.
func TestReviewApprovesAndStops(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewerReceipt := filepath.Join(t.TempDir(), "reviewer")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), workerCommit)
	stubInto(t, stub, "copilot", reviewerReceipt, `printf 'LGTM\n'`)

	rec := runWorker(t, home, stub, repo, "REV-1")

	out, err := orcRun(t, home, stub, "review", "REV-1")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("review output = %q, want the approval reported", out)
	}

	got := loadReviewRecord(t, home, "REV-1")
	if got.Status != "reviewed" {
		t.Errorf("status = %q, want reviewed", got.Status)
	}
	if got.ReviewRound != 1 {
		t.Errorf("review_round = %d, want 1", got.ReviewRound)
	}

	// The reviewer saw the diff and the task in a clean session, with no resume.
	r := readFile(t, reviewerReceipt)
	for _, want := range []string{"do the thing", "git diff main...HEAD", "LGTM"} {
		if !strings.Contains(r, want) {
			t.Errorf("reviewer prompt is missing %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "arg=--resume") {
		t.Error("the reviewer resumed a session; it must review in a clean one")
	}

	// The reviewer's worktree is disposable and must not survive the round.
	if _, err := os.Stat(filepath.Join(home, "reviews", "REV-1")); !os.IsNotExist(err) {
		t.Error("the reviewer's worktree was left behind")
	}
	// The worker's worktree is not.
	if _, err := os.Stat(rec.Worktree); err != nil {
		t.Errorf("the worker's worktree was removed: %v", err)
	}
}

// TestReviewLoopsFeedbackBackToTheWorker covers the long path: comments are
// handed to the original session, and the cap stops the loop.
func TestReviewLoopsFeedbackBackToTheWorker(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	workerReceipt := filepath.Join(t.TempDir(), "worker")

	stub := stubAgent(t, "claude", workerReceipt, workerCommit)
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"),
		`printf -- '- handler.go: the retry never backs off\n- add a test for the timeout path\n'`)

	rec := runWorker(t, home, stub, repo, "REV-2")

	out, err := orcRun(t, home, stub, "review", "REV-2")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	for _, want := range []string{"2 comment(s)", "the retry never backs off", "worker addressed"} {
		if !strings.Contains(out, want) {
			t.Errorf("review output is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "cap of 1 round(s) reached") {
		t.Errorf("review output = %q, want the cap to stop the loop", out)
	}

	// The worker was resumed in its own session, not a fresh one, and given
	// the reviewer's comments.
	w := readFile(t, workerReceipt)
	if !strings.Contains(w, "arg=--resume") || !strings.Contains(w, "arg="+rec.SessionID) {
		t.Errorf("the worker was not resumed by session id:\n%s", w)
	}
	if !strings.Contains(w, "the retry never backs off") {
		t.Errorf("the worker was not given the reviewer's comments:\n%s", w)
	}

	// The resumed worker's follow-up commit landed on the same branch.
	if n := strings.Count(git(t, repo, "log", "--oneline", "main..agent-orc/rev-2"), "\n"); n != 2 {
		t.Errorf("branch has %d commits, want 2 (the original plus the follow-up)", n)
	}

	got := loadReviewRecord(t, home, "REV-2")
	if got.ReviewRound != 1 {
		t.Errorf("review_round = %d, want 1", got.ReviewRound)
	}
	if got.Status == "reviewed" {
		t.Error("status = reviewed, want it left unreviewed when comments were raised")
	}
}

// TestReviewStopsOnAnUnreadableVerdict covers §15: no guessing either way.
func TestReviewStopsOnAnUnreadableVerdict(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"),
		`printf 'I had a look and it seems fine to me.\n'`)

	runWorker(t, home, stub, repo, "REV-3")

	out, err := orcRun(t, home, stub, "review", "REV-3")
	if err == nil {
		t.Fatalf("review with an unreadable verdict succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "neither LGTM nor a list of comments") {
		t.Errorf("error = %q, want it to say the response could not be read", out)
	}
	if got := loadReviewRecord(t, home, "REV-3"); got.ReviewRound != 0 {
		t.Errorf("review_round = %d, want 0; an unread verdict is not a completed round", got.ReviewRound)
	}
}

// TestReviewRefusesATaskThatDidNotEnableIt keeps review opt-in.
func TestReviewRefusesATaskThatDidNotEnableIt(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "REV-4", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "REV-4", "done", "failed")

	out, err := orcRun(t, home, stub, "review", "REV-4")
	if err == nil {
		t.Fatalf("review on a task without it enabled succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "does not have review enabled") {
		t.Errorf("error = %q, want it to say review is not enabled", out)
	}
}

// TestReviewHonoursAHigherCap checks that max_rounds actually raises the cap.
func TestReviewHonoursAHigherCap(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewerReceipt := filepath.Join(t.TempDir(), "reviewer")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "copilot", reviewerReceipt, `printf -- '- keep going\n'`)

	runWorker(t, home, stub, repo, "REV-5", "--review-max-rounds", "2")

	out, err := orcRun(t, home, stub, "review", "REV-5")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	if !strings.Contains(out, "round 2 of 2") {
		t.Errorf("review output = %q, want two rounds to run", out)
	}
	if got := loadReviewRecord(t, home, "REV-5"); got.ReviewRound != 2 {
		t.Errorf("review_round = %d, want 2", got.ReviewRound)
	}

	// Two rounds means two reviewer invocations, each in a fresh session.
	if n := strings.Count(readFile(t, reviewerReceipt), "arg=-p"); n != 2 {
		t.Errorf("the reviewer ran %d time(s), want 2", n)
	}
}

// TestReviewRefusesAWorkerThatCannotBeResumed surfaces the limitation instead
// of silently starting the worker over from scratch.
func TestReviewRefusesAWorkerThatCannotBeResumed(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "codex", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "claude", filepath.Join(t.TempDir(), "reviewer"), `printf -- '- fix it\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "REV-6", "--repo", repo, "--prompt", "do it", "--cli", "codex",
		"--no-auto-pr", "--review", "--review-cli", "claude"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "REV-6", "done", "failed")

	out, err := orcRun(t, home, stub, "review", "REV-6")
	if err == nil {
		t.Fatalf("review of a codex task succeeded, want an error\n%s", out)
	}
	if !strings.Contains(out, "cannot be resumed") {
		t.Errorf("error = %q, want it to explain codex cannot be resumed", out)
	}
}
