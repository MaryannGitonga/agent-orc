package main

import (
	"fmt"
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
		got, dropped := tailLines(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("tailLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
		// It has to say when it dropped lines, so the pane can say so too.
		if want := strings.Count(strings.TrimRight(tc.in, "\n"), "\n")+1 > tc.n && tc.in != ""; dropped != want {
			t.Errorf("tailLines(%q, %d) reported dropped=%v, want %v", tc.in, tc.n, dropped, want)
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

func TestAgentLogBeforeTheAgentHasWrittenAnything(t *testing.T) {
	// The selection can land on a task whose agent has not started yet, or
	// whose log cleanup removed a moment ago.
	missing := state.Task{
		Task:    task.Task{ID: "NEW-1", CLI: task.CLIClaude},
		LogPath: filepath.Join(t.TempDir(), "NEW-1.log"),
	}
	if got := agentLog(missing); !strings.Contains(got, "nothing yet") {
		t.Errorf("agentLog() = %q, want it to say the log has not been written", got)
	}
}

// writeLines writes n numbered lines, each padded so the file gets large fast.
func writeLines(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %07d %s\n", i, strings.Repeat("x", 80))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadTailIsBounded covers the cost of a refresh. The whole file used to be
// read every second, which for a long run's log is hundreds of megabytes a
// second; what is read now is capped however large the file grows.
func TestReadTailIsBounded(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	writeLines(t, big, 60000) // about 5.5 MB
	data, cut, err := readTail(big, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1<<20 {
		t.Errorf("read %d bytes, want at most %d", len(data), 1<<20)
	}
	if !cut {
		t.Error("readTail() did not report leaving the start of the file out")
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// A whole line first, not the back half of one.
	if !strings.HasPrefix(lines[0], "line ") || len(lines[0]) != len(lines[1]) {
		t.Errorf("the chunk starts part way through a line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "line 0060000") {
		t.Errorf("the chunk does not end at the file's last line: %q", lines[len(lines)-1])
	}

	small := filepath.Join(dir, "small.log")
	writeLines(t, small, 3)
	if data, cut, err := readTail(small, 1<<20); err != nil || cut || strings.Count(string(data), "\n") != 3 {
		t.Errorf("readTail() on a small file = %d bytes, cut=%v, err=%v; want all of it", len(data), cut, err)
	}

	// One line longer than the limit: there is no whole line to show.
	giant := filepath.Join(dir, "giant.log")
	if err := os.WriteFile(giant, []byte(strings.Repeat("y", 2<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, cut, err := readTail(giant, 1<<20); err != nil || !cut || len(data) != 0 {
		t.Errorf("readTail() on one giant line = %d bytes, cut=%v, err=%v; want nothing kept", len(data), cut, err)
	}
}

// TestLogWindowSaysWhenItLeftOutputOut covers what the top of the pane claims.
// With only the end of the log loaded, g lands on the start of that window, and
// without a note it would read as the start of the run.
func TestLogWindowSaysWhenItLeftOutputOut(t *testing.T) {
	home := t.TempDir()
	layout := paths.New(home)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeLines(t, layout.SupervisorLogFile("LONG-1"), 2000)
	got := supervisorLog(layout, "LONG-1")
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], "earlier output not shown") || !strings.Contains(lines[0], "LONG-1.supervisor.log") {
		t.Errorf("the first line does not say earlier output was left out, or where it is:\n%s", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "line 0002000") {
		t.Errorf("the window does not end at the log's last line: %q", lines[len(lines)-1])
	}
	if len(lines) != logTail+1 {
		t.Errorf("the window holds %d lines, want %d and the note", len(lines)-1, logTail)
	}

	writeLines(t, layout.SupervisorLogFile("SHORT-1"), 5)
	if got := supervisorLog(layout, "SHORT-1"); strings.Contains(got, "earlier output") {
		t.Errorf("a log shown in full claims output was left out:\n%s", got)
	}
}
