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

func TestBuildTaskRequiresIDAndPrompt(t *testing.T) {
	if _, err := buildTask("", "p", ".", "", "", "claude", ""); err == nil ||
		!strings.Contains(err.Error(), "--id") {
		t.Errorf("buildTask() without an id = %v, want an error naming --id", err)
	}
	if _, err := buildTask("X", "  ", ".", "", "", "claude", ""); err == nil ||
		!strings.Contains(err.Error(), "--prompt") {
		t.Errorf("buildTask() without a prompt = %v, want an error naming --prompt", err)
	}
	if _, err := buildTask("X", "p", ".", "", "", "", ""); err == nil ||
		!strings.Contains(err.Error(), "--cli") {
		t.Errorf("buildTask() without a cli = %v, want an error naming --cli", err)
	}
}

func TestBuildTaskRejectsANonRepository(t *testing.T) {
	if _, err := buildTask("X", "p", t.TempDir(), "", "main", "claude", ""); err == nil {
		t.Error("buildTask() outside a git repository = nil, want an error")
	}
}
