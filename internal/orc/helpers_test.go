package orc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout := paths.New(t.TempDir())
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestStandingInstructionsAddUp(t *testing.T) {
	layout := testLayout(t)
	d := &Dispatcher{layout: layout, out: io.Discard}

	// No machine-wide file is the normal case, not an error.
	if got, err := d.standingInstructions(task.Task{Instructions: "  batch rule \n"}); err != nil || got != "batch rule" {
		t.Errorf("standingInstructions() = %q, %v, want the batch's rule alone", got, err)
	}

	if err := os.WriteFile(layout.InstructionsFile(), []byte("global rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := d.standingInstructions(task.Task{Instructions: "batch rule"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "global rule\n\nbatch rule"; got != want {
		t.Errorf("standingInstructions() = %q, want %q, broadest first", got, want)
	}
}

func TestStandingInstructionsReportsAFileItCannotRead(t *testing.T) {
	layout := testLayout(t)
	// A directory where the file should be: there, but not readable as rules.
	if err := os.MkdirAll(layout.InstructionsFile(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (&Dispatcher{layout: layout}).standingInstructions(task.Task{})
	if err == nil || !strings.Contains(err.Error(), layout.InstructionsFile()) {
		t.Errorf("standingInstructions() = %v, want an error naming the file", err)
	}
}

func TestTruncateLogsClearsAnEarlierRun(t *testing.T) {
	layout := testLayout(t)
	d := &Dispatcher{layout: layout}
	logs := []string{layout.LogFile("X"), layout.SupervisorLogFile("X"), layout.ReviewLogFile("X")}
	for _, path := range logs {
		if err := os.WriteFile(path, []byte("earlier run\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.truncateLogs("X"); err != nil {
		t.Fatalf("truncateLogs() = %v", err)
	}
	for _, path := range logs {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived, want it removed", path)
		}
	}
	// A second time, with nothing left, is not an error either.
	if err := d.truncateLogs("X"); err != nil {
		t.Errorf("truncateLogs() with no logs = %v", err)
	}
}

func TestTruncateLogsReportsWhatItCannotClear(t *testing.T) {
	layout := testLayout(t)
	stuck := layout.LogFile("X")
	if err := os.MkdirAll(filepath.Join(stuck, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&Dispatcher{layout: layout}).truncateLogs("X"); err == nil || !strings.Contains(err.Error(), stuck) {
		t.Errorf("truncateLogs() = %v, want an error naming %s", err, stuck)
	}
}

func TestAnUnknownCLIIsAnErrorNotAPanic(t *testing.T) {
	// Supervise runs from a state file, which can name any CLI at all.
	tk := task.Task{ID: "X", CLI: task.CLI("nope")}
	if _, err := agentBinary(tk); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("agentBinary() = %v, want an error naming the cli", err)
	}
	if _, err := buildCommand(tk, "sess"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("buildCommand() = %v, want an error naming the cli", err)
	}

	s := &Supervisor{out: io.Discard}
	record := state.Task{Task: tk}
	if got := s.readSessionID(record); got != "" {
		t.Errorf("readSessionID() = %q, want nothing for a cli with no adapter", got)
	}
	if got := s.readUsage(record); got != nil {
		t.Errorf("readUsage() = %v, want nothing for a cli with no adapter", got)
	}
}

func TestBuildCommandPinsTheSession(t *testing.T) {
	argv, err := buildCommand(task.Task{ID: "X", CLI: task.CLIClaude, Prompt: "do it"}, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); !strings.HasPrefix(got, "claude ") || !strings.HasSuffix(got, "--session-id sess-1") {
		t.Errorf("argv = %q, want claude's command ending with the assigned session", got)
	}
}

func TestReadSessionIDPrefersTheAssignedOne(t *testing.T) {
	s := &Supervisor{out: io.Discard}
	log := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(log, []byte(`{"session_id":"from-the-log"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := state.Task{Task: task.Task{CLI: task.CLIClaude}, LogPath: log}

	if got := s.readSessionID(record); got != "from-the-log" {
		t.Errorf("readSessionID() = %q, want the log's when none was assigned", got)
	}
	record.SessionID = "assigned"
	if got := s.readSessionID(record); got != "assigned" {
		t.Errorf("readSessionID() = %q, want the one agent-orc assigned at launch", got)
	}
	record.SessionID, record.LogPath = "", filepath.Join(t.TempDir(), "missing.log")
	if got := s.readSessionID(record); got != "" {
		t.Errorf("readSessionID() = %q, want nothing when the log cannot be read", got)
	}
}
