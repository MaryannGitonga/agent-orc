package orc

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// TestStaleWriteCannotClobberARedispatchedTask covers the interleaving the
// scoping exists to stop: a supervisor loads a record it owns, and before it
// writes, cleanup removes the task and a new dispatch takes the id.
//
// The two run against each other for real rather than being stepped through,
// because the window is a few instructions wide. Whichever order they land in,
// the invariant holds: once the record on disk is the new run, nothing the
// stale writer does may appear in it.
func TestStaleWriteCannotClobberARedispatchedTask(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	firstRun := time.Now().UTC().Add(-time.Hour)
	stale := scopeTo(store, firstRun)

	for i := 0; i < 300; i++ {
		if err := store.Save(state.Task{
			Task:      task.Task{ID: "X", CLI: task.CLIClaude},
			Status:    state.StatusRunning,
			PID:       111,
			StartedAt: firstRun,
		}); err != nil {
			t.Fatal(err)
		}
		secondRun := time.Now().UTC()

		// Each goroutine keeps its own error for the main one to check, so a
		// lock or disk failure cannot pass for the race resolving cleanly.
		var staleErr, deleteErr, saveErr error
		var wg sync.WaitGroup
		wg.Add(2)
		// The supervisor of the run that is being replaced, finishing up.
		go func() {
			defer wg.Done()
			staleErr = stale.update("X", func(k *state.Task) {
				k.Status = state.StatusDone
				k.PID = 0
			})
		}()
		// cleanup, then a new task taking the id.
		go func() {
			defer wg.Done()
			deleteErr = store.Delete("X")
			saveErr = store.Save(state.Task{
				Task:      task.Task{ID: "X", CLI: task.CLIClaude},
				Status:    state.StatusRunning,
				PID:       222,
				StartedAt: secondRun,
			})
		}()
		wg.Wait()

		// The stale update is allowed to find the record gone, and nothing else.
		if staleErr != nil && !errors.Is(staleErr, state.ErrNotFound) {
			t.Fatalf("iteration %d: stale update: %v", i, staleErr)
		}
		if deleteErr != nil {
			t.Fatalf("iteration %d: delete: %v", i, deleteErr)
		}
		if saveErr != nil {
			t.Fatalf("iteration %d: redispatch: %v", i, saveErr)
		}

		got, err := store.Load("X")
		if errors.Is(err, state.ErrNotFound) {
			continue // the delete won; nothing to check
		}
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !got.StartedAt.Equal(secondRun) {
			continue // the old run is still on disk, about to be cleaned up
		}
		if got.Status != state.StatusRunning || got.PID != 222 {
			t.Fatalf("iteration %d: the new run was overwritten by the old one: status=%q pid=%d",
				i, got.Status, got.PID)
		}
	}
}

// TestCleanupDeletesOnlyTheRunItLoaded covers the other end of the same window.
// Cleanup decides what to remove, tears down a worktree and a branch, and only
// then drops the record, by which point the id may have been dispatched again.
//
// The interleaving is pinned rather than raced for: the task's lock is held so
// cleanup stops at the delete, the new run is written while it waits, and the
// lock is then released to let it finish against a record it never loaded.
func TestCleanupDeletesOnlyTheRunItLoaded(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Root: home, State: filepath.Join(home, "state"), Logs: filepath.Join(home, "logs")}
	store := state.NewStore(layout.State)
	firstRun := time.Now().UTC().Add(-time.Hour)
	secondRun := time.Now().UTC()

	// A finished task whose worktree is already gone, so cleanup has no git to
	// do and reaches the delete directly.
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "Y", CLI: task.CLIClaude},
		Worktree:  filepath.Join(home, "gone"),
		Status:    state.StatusDone,
		StartedAt: firstRun,
	}); err != nil {
		t.Fatal(err)
	}

	lock, err := os.OpenFile(filepath.Join(layout.State, "Y.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- NewCleaner(layout, io.Discard).Clean("Y", false, false) }()

	// Long enough for cleanup to load the finished run and block on the lock:
	// all it has to do first is one stat.
	time.Sleep(200 * time.Millisecond)

	// The id, dispatched again while cleanup waits. Written directly because
	// Save would want the lock this test is holding.
	replacement, err := json.Marshal(state.Task{
		Task:      task.Task{ID: "Y", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       222,
		StartedAt: secondRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.State, "Y.json"), replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Clean() = %v", err)
	}

	got, err := store.Load("Y")
	if err != nil {
		t.Fatalf("the new run's record did not survive cleanup: %v", err)
	}
	if !got.StartedAt.Equal(secondRun) || got.PID != 222 {
		t.Errorf("record is %v pid=%d, want the second run untouched", got.StartedAt, got.PID)
	}
}

// TestStoppedTreatsAReplacedRunAsStopped covers what the loops that outlive the
// agent ask before each round. A stop is not the only reason to stop: the task
// having been cleaned up, or the id having been taken by a newer run, ends this
// run just as firmly, and the worktree it would carry on in is not its own.
func TestStoppedTreatsAReplacedRunAsStopped(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	firstRun := time.Now().UTC().Add(-time.Hour)
	mine := scopeTo(store, firstRun)

	save := func(status state.Status, startedAt time.Time) {
		t.Helper()
		if err := store.Save(state.Task{
			Task:      task.Task{ID: "S", CLI: task.CLIClaude},
			Status:    status,
			StartedAt: startedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	save(state.StatusRunning, firstRun)
	if mine.stopped("S") {
		t.Error("stopped() = true for this run, still running")
	}

	save(state.StatusStopped, firstRun)
	if !mine.stopped("S") {
		t.Error("stopped() = false for this run, stopped")
	}

	// Cleaned up and dispatched again: a healthy record, but not this one's.
	save(state.StatusRunning, time.Now().UTC())
	if !mine.stopped("S") {
		t.Error("stopped() = false for a record that belongs to a later run")
	}
	// An unscoped caller is asking about whatever the id names now.
	if scopeTo(store, time.Time{}).stopped("S") {
		t.Error("stopped() = true unscoped, for a running task")
	}

	if err := store.Delete("S"); err != nil {
		t.Fatal(err)
	}
	if !mine.stopped("S") {
		t.Error("stopped() = false for a record that is gone")
	}
}

// TestSuperviseRefusesARunItWasNotStartedFor covers the gap between a record
// being written and its supervisor getting going: a forced cleanup can free the
// id and another run take it, and two agents in one worktree is the worst thing
// that can come of it.
func TestSuperviseRefusesARunItWasNotStartedFor(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Root: home, State: filepath.Join(home, "state"), Logs: filepath.Join(home, "logs")}
	store := state.NewStore(layout.State)
	dispatchedFor := time.Now().UTC().Add(-time.Hour)

	// The record the id names now: a different run from the one being started.
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "Z", CLI: task.CLIClaude},
		Worktree:  filepath.Join(home, "worktree"),
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	err := NewSupervisor(layout, io.Discard).Supervise("Z", dispatchedFor)
	if err == nil {
		t.Fatal("Supervise() = nil, want it to refuse a record from another run")
	}
	if !strings.Contains(err.Error(), "later run") {
		t.Errorf("Supervise() = %v, want it to say the run had moved on", err)
	}
	// Refusing means leaving it alone: no status change, no failure recorded.
	got, err := store.Load("Z")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusPending || got.Error != "" {
		t.Errorf("record is %q/%q, want the newer run untouched", got.Status, got.Error)
	}
}
