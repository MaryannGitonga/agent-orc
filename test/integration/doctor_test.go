//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loginStub installs a claude whose `auth status` answers loggedIn, counting
// each time it is asked in count, and whose agent run does nothing.
func loginStub(t *testing.T, loggedIn bool, count string) string {
	t.Helper()
	dir := t.TempDir()
	answer := "false"
	if loggedIn {
		answer = "true"
	}
	script := "#!/usr/bin/env bash\n" +
		"if [ \"${1:-} ${2:-}\" = 'auth status' ]; then echo x >> '" + count + "'; echo '{\"loggedIn\": " + answer + "}'; exit 0; fi\n" +
		"exit 0\n"
	write(t, filepath.Join(dir, "claude"), script)
	if err := os.Chmod(filepath.Join(dir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunRefusesALoggedOutCLIBeforeMakingAnything covers the point of the
// checks: a task that cannot run is stopped before a worktree, a branch or a
// record exists, let alone a paid agent run.
func TestRunRefusesALoggedOutCLIBeforeMakingAnything(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	stub := loginStub(t, false, filepath.Join(t.TempDir(), "count"))

	out, err := orcRun(t, home, stub, "run",
		"--id", "OUT-1", "--repo", repo, "--cli", "claude", "--prompt", "do it", "--no-auto-pr")
	if err == nil || !strings.Contains(out, "claude logged in: not logged in") {
		t.Fatalf("agent-orc run = %v, want it refused for the login\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "OUT-1.json")); !os.IsNotExist(err) {
		t.Error("a record was created for a task that was refused")
	}
	if strings.Contains(git(t, repo, "branch", "--list"), "out-1") {
		t.Error("a branch was created for a task that was refused")
	}
}

func TestABatchChecksTheLoginOnce(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	count := filepath.Join(t.TempDir(), "count")
	stub := loginStub(t, true, count)
	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, "repo: "+repo+"\ndefaults:\n  cli: claude\n  auto_pr: false\ntasks:\n"+
		"  - id: B-1\n    prompt: one\n  - id: B-2\n    prompt: two\n  - id: B-3\n    prompt: three\n")

	if out, err := orcRun(t, home, stub, "run", batch); err != nil {
		t.Fatalf("agent-orc run batch = %v\n%s", err, out)
	}
	for _, id := range []string{"B-1", "B-2", "B-3"} {
		waitForStatus(t, home, id, "done", "failed")
	}
	if asked := strings.Count(readFile(t, count), "x"); asked != 1 {
		t.Errorf("claude auth status asked %d times for 3 tasks, want once", asked)
	}
}

func TestDoctorReportsEveryCheck(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "cli: claude\ntest_command: no-such-program-xyz\n")

	out, err := orcRun(t, home, loginStub(t, true, filepath.Join(t.TempDir(), "count")), "doctor", "--repo", repo)
	if err == nil {
		t.Fatalf("agent-orc doctor = nil, want it to fail over the test command\n%s", out)
	}
	for _, want := range []string{"ok    claude installed", "ok    claude logged in", "FAIL  test command", "1 check(s) failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output is missing %q:\n%s", want, out)
		}
	}

	write(t, filepath.Join(repo, ".agent-orc.yaml"), "cli: claude\ntest_command: true\n")
	if out, err := orcRun(t, home, loginStub(t, true, filepath.Join(t.TempDir(), "count")), "doctor", "--repo", repo); err != nil {
		t.Errorf("agent-orc doctor = %v, want it to pass\n%s", err, out)
	}
}
