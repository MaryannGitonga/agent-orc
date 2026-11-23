package orc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// fakeCLI puts an executable called name, running body, first on PATH.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestClaudeLoginFollowsItsStatus(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	fakeCLI(t, "claude", `echo '{"loggedIn": false}'`)
	if got := checkLogin(task.CLIClaude); got.Level != LevelFail || !strings.Contains(got.Detail, "claude auth login") {
		t.Errorf("logged out = %+v, want a failure saying how to log in", got)
	}
	fakeCLI(t, "claude", `echo '{"loggedIn": true}'`)
	if got := checkLogin(task.CLIClaude); got.Level != LevelOK {
		t.Errorf("logged in = %+v, want ok", got)
	}
}

func TestClaudeLoginAcceptsAnAPIKey(t *testing.T) {
	fakeCLI(t, "claude", `echo '{"loggedIn": false}'`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if got := checkLogin(task.CLIClaude); got.Level != LevelOK {
		t.Errorf("with an API key = %+v, want ok", got)
	}
}

func TestCodexLoginCountsOnlyAClearNo(t *testing.T) {
	for body, want := range map[string]Level{
		"echo 'Logged in using ChatGPT'": LevelOK,
		"echo 'Not logged in'":           LevelFail,
		"echo 'Not logged in'; exit 1":   LevelFail,
		"echo 'something else'; exit 2":  LevelFail,
		"echo 'Logged in using API key'": LevelOK,
	} {
		fakeCLI(t, "codex", body)
		if got := checkLogin(task.CLICodex); got.Level != want {
			t.Errorf("%s: level = %s, want %s", body, got.Level, want)
		}
	}
}

// TestCopilotLoginNeverRunsCopilot covers looking for Copilot's credentials
// without starting it: running it can cost money.
func TestCopilotLoginNeverRunsCopilot(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	fakeCLI(t, "copilot", "touch '"+marker+"'")
	for _, v := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(v, "")
	}
	if got := checkLogin(task.CLICopilot); got.Level != LevelWarn {
		t.Errorf("no token = %+v, want a warning that it cannot be checked", got)
	}
	t.Setenv("GH_TOKEN", "gho_test")
	if got := checkLogin(task.CLICopilot); got.Level != LevelOK || !strings.Contains(got.Detail, "GH_TOKEN") {
		t.Errorf("with GH_TOKEN = %+v, want ok naming it", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("checking Copilot's login ran copilot")
	}
}

func TestTestCommandIsCheckedByTheShell(t *testing.T) {
	for command, want := range map[string]Level{
		"exit 1":                        LevelOK,
		"cd sub && make test":           LevelOK,
		"FOO=1 true":                    LevelOK,
		"(cd sub; make test)":           LevelOK,
		"no-such-program-xyz --run":     LevelFail,
		"FOO=1 no-such-program-xyz run": LevelFail,
	} {
		if got := checkTestCommand(command); got.Level != want {
			t.Errorf("checkTestCommand(%q) = %+v, want %s", command, got, want)
		}
	}
}

func TestABatchProbesEachThingOnce(t *testing.T) {
	var c checker
	calls := 0
	probe := func() Check { calls++; return Check{"x", LevelOK, ""} }
	c.once("login:claude", probe)
	c.once("login:claude", probe)
	c.once("login:codex", probe)
	if calls != 2 {
		t.Errorf("probes run = %d, want one per key", calls)
	}
}

func TestFailuresNameEveryRefusal(t *testing.T) {
	if failures("A", []Check{{"one", LevelOK, ""}, {"two", LevelWarn, "w"}}) != nil {
		t.Error("failures() refused over a warning")
	}
	err := failures("A", []Check{{"claude logged in", LevelFail, "not logged in"}, {"gh logged in", LevelFail, "no"}})
	if err == nil || !strings.Contains(err.Error(), "claude logged in: not logged in") ||
		!strings.Contains(err.Error(), "gh logged in: no") || !strings.Contains(err.Error(), "agent-orc doctor") {
		t.Errorf("failures() = %v, want every failure named and doctor suggested", err)
	}
}

func TestPrintChecksMarksEachLevel(t *testing.T) {
	var out bytes.Buffer
	PrintChecks(&out, []Check{{"a", LevelOK, "fine"}, {"b", LevelWarn, ""}, {"c", LevelFail, "broken"}})
	for _, want := range []string{"ok    a: fine", "warn  b", "FAIL  c: broken"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}
