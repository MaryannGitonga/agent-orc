//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestVerifyDiscoversTestsFromTheRepository covers the zero-config path: a
// repository states how it is tested in its own build files, and nobody should
// have to repeat that in a flag.
func TestVerifyDiscoversTestsFromTheRepository(t *testing.T) {
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

// TestVerifyTrimsTheConfiguredCommand covers three places that have to agree on
// whether a command is set: the record, the log line naming the phase, and the
// gate that decides whether to run anything. Deciding on a trimmed copy while
// storing the original is how a whitespace-only setting puts a task into
// verifying for a command that will never run.
func TestVerifyTrimsTheConfiguredCommand(t *testing.T) {
	t.Run("whitespace only is nothing at all", func(t *testing.T) {
		repo := initRepo(t)
		home := t.TempDir()
		stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
		write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: \"   \"\n")

		out, err := orcRun(t, home, stub, "run",
			"--id", "WS-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
		if err != nil {
			t.Fatalf("agent-orc run = %v\n%s", err, out)
		}
		got := waitForStatus(t, home, "WS-1", "done", "failed")
		if got.TestCommand != "" {
			t.Errorf("test_command = %q, want nothing recorded", got.TestCommand)
		}
		if log := readFile(t, filepath.Join(home, "logs", "WS-1.supervisor.log")); strings.Contains(log, "task is verifying") {
			t.Errorf("the task entered verifying for a command that never runs:\n%s", log)
		}
		if got.TestRuns != 0 {
			t.Errorf("test_runs = %d, want nothing run", got.TestRuns)
		}
	})

	t.Run("a real command keeps no stray whitespace", func(t *testing.T) {
		repo := initRepo(t)
		home := t.TempDir()
		stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
		write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: \"true   \"\n")

		if out, err := orcRun(t, home, stub, "run",
			"--id", "WS-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
			t.Fatalf("agent-orc run = %v\n%s", err, out)
		}
		got := waitForStatus(t, home, "WS-2", "done", "failed")
		if got.TestCommand != "true" {
			t.Errorf("test_command = %q, want it stored trimmed", got.TestCommand)
		}
		if got.TestRuns != 1 {
			t.Errorf("test_runs = %d, want the command to have run once", got.TestRuns)
		}
	})
}

// TestVerifyDiscoversFromTheRepositoryRoot covers where discovery looks. The
// markers it reads sit at the top of the working tree, so a --repo pointing
// into the tree has to be resolved to that top or a repository with tests reads
// as having none.
func TestVerifyDiscoversFromTheRepositoryRoot(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(repo, "go.mod"), "module example.com/x\n\ngo 1.22\n")
	write(t, filepath.Join(repo, "x.go"), "package x\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--no-gpg-sign", "-m", "chore: module")

	deep := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := orcRun(t, home, stub, "run",
		"--id", "ROOT-1", "--repo", deep, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "go test ./... (from go.mod)") {
		t.Errorf("run = %q, want the root's marker found from a subdirectory", out)
	}
	got := waitForStatus(t, home, "ROOT-1", "done", "failed")
	if got.TestRuns != 1 {
		t.Errorf("test_runs = %d, want the discovered command to have run", got.TestRuns)
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

	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")
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
	// And the agent was not asked to run them either: a suite somebody turned
	// off is not one to spend the task's budget hunting for.
	if r := readFile(t, receipt); strings.Contains(r, "test suite") {
		t.Errorf("an opted-out task still told the agent to run tests:\n%s", r)
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

// TestVerifyTimesOutAHangingTestCommand covers the one failure the loop's own
// stop conditions cannot see. A command that never returns never passes, never
// fails, and never hands the agent anything to act on, so without a cap the
// task waits on it forever.
func TestVerifyTimesOutAHangingTestCommand(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child.pid")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(repo, ".agent-orc.yaml"),
		"test_command: 'sleep 300 & echo $! > "+marker+"; sleep 300'\ntest_timeout: 2s\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "TMO-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	// The cap is reported before it applies, not only in the log afterwards.
	if !strings.Contains(out, "killed after 2s") {
		t.Errorf("run = %q, want the timeout reported at dispatch", out)
	}

	got := waitForStatus(t, home, "TMO-1", "failed", "done")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed\n%s", got.Status,
			readFile(t, filepath.Join(home, "logs", "TMO-1.supervisor.log")))
	}
	if !strings.Contains(got.Error, "did not finish") {
		t.Errorf("error = %q, want it to say the command was killed rather than that it failed", got.Error)
	}

	// The whole group went, not just the command that was waited on.
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, marker)))
	if err != nil {
		t.Fatalf("parsing the child pid: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("the timed-out command's child (pid %d) was left running", pid)
}

// TestVerifyRunsUncappedWhenAsked covers the way out for a suite that really
// does take longer than the default: the cap is off, not merely larger.
func TestVerifyRunsUncappedWhenAsked(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: 'true'\ntest_timeout: none\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "TMO-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if !strings.Contains(out, "uncapped") {
		t.Errorf("run = %q, want the absent cap reported", out)
	}
	if got := waitForStatus(t, home, "TMO-2", "done", "failed"); got.Status != "done" {
		t.Errorf("status = %q, want done", got.Status)
	}
}

// TestVerifyRejectsABadTimeout keeps an unreadable cap from reaching a run.
func TestVerifyRejectsABadTimeout(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_timeout: soon\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "TMO-3", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err == nil {
		t.Fatalf("agent-orc run = nil, want a refusal\n%s", out)
	}
	if !strings.Contains(out, "test_timeout") {
		t.Errorf("run = %q, want it to name the bad setting", out)
	}
}

// TestBatchAutoReviewRunsWithoutEnabled covers the invariant end to end. A
// batch that asks for review on finishing has asked for review, and before the
// two were tied together such a file dispatched a task that quietly never got
// reviewed at all.
func TestBatchAutoReviewRunsWithoutEnabled(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	reviewer := filepath.Join(t.TempDir(), "reviewer")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")
	stubInto(t, stub, "copilot", reviewer, `printf 'LGTM\n'`)

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	// auto, and deliberately no enabled.
	write(t, batch, "repo: "+repo+"\ndefaults:\n  cli: claude\n  auto_pr: false\n  review:\n    auto: true\n    cli: copilot\ntasks:\n  - id: BAUTO-1\n    prompt: do it\n")

	if out, err := orcRun(t, home, stub, "run", batch); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if got := waitForStatus(t, home, "BAUTO-1", "done", "failed", "review_failed"); got.Status != "done" {
		t.Fatalf("status = %q, want done\n%s", got.Status,
			readFile(t, filepath.Join(home, "logs", "BAUTO-1.supervisor.log")))
	}
	if r := readFile(t, reviewer); !strings.Contains(r, "git diff main...HEAD") {
		t.Errorf("the reviewer never ran, so auto did not imply enabled:\n%s", r)
	}
}

// TestStopReachesAHangingTestCommand covers the phases that run after the agent
// has exited. Both loops run until they succeed, so a test command that never
// returns would hang the task forever; before the child was given a process
// group and a recorded pid there was nothing for `stop` to signal, and it
// refused outright because the status carried no process.
func TestStopReachesAHangingTestCommand(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "grandchild.pid")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	// The test command hangs, and spawns a child of its own that has to go with
	// it: signalling only the leader would leave the child running.
	write(t, filepath.Join(repo, ".agent-orc.yaml"),
		"test_command: 'sleep 300 & echo $! > "+marker+"; sleep 300'\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "HANG-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "HANG-1", "verifying")
	if got.PID == 0 {
		t.Fatal("no pid recorded while the test command runs, so stop has nothing to signal")
	}
	grandchild := strings.TrimSpace(readFile(t, marker))
	if grandchild == "" {
		t.Fatal("the test command did not report its child")
	}

	out, err := orcRun(t, home, stub, "stop", "HANG-1")
	if err != nil {
		t.Fatalf("agent-orc stop = %v\n%s", err, out)
	}
	if got := waitForStatus(t, home, "HANG-1", "stopped"); got.PID != 0 {
		t.Errorf("pid = %d, want it cleared once the task is stopped", got.PID)
	}

	// The whole group went, not just the leader.
	pid, err := strconv.Atoi(grandchild)
	if err != nil {
		t.Fatalf("parsing the child pid %q: %v", grandchild, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("the test command's own child (pid %d) survived the stop", pid)
}

// TestStopDuringVerificationLeavesTheBranchUnpublished covers the end-to-end
// promise of stopping a task in its verification phase: the record says stopped,
// no pull request is opened, and nothing reaches the remote.
//
// It does not reach the narrower window between the suite passing and the
// publish starting. Any stop issued while the test command runs kills that
// command, since the command is the tracked child, so verification gives up
// first and never gets as far as publishing. See the check in Supervisor.finish.
func TestStopDuringVerificationLeavesTheBranchUnpublished(t *testing.T) {
	repo, remote := initRepoWithRemote(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"),
		"printf 'work\\n' > out.txt\ngit add . && git commit --no-gpg-sign -m 'feat: work' >/dev/null")
	stubInto(t, stub, "gh", filepath.Join(t.TempDir(), "gh"), `printf 'https://example.com/pr/1\n'`)
	// The suite stops the task and then hangs, so the stop is what ends it.
	write(t, filepath.Join(repo, ".agent-orc.yaml"),
		"test_command: '"+buildBinary(t)+" stop GAP-1 >/dev/null 2>&1; sleep 60'\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "GAP-1", "--repo", repo, "--cli", "claude", "--prompt", "do it"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}

	got := waitForStatus(t, home, "GAP-1", "stopped", "done", "publish_failed", "failed")
	if got.Status != "stopped" {
		t.Fatalf("status = %q, want the stop to have stuck\n%s", got.Status,
			readFile(t, filepath.Join(home, "logs", "GAP-1.supervisor.log")))
	}
	// Give the supervisor time to publish if it were going to.
	time.Sleep(2 * time.Second)
	final := loadRecord(t, home, "GAP-1")
	if final.Status != "stopped" || final.PRURL != "" {
		t.Errorf("status/pr = %q/%q, want a stopped task left unpublished", final.Status, final.PRURL)
	}
	if pushed := git(t, remote, "branch", "--list", "agent-orc/gap-1"); strings.Contains(pushed, "gap-1") {
		t.Error("a stopped task was pushed to the remote")
	}
}

// TestStopDoesNotRestartAStoppedTask checks the loop notices the stop. A killed
// test command exits non-zero, which on its own reads as a failing suite and
// would send the task round again against the agent it just stopped.
func TestStopDoesNotRestartAStoppedTask(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt")
	stub := stubAgent(t, "claude", receipt, "true")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "test_command: sleep 300\n")

	if out, err := orcRun(t, home, stub, "run",
		"--id", "HANG-2", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	waitForStatus(t, home, "HANG-2", "verifying")
	if out, err := orcRun(t, home, stub, "stop", "HANG-2"); err != nil {
		t.Fatalf("agent-orc stop = %v\n%s", err, out)
	}
	waitForStatus(t, home, "HANG-2", "stopped")

	// Give the supervisor time to do the wrong thing if it is going to.
	time.Sleep(2 * time.Second)
	got := loadRecord(t, home, "HANG-2")
	if got.Status != "stopped" {
		t.Errorf("status = %q, want a stopped task to stay stopped", got.Status)
	}
	if n := strings.Count(readFile(t, receipt), "arg=--resume"); n != 0 {
		t.Errorf("the agent was resumed %d time(s) after the stop; the loop should have ended", n)
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
