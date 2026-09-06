package orc

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// startedWriter records what was written and signals the first time anything
// is. Nothing reaches it until the reporter has loaded the record, opened the
// log and copied from it, so closing the channel is the moment the follower is
// known to be attached to that particular file. A sleep here would only be a
// guess at the same thing, and a wrong guess would have the test race the
// follower to the state file and pass for the wrong reason.
type startedWriter struct {
	mu      sync.Mutex
	buf     strings.Builder
	once    sync.Once
	started chan struct{}
}

func (w *startedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	w.once.Do(func() { close(w.started) })
	return n, err
}

func (w *startedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestFollowEndsWhenTheIDIsDispatchedAgain covers the one way `logs -f` can
// wait forever. A task's log is unlinked when its id is reused, so a follower
// holding the old file has a handle on an inode nothing will ever write to
// again, while the record under that id looks alive because a different run
// owns it now. Without noticing the change, the loop polls a dead file until
// the user gives up.
func TestFollowEndsWhenTheIDIsDispatchedAgain(t *testing.T) {
	home := t.TempDir()
	layout := paths.New(home)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.State)

	logPath := layout.LogFile("PROJ-1")
	if err := os.WriteFile(logPath, []byte("first run output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := state.Task{
		Task:      task.Task{ID: "PROJ-1", CLI: task.CLICopilot},
		Status:    state.StatusRunning,
		LogPath:   logPath,
		StartedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}

	out := &startedWriter{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- NewReporter(layout.State, out).Logs("PROJ-1", true, false) }()

	// Wait until the follower is demonstrably reading the first run's log,
	// rather than assuming it got there.
	select {
	case <-out.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the follower never started reading the first log")
	}

	// The id is cleaned up and dispatched again: a new run, still active, with
	// a log file of its own at the same path.
	second := first
	second.StartedAt = time.Now().UTC()
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("second run output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "dispatched again") {
			t.Errorf("Logs() = %v, want it to say the id belongs to a new run", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f never returned; it is following a file nothing writes to")
	}

	if got := out.String(); !strings.Contains(got, "first run output") {
		t.Errorf("output = %q, want what the followed run had written", got)
	} else if strings.Contains(got, "second run output") {
		t.Errorf("output = %q, want no output from the run that took the id over", got)
	}
}
