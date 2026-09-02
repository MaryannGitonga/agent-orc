package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/version"
)

func TestDispatchVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out bytes.Buffer
		if err := dispatch([]string{arg}, &out); err != nil {
			t.Fatalf("dispatch(%q) = %v", arg, err)
		}
		if got := strings.TrimSpace(out.String()); got != version.String() {
			t.Errorf("dispatch(%q) printed %q, want %q", arg, got, version.String())
		}
	}
}

func TestDispatchWithNoArgumentsPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := dispatch(nil, &out); err != nil {
		t.Fatalf("dispatch(nil) = %v", err)
	}
	if !strings.Contains(out.String(), "agent-orc run") {
		t.Errorf("usage = %q, want it to mention the run command", out.String())
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	err := dispatch([]string{"frobnicate"}, &out)
	if err == nil {
		t.Fatal("dispatch() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error = %q, want it to name the unknown command", err)
	}
}

// TestRunHelpIsNotAnError covers the flow the usage text recommends: `run -h`
// prints the flags and exits successfully.
func TestRunHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var out bytes.Buffer
		if err := dispatch([]string{"run", arg}, &out); err != nil {
			t.Errorf("dispatch(run %s) = %v, want nil", arg, err)
		}
		if !strings.Contains(out.String(), "Usage: agent-orc run") {
			t.Errorf("run %s printed %q, want the run usage", arg, out.String())
		}
	}
}

func TestBuildTaskRequiresIDAndSomethingToDo(t *testing.T) {
	if _, err := buildTask("", "", "p", ".", "", "", "claude", ""); err == nil ||
		!strings.Contains(err.Error(), "--id") {
		t.Errorf("buildTask() without an id = %v, want an error naming --id", err)
	}
	if _, err := buildTask("X", "", "  ", ".", "", "", "claude", ""); err == nil ||
		!strings.Contains(err.Error(), "--source") {
		t.Errorf("buildTask() with neither prompt nor source = %v, want an error naming both", err)
	}
	if _, err := buildTask("X", "", "p", ".", "", "", "", ""); err == nil ||
		!strings.Contains(err.Error(), "--cli") {
		t.Errorf("buildTask() without a cli = %v, want an error naming --cli", err)
	}
}

func TestBuildTaskAcceptsASourceWithNoPrompt(t *testing.T) {
	// The prompt is filled in from the ticket at launch, so a task with only
	// a source must get past flag validation.
	_, err := buildTask("X", "github://o/r#1", "", t.TempDir(), "", "main", "claude", "")
	if err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Errorf("buildTask() = %v, want it to fail only on the missing repository", err)
	}
}

func TestBuildTaskRejectsANonRepository(t *testing.T) {
	if _, err := buildTask("X", "", "p", t.TempDir(), "", "main", "claude", ""); err == nil {
		t.Error("buildTask() outside a git repository = nil, want an error")
	}
}

func TestCLINamesCoversEveryKnownCLI(t *testing.T) {
	if got, want := len(cliNames()), 3; got != want {
		t.Errorf("cliNames() returned %d names, want %d", got, want)
	}
	for _, want := range []string{"claude", "copilot", "codex"} {
		if !strings.Contains(strings.Join(cliNames(), ","), want) {
			t.Errorf("cliNames() is missing %q", want)
		}
	}
}

// TestRunRejectsFlagsAlongsideABatchFile keeps batch settings coming from the
// file: a flag passed with it would otherwise be dropped without a word.
func TestRunRejectsFlagsAlongsideABatchFile(t *testing.T) {
	for _, flags := range [][]string{
		{"--cli", "claude"},
		{"--model", "opus-4-6"},
		{"--branch", "b"},
		{"--base-branch", "main"},
		{"--id", "X"},
	} {
		var out bytes.Buffer
		argv := append(append([]string{"run"}, flags...), "tasks.yaml")
		err := dispatch(argv, &out)
		if err == nil {
			t.Errorf("dispatch(%v) = nil, want a refusal", argv)
			continue
		}
		if !strings.Contains(err.Error(), flags[0]) {
			t.Errorf("dispatch(%v) = %q, want it to name %s", argv, err, flags[0])
		}
	}
}
