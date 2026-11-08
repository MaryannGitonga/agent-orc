package orc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// finishedRecord saves a done task whose worktree is already gone, so cleanup
// has no git to do.
func finishedRecord(t *testing.T, layout paths.Layout, id string) {
	t.Helper()
	if err := state.NewStore(layout.State).Save(state.Task{
		Task:      task.Task{ID: id, CLI: task.CLIClaude, Branch: "agent-orc/" + strings.ToLower(id)},
		Worktree:  filepath.Join(layout.Root, "gone", id),
		Status:    state.StatusDone,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func tornRecord(t *testing.T, layout paths.Layout, id string) string {
	t.Helper()
	path := filepath.Join(layout.State, id+".json")
	if err := os.WriteFile(path, []byte(`{"id": "`+id+`", "sta`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStatusShowsTheRestAndNamesAnUnreadableRecord(t *testing.T) {
	layout := testLayout(t)
	finishedRecord(t, layout, "GOOD-1")
	path := tornRecord(t, layout, "TORN-1")

	var out bytes.Buffer
	if err := NewReporter(layout.State, &out).Status(); err != nil {
		t.Fatalf("Status() = %v, want the table despite one bad record", err)
	}
	got := out.String()
	for _, want := range []string{"GOOD-1", "unreadable  TORN-1", "fix or remove " + path} {
		if !strings.Contains(got, want) {
			t.Errorf("status is missing %q:\n%s", want, got)
		}
	}
}

func TestStatusWithOnlyAnUnreadableRecord(t *testing.T) {
	layout := testLayout(t)
	tornRecord(t, layout, "TORN-1")

	var out bytes.Buffer
	if err := NewReporter(layout.State, &out).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	// "no tasks" alone would claim the directory is empty when it is not.
	if !strings.Contains(out.String(), "unreadable  TORN-1") {
		t.Errorf("status = %q, want the unreadable record named", out.String())
	}
}

func TestCleanupAllNamesWhatItCouldNotRead(t *testing.T) {
	layout := testLayout(t)
	finishedRecord(t, layout, "GOOD-1")
	path := tornRecord(t, layout, "TORN-1")

	var out bytes.Buffer
	err := NewCleaner(layout, &out).CleanAll(false, false)
	if err == nil {
		t.Fatalf("CleanAll() = nil, want it to fail over a record it could not clean\n%s", out.String())
	}
	if !strings.Contains(out.String(), "TORN-1  not cleaned") || !strings.Contains(out.String(), path) {
		t.Errorf("output = %q, want the unreadable record and its file named", out.String())
	}
	if _, err := os.Stat(filepath.Join(layout.State, "GOOD-1.json")); !os.IsNotExist(err) {
		t.Error("GOOD-1 was not cleaned up alongside the unreadable record")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the unreadable record was removed: %v; it is the user's to look at", err)
	}
}

func TestCleanupSweepsAbandonedTempFiles(t *testing.T) {
	layout := testLayout(t)
	finishedRecord(t, layout, "GOOD-1")
	abandoned, err := os.CreateTemp(layout.State, "OTHER-1.*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	abandoned.Close()

	var out bytes.Buffer
	if err := NewCleaner(layout, &out).Clean("GOOD-1", false, false); err != nil {
		t.Fatalf("Clean() = %v", err)
	}
	if _, err := os.Stat(abandoned.Name()); !os.IsNotExist(err) {
		t.Error("an abandoned temporary file survived cleanup")
	}
	if !strings.Contains(out.String(), "removed 1 leftover temporary state file(s)") {
		t.Errorf("output = %q, want the sweep reported", out.String())
	}
}

// TestCleanupWarnsWhenTheSweepFails covers the sweep being housekeeping: a
// file it cannot remove is reported, not turned into a failed cleanup.
func TestCleanupWarnsWhenTheSweepFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	layout := testLayout(t)
	if err := os.WriteFile(filepath.Join(layout.State, "X-1.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stuck, err := os.CreateTemp(layout.State, "X-1.*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	stuck.Close()
	if err := os.Chmod(layout.State, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.State, 0o755) })

	var out bytes.Buffer
	NewCleaner(layout, &out).sweepTemp()
	if !strings.Contains(out.String(), "warning: removing "+stuck.Name()) {
		t.Errorf("output = %q, want a warning naming the file", out.String())
	}
}
