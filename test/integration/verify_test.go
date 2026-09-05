//go:build integration

package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyBlocksAPublishOnRedTests covers the gate that makes "iterate until
// the tests pass" mean something: the agent committed, so the branch looks
// finished, and only running the suite says otherwise.
func TestVerifyBlocksAPublishOnRedTests(t *testing.T) {
	repo, _ := initRepoWithRemote(t)
	home := t.TempDir()
	// The agent commits work that does not pass, and never fixes it.
	stub := stubAgent(t, "codex", filepath.Join(t.TempDir(), "receipt"),
		"printf 'broken\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: exit 1\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "VER-1", "--repo", repo, "--cli", "codex", "--prompt", "do it")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "VER-1", "done", "failed", "publishing", "publish_failed")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed on a red suite", got.Status)
	}
	if !strings.Contains(got.Error, "test command did not pass") {
		t.Errorf("error = %q, want it to name the failing suite", got.Error)
	}
	if got.PRURL != "" {
		t.Errorf("a PR was opened over a red branch: %s", got.PRURL)
	}
	// Codex cannot be resumed, so there is nothing to hand the failure back to
	// and the suite is run exactly once rather than looped on.
	if got.TestRuns != 1 {
		t.Errorf("test_runs = %d, want 1 for a CLI that cannot be resumed", got.TestRuns)
	}
}

// TestVerifyDiscoversTheRepositorysOwnTests covers the zero-config path: a
// repository states how it is tested in its own build files, and nobody should
// have to repeat that in a flag.
func TestVerifyDiscoversTheRepositorysOwnTests(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	// A go module, so the discovered command is one this machine can run.
	write(t, filepath.Join(repo, "go.mod"), "module example.com/x\n\ngo 1.22\n")
	write(t, filepath.Join(repo, "x.go"), "package x\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--no-gpg-sign", "-m", "chore: module")

	stub := stubAgent(t, "claude", receipt, "true")
	out, err := orcRun(t, home, stub, "run",
		"--id", "DISC-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "go test ./... (from go.mod)") {
		t.Errorf("run = %q, want the discovered command reported", out)
	}
	got := waitForStatus(t, home, "DISC-1", "done", "failed")
	if got.Status != "done" || got.TestRuns != 1 {
		t.Errorf("status/test_runs = %q/%d, want done after one discovered run", got.Status, got.TestRuns)
	}
	// And the agent was told the same command agent-orc was about to run.
	if r := readFile(t, receipt); !strings.Contains(r, "run `go test ./...`") {
		t.Errorf("the prompt did not name the discovered command:\n%s", r)
	}
}

// TestVerifyNoneOptsOut is how a repository whose tests agent-orc should not be
// running says so, without giving up the rest of the config.
func TestVerifyNoneOptsOut(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	write(t, filepath.Join(repo, "go.mod"), "module example.com/x\n\ngo 1.22\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--no-gpg-sign", "-m", "chore: module")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: none\n")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	out, err := orcRun(t, home, stub, "run",
		"--id", "DISC-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "not run for this task") {
		t.Errorf("run = %q, want it to say the tests are not run", out)
	}
	if got := waitForStatus(t, home, "DISC-2", "done", "failed"); got.TestRuns != 0 {
		t.Errorf("test_runs = %d, want the suite left alone", got.TestRuns)
	}
}

// TestVerifyHandsFailuresBackToTheAgent is the iteration itself: a red suite
// goes back to the agent's own session, and the branch publishes once green.
func TestVerifyHandsFailuresBackToTheAgent(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	// First run leaves the marker absent, so the suite fails. The resumed run
	// creates it, so the second attempt passes.
	stub := stubAgent(t, "claude", receipt,
		"if [ -f resumed ]; then printf 'ok\\n' > passing; fi\n"+
			"git add -A && git commit --no-gpg-sign -m 'feat: work' >/dev/null 2>&1 || true\n"+
			"touch resumed")

	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: test -f passing\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "VER-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "VER-2", "done", "failed")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done once the agent fixed the suite\n%s",
			got.Status, readFile(t, filepath.Join(home, "logs", "VER-2.supervisor.log")))
	}
	if got.TestRuns != 2 || got.TestsPassed == nil || !*got.TestsPassed {
		t.Errorf("test_runs/passed = %d/%v, want 2 runs ending green", got.TestRuns, got.TestsPassed)
	}
	// The agent was resumed with the failing output, not started afresh.
	r := readFile(t, receipt)
	if !strings.Contains(r, "arg=--resume") {
		t.Errorf("the agent was not resumed to fix the tests:\n%s", r)
	}
	if !strings.Contains(r, "test command for this project failed") {
		t.Errorf("the agent was not told what failed:\n%s", r)
	}
}

// TestVerifyStopsWhenTheAgentStopsCommitting covers the only thing that ends
// an unbounded loop short of the suite passing or the budget running out: a
// round that changes nothing leaves the next run identical to the last, so
// repeating it is not another attempt at anything.
func TestVerifyStopsWhenTheAgentStopsCommitting(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	// The agent commits on its first run and never again, so the suite stays
	// red with nothing changing between attempts.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"if [ ! -f .committed ]; then\n"+
			"  touch .committed out.txt\n"+
			"  git add -A && git commit --no-gpg-sign -m 'feat: work' >/dev/null\n"+
			"fi")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: exit 1\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "STUCK-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "STUCK-1", "done", "failed")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed\n%s", got.Status,
			readFile(t, filepath.Join(home, "logs", "STUCK-1.supervisor.log")))
	}
	if !strings.Contains(got.Error, "committed nothing in its last round") {
		t.Errorf("error = %q, want it to say why looping stopped", got.Error)
	}
	// It gave up at the fixed point rather than after a fixed number of tries.
	if got.TestRuns != 1 {
		t.Errorf("test_runs = %d, want it to stop as soon as a round changed nothing", got.TestRuns)
	}
}

// TestVerifyKeepsGoingWhileTheAgentIsWorking is the other side: as long as
// each round commits something, the loop runs past any old attempt cap.
func TestVerifyKeepsGoingWhileTheAgentIsWorking(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	// Five rounds of real work before the suite goes green, which is past the
	// three attempts this used to allow.
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"n=$(cat .rounds 2>/dev/null || echo 0)\n"+
			"n=$((n + 1))\n"+
			"echo $n > .rounds\n"+
			"if [ $n -ge 5 ]; then touch passing; fi\n"+
			"git add -A && git commit --no-gpg-sign -m \"feat: round $n\" >/dev/null")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: test -f passing\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "LOOP-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "LOOP-1", "done", "failed")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done once the suite went green\n%s", got.Status,
			readFile(t, filepath.Join(home, "logs", "LOOP-1.supervisor.log")))
	}
	if got.TestRuns < 4 {
		t.Errorf("test_runs = %d, want the loop to have run past the old cap of 3", got.TestRuns)
	}
	if got.TestsPassed == nil || !*got.TestsPassed {
		t.Errorf("tests_passed = %v, want true", got.TestsPassed)
	}
}

// TestVerifyTellsTheAgentTheCommand checks the command reaches the prompt, so
// the agent can get the suite green before agent-orc ever has to.
func TestVerifyTellsTheAgentTheCommand(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")

	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: pytest -q\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "VER-3", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "VER-3", "done", "failed")
	if r := readFile(t, receipt); !strings.Contains(r, "run `pytest -q`") {
		t.Errorf("the prompt did not name the test command:\n%s", r)
	}
}

// TestAutoReviewRunsBeforeThePR covers the flag: the review happens on the way
// to the draft PR, not as a separate command afterwards.
func TestAutoReviewRunsBeforeThePR(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewer := filepath.Join(t.TempDir(), "reviewer")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), workerCommit)
	stubInto(t, stub, "copilot", reviewer, `printf 'LGTM\n'`)

	if out, err := orcRun(t, home, stub, "run",
		"--id", "AUTO-1", "--repo", repo, "--cli", "claude", "--prompt", "do the thing",
		"--no-auto-pr", "--auto-review", "--review-cli", "copilot"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if st := waitForStatus(t, home, "AUTO-1", "done", "failed", "review_failed"); st.Status != "done" {
		t.Fatalf("status = %q, want done\n%s", st.Status,
			readFile(t, filepath.Join(home, "logs", "AUTO-1.supervisor.log")))
	}
	got := loadReviewRecord(t, home, "AUTO-1")
	// The review ran without anyone invoking 'agent-orc review'.
	if got.ReviewRound != 1 {
		t.Errorf("review_round = %d, want the automatic round recorded", got.ReviewRound)
	}
	if r := readFile(t, reviewer); !strings.Contains(r, "git diff main...HEAD") {
		t.Errorf("the reviewer did not run:\n%s", r)
	}
}
