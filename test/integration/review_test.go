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

// workerCommitsTwice commits on its first run and its first resume, then stops.
// The review loop has no round cap, so a worker that commits every time it is
// handed comments loops forever against a reviewer that always has some. This
// stub does real work for one round and then reaches the fixed point that ends
// the loop, which is what tests about the feedback path need.
const workerCommitsTwice = `n=$(cat .rounds 2>/dev/null || echo 0)
n=$((n + 1))
echo $n > .rounds
if [ $n -le 2 ]; then
  printf 'work %s\n' "$n" >> out.txt
  git add .
  git commit --no-gpg-sign -m "feat: round $n" >/dev/null
fi`

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

// runWorkerWithCLI dispatches a task under the named *worker* CLI, with the
// other of the two as its reviewer. The reviewer is what most callers actually
// care about, so read it as picking that one by elimination: pass "copilot" to
// get a claude reviewer.
func runWorkerWithCLI(t *testing.T, home, stub, repo, id, workerCLI string) reviewRecord {
	t.Helper()
	reviewer := "claude"
	if workerCLI == "claude" {
		reviewer = "copilot"
	}
	if out, err := orcRun(t, home, stub, "run", "--id", id, "--repo", repo, "--cli", workerCLI,
		"--prompt", "do the thing", "--no-auto-pr", "--review", "--review-cli", reviewer); err != nil {
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

// TestReviewReadsAVerdictOutOfAJSONEnvelope covers a reviewer whose CLI wraps
// its answer instead of printing it. Every other review test stubs a CLI that
// echoes bare prose, so the verdict is the whole of stdout; a real Claude Code
// run buries it in one field of a very large object, and reading that object
// as prose finds neither an approval nor a comment list.
func TestReviewReadsAVerdictOutOfAJSONEnvelope(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "copilot", filepath.Join(t.TempDir(), "receipt"), workerCommit)
	// The shape Claude Code writes under --output-format json, verdict included.
	stubInto(t, stub, "claude", filepath.Join(t.TempDir(), "reviewer"),
		`printf '{"type":"result","subtype":"success","is_error":false,`+
			`"num_turns":6,"total_cost_usd":0.13,"session_id":"s1","result":"LGTM"}\n'`)

	runWorkerWithCLI(t, home, stub, repo, "REVJ-1", "copilot")

	out, err := orcRun(t, home, stub, "review", "REVJ-1")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("review output = %q, want the approval read out of the envelope", out)
	}
	got := loadReviewRecord(t, home, "REVJ-1")
	if got.Status != "reviewed" || got.ReviewRound != 1 {
		t.Errorf("status/round = %q/%d, want reviewed/1", got.Status, got.ReviewRound)
	}
}

// TestReviewReadsCommentsOutOfAJSONEnvelope is the other half: a wrapped
// verdict that is a comment list has to reach the worker, newlines intact.
func TestReviewReadsCommentsOutOfAJSONEnvelope(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	workerReceipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "copilot", workerReceipt, workerCommitsTwice)
	stubInto(t, stub, "claude", filepath.Join(t.TempDir(), "reviewer"),
		`printf '{"type":"result","subtype":"success","result":"- greeter.py: farewell() has no test\\n- README.md: document the new function"}\n'`)

	runWorkerWithCLI(t, home, stub, repo, "REVJ-2", "copilot")

	out, err := orcRun(t, home, stub, "review", "REVJ-2")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	if !strings.Contains(out, "raised 2 comment(s)") {
		t.Errorf("review output = %q, want both comments read out of the envelope", out)
	}
	// And they were handed back to the worker, not just printed.
	if r := readFile(t, workerReceipt); !strings.Contains(r, "farewell() has no test") {
		t.Errorf("the worker was not given the comments:\n%s", r)
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
// handed to the original session, and the loop ends when the worker stops
// acting on them.
func TestReviewLoopsFeedbackBackToTheWorker(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	workerReceipt := filepath.Join(t.TempDir(), "worker")

	stub := stubAgent(t, "claude", workerReceipt, workerCommitsTwice)
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
	// The worker committed nothing in response, so the reviewer would read the
	// same branch and raise the same comments; there is nothing left to try.
	if !strings.Contains(out, "committed nothing in response") {
		t.Errorf("review output = %q, want the loop to stop at the fixed point", out)
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

	// Two rounds: the first the worker acted on, the second it did not, which
	// is what stopped the loop.
	got := loadReviewRecord(t, home, "REV-2")
	if got.ReviewRound != 2 {
		t.Errorf("review_round = %d, want 2", got.ReviewRound)
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

// TestReviewApprovalClearsAnEarlierFailure covers a retry after an automatic
// review that could not be read. The automatic round leaves the task
// review_failed with the reason; a manual round then approves, and the record
// has to stop saying the first thing happened.
func TestReviewApprovalClearsAnEarlierFailure(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	verdict := filepath.Join(t.TempDir(), "verdict")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommitsTwice)
	// Unreadable the first time it is asked, an approval the second.
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"),
		"if [ -f '"+verdict+"' ]; then printf 'LGTM\n'; else touch '"+verdict+"'; printf 'no idea\n'; fi")

	if out, err := orcRun(t, home, stub, "run", "--id", "RETRY-1", "--repo", repo,
		"--cli", "claude", "--prompt", "do the thing", "--no-auto-pr",
		"--auto-review", "--review-cli", "copilot"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if got := waitForStatus(t, home, "RETRY-1", "review_failed", "done", "failed"); got.Status != "review_failed" {
		t.Fatalf("status = %q, want review_failed from the unreadable verdict", got.Status)
	}
	if got := loadReviewRecord(t, home, "RETRY-1"); got.Error == "" {
		t.Fatal("the automatic round recorded no reason, so there is nothing to clear")
	}

	if out, err := orcRun(t, home, stub, "review", "RETRY-1"); err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	got := loadReviewRecord(t, home, "RETRY-1")
	if got.Status != "reviewed" {
		t.Errorf("status = %q, want reviewed", got.Status)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want the approved task to carry none", got.Error)
	}
}

// TestReviewLoopsUntilApproved is what replaced the round cap: as long as the
// worker keeps acting on the comments, the loop keeps going, and it ends on the
// approval rather than on a number.
func TestReviewLoopsUntilApproved(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewerReceipt := filepath.Join(t.TempDir(), "reviewer")
	counter := filepath.Join(t.TempDir(), "rounds")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	// Three rounds of comments before the reviewer is satisfied, which is past
	// anything the old default cap of one would have allowed.
	stubInto(t, stub, "copilot", reviewerReceipt,
		"n=$(cat '"+counter+"' 2>/dev/null || echo 0)\n"+
			"n=$((n + 1))\n"+
			"echo $n > '"+counter+"'\n"+
			"if [ $n -ge 3 ]; then printf 'LGTM\\n'; else printf -- '- keep going\\n'; fi")

	runWorker(t, home, stub, repo, "REV-5")

	out, err := orcRun(t, home, stub, "review", "REV-5")
	if err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}
	if !strings.Contains(out, "round 3") || !strings.Contains(out, "approved") {
		t.Errorf("review output = %q, want three rounds ending in an approval", out)
	}
	got := loadReviewRecord(t, home, "REV-5")
	if got.ReviewRound != 3 {
		t.Errorf("review_round = %d, want 3", got.ReviewRound)
	}
	if got.Status != "reviewed" {
		t.Errorf("status = %q, want reviewed", got.Status)
	}
	if n := strings.Count(readFile(t, reviewerReceipt), "arg=-p"); n != 3 {
		t.Errorf("the reviewer ran %d time(s), want 3", n)
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

// TestReviewRecoversFromAKilledPreviousRound covers the state a review left
// behind when it was killed: the checkout is gone but git still has a record of
// it. Clearing only the directory makes every later review fail with "missing
// but already registered", so the run must prune that record first.
func TestReviewRecoversFromAKilledPreviousRound(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"), `printf 'LGTM\n'`)

	runWorker(t, home, stub, repo, "KILLED-1")

	// Exactly what a killed review leaves: registered with git, gone from disk.
	stale := filepath.Join(home, "reviews", "KILLED-1")
	git(t, repo, "worktree", "add", "--detach", stale, "agent-orc/killed-1")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	if out := git(t, repo, "worktree", "list"); !strings.Contains(out, "KILLED-1") {
		t.Fatalf("the stale record was not created, so this is not the case under test:\n%s", out)
	}

	out, err := orcRun(t, home, stub, "review", "KILLED-1")
	if err != nil {
		t.Fatalf("review after a killed round = %v\n%s", err, out)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("review output = %q, want the approval reported", out)
	}
}

// TestReviewerIsNotToldToCommit covers the contradiction end to end: the
// reviewer prompt forbids editing, so the worker operating rules must not be
// appended to it. They tell the agent to commit, and the reviewer is sitting in
// a checkout of the branch under review.
func TestReviewerIsNotToldToCommit(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewerReceipt := filepath.Join(t.TempDir(), "reviewer")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "copilot", reviewerReceipt, `printf 'LGTM\n'`)

	runWorker(t, home, stub, repo, "RAW-1")
	if out, err := orcRun(t, home, stub, "review", "RAW-1"); err != nil {
		t.Fatalf("agent-orc review = %v\n%s", err, out)
	}

	got := readFile(t, reviewerReceipt)
	for _, rule := range []string{"Commit your work locally", "Operating rules for this run"} {
		if strings.Contains(got, rule) {
			t.Errorf("the reviewer was given the worker rule %q:\n%s", rule, got)
		}
	}
	// It still received its own instructions.
	if !strings.Contains(got, "LGTM") {
		t.Errorf("the reviewer prompt lost its own instructions:\n%s", got)
	}
}

// TestReviewRefusesWhilePublishing keeps a review off a moving target: the
// publish chain is still rewriting and pushing the branch.
func TestReviewRefusesWhilePublishing(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "worker"), workerCommit)
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"), `printf 'LGTM\n'`)

	rec := runWorker(t, home, stub, repo, "PUB-REV-1")
	// Put the record back into the publishing state the chain runs in.
	statePath := filepath.Join(home, "state", "PUB-REV-1.json")
	raw := readFile(t, statePath)
	write(t, statePath, strings.Replace(raw, `"status": "`+rec.Status+`"`, `"status": "publishing"`, 1))

	out, err := orcRun(t, home, stub, "review", "PUB-REV-1")
	if err == nil {
		t.Fatalf("review during publishing succeeded, want a refusal\n%s", out)
	}
	if !strings.Contains(out, "publishing") {
		t.Errorf("error = %q, want it to name the publishing status", out)
	}
}
