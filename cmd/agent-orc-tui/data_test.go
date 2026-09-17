package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func TestTailLinesKeepsTheEnd(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"a\nb", 5, "a\nb"},
		{"a\nb\nc\nd", 2, "c\nd"},
		{"a\nb\n", 2, "a\nb"}, // a trailing newline is not an empty last line
	} {
		if got := tailLines(tc.in, tc.n); got != tc.want {
			t.Errorf("tailLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestLoadTasksPutsTheNewestFirst(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	base := time.Now().UTC().Add(-time.Hour)
	for i, id := range []string{"OLD-1", "MID-1", "NEW-1"} {
		if err := store.Save(state.Task{
			Task:      task.Task{ID: id, CLI: task.CLIClaude},
			Status:    state.StatusDone,
			StartedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := loadTasks(store)
	if err != nil {
		t.Fatal(err)
	}
	// The task someone just dispatched is the one they are watching, so it
	// belongs at the top rather than at the bottom of a list that scrolls off.
	want := []string{"NEW-1", "MID-1", "OLD-1"}
	if len(tasks) != len(want) {
		t.Fatalf("loadTasks() returned %d task(s), want %d", len(tasks), len(want))
	}
	for i, id := range want {
		if tasks[i].ID != id {
			t.Errorf("tasks[%d] = %q, want %q", i, tasks[i].ID, id)
		}
	}
}

func TestLoadTasksDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	// A record that looks running behind a pid that is long gone: exactly what
	// the status table reconciles by writing. A dashboard polling once a second
	// must not take that write lock over and over.
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "GONE-1", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       0x7FFFFFFF,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "GONE-1.json")
	before, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := loadTasks(store); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("loadTasks() rewrote the record; it must only read")
	}
}

func TestSupervisorLogSaysWhenThereIsNothingYet(t *testing.T) {
	home := t.TempDir()
	layout := paths.New(home)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}

	if got := supervisorLog(layout, "MISSING-1"); !strings.Contains(got, "nothing yet") {
		t.Errorf("supervisorLog() = %q, want it to say the log has not been written", got)
	}

	body := "[2026-09-18T10:22:41Z] running the test command (attempt 1): go test ./...\n"
	if err := os.WriteFile(layout.SupervisorLogFile("HAS-1"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := supervisorLog(layout, "HAS-1"); !strings.Contains(got, "attempt 1") {
		t.Errorf("supervisorLog() = %q, want the log's contents", got)
	}
}

func TestAgentLogReportsAMissingTaskRatherThanCrashing(t *testing.T) {
	home := t.TempDir()
	layout := paths.New(home)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	// The selection can move onto a task that cleanup removed a moment ago.
	if got := agentLog(layout, "NEVER-EXISTED"); !strings.Contains(got, "could not read") {
		t.Errorf("agentLog() = %q, want it to report the failure in the pane", got)
	}
}
