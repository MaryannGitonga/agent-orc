package orc

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
// The interleaving is arranged rather than waited for: the task's lock is held
// so cleanup stops at the delete, the new run is written while it waits, and
// the lock is then released to let it finish against a record it never loaded.
// Whether cleanup reached the lock in time is not assumed, it is read back from
// what it returned, and an attempt that lost the ordering is retried rather
// than asserted on.
func TestCleanupDeletesOnlyTheRunItLoaded(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Root: home, State: filepath.Join(home, "state"), Logs: filepath.Join(home, "logs")}
	store := state.NewStore(layout.State)
	record := filepath.Join(layout.State, "Y.json")
	firstRun := time.Now().UTC().Add(-time.Hour)

	for attempt := 1; ; attempt++ {
		if attempt > 20 {
			t.Fatal("cleanup never reached the delete while the lock was held")
		}
		secondRun := time.Now().UTC()

		// A finished task whose worktree is already gone, so cleanup has no git
		// to do and reaches the delete directly.
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
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() { done <- NewCleaner(layout, io.Discard).Clean("Y", false, false) }()

		// Time for cleanup to load the finished run and block on the lock: all
		// it has to do first is one stat. Whether it managed to is checked
		// below rather than trusted.
		time.Sleep(200 * time.Millisecond)

		// The id, dispatched again while cleanup waits. Written directly
		// because Save would want the lock this test is holding.
		replacement, err := json.Marshal(state.Task{
			Task:      task.Task{ID: "Y", CLI: task.CLIClaude},
			Status:    state.StatusRunning,
			PID:       222,
			StartedAt: secondRun,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(record, replacement, 0o644); err != nil {
			t.Fatal(err)
		}

		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		lock.Close()

		// Cleanup refuses an active task, so an error naming that is proof it
		// read the replacement instead of the finished run: the ordering this
		// test needs did not happen, and there is nothing to assert on.
		switch err := <-done; {
		case err == nil: // it deleted against the record it loaded
		case strings.Contains(err.Error(), "stop it first"):
			continue
		default:
			t.Fatalf("Clean() = %v", err)
		}

		got, err := store.Load("Y")
		if err != nil {
			t.Fatalf("the new run's record did not survive cleanup: %v", err)
		}
		if !got.StartedAt.Equal(secondRun) || got.PID != 222 {
			t.Errorf("record is %v pid=%d, want the second run untouched", got.StartedAt, got.PID)
		}
		return
	}
}

// TestHaltedNamesTheReasonThisRunIsOver covers what the loops that outlive the
// agent ask before each round. A stop is not the only ending: the task having
// been cleaned up, or its id taken by a newer run, ends this run just as
// firmly. They are reported apart, because a run that lost its id is not one a
// human stopped, and a store that cannot be read is neither.
func TestHaltedNamesTheReasonThisRunIsOver(t *testing.T) {
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
	wantReason := func(what string, got error, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("halted() = nil for %s, want %q", what, want)
			return
		}
		if !strings.Contains(got.Error(), want) {
			t.Errorf("halted() = %q for %s, want it to say %q", got, what, want)
		}
	}

	save(state.StatusRunning, firstRun)
	if reason := mine.halted("S"); reason != nil {
		t.Errorf("halted() = %v for this run, still running", reason)
	}

	save(state.StatusStopped, firstRun)
	wantReason("a stopped task", mine.halted("S"), "was stopped")

	// Cleaned up and dispatched again: a healthy record, but not this one's.
	save(state.StatusRunning, time.Now().UTC())
	wantReason("a replaced run", mine.halted("S"), "belongs to a later run")
	// An unscoped caller is asking about whatever the id names now.
	if reason := scopeTo(store, time.Time{}).halted("S"); reason != nil {
		t.Errorf("halted() = %v unscoped, for a running task", reason)
	}

	// A store that cannot be read is not an answer about ownership, and saying
	// the run was replaced would send someone looking for a task that is fine.
	if err := os.WriteFile(filepath.Join(dir, "S.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantReason("an unreadable record", mine.halted("S"), "reading task")

	// Removed directly: a record that cannot be parsed cannot be judged, so
	// Delete declines to guess which run it would be removing.
	if err := os.Remove(filepath.Join(dir, "S.json")); err != nil {
		t.Fatal(err)
	}
	wantReason("a deleted task", mine.halted("S"), "no longer exists")
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

// TestSuperviseStopsAnAgentItNoLongerOwns covers the gap between the check at
// the top of Supervise and the agent actually starting. A forced cleanup and a
// redispatch in that window leave an agent from the old run loose in the new
// run's worktree, and declining to record its pid does not make it stop: it has
// to be killed, or it goes on committing in a checkout that is not its own.
func TestSuperviseStopsAnAgentItNoLongerOwns(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Root: home, State: filepath.Join(home, "state"), Logs: filepath.Join(home, "logs")}
	store := state.NewStore(layout.State)
	dispatchedFor := time.Now().UTC().Add(-time.Hour)
	if err := os.MkdirAll(layout.Logs, 0o755); err != nil {
		t.Fatal(err)
	}

	// A stub on PATH that outlives this test unless something stops it, and
	// says where to find it.
	pidFile := filepath.Join(home, "agent.pid")
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\necho $$ > " + pidFile + "\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := store.Save(state.Task{
		Task:      task.Task{ID: "K", CLI: task.CLIClaude, Prompt: "do it"},
		Worktree:  home,
		LogPath:   filepath.Join(layout.Logs, "K.log"),
		Status:    state.StatusPending,
		StartedAt: dispatchedFor,
	}); err != nil {
		t.Fatal(err)
	}

	// Held so the supervisor blocks where it records the running agent, which
	// is the first thing it does after starting one.
	lock, err := os.OpenFile(filepath.Join(layout.State, "K.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- NewSupervisor(layout, io.Discard).Supervise("K", dispatchedFor) }()

	// Long enough for the agent to have started and written its pid.
	var agentPID int
	for waited := 0; agentPID == 0 && waited < 100; waited++ {
		time.Sleep(50 * time.Millisecond)
		if b, err := os.ReadFile(pidFile); err == nil {
			agentPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if agentPID == 0 {
		t.Fatal("the stub agent never started")
	}

	// Cleaned up and dispatched again while the supervisor waits to record it.
	replacement, err := json.Marshal(state.Task{
		Task:      task.Task{ID: "K", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.State, "K.json"), replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no longer belongs") {
			t.Fatalf("Supervise() = %v, want it to report the id had moved on", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Supervise() waited for an agent it does not own")
	}

	// The point of all this: the agent is gone, not merely unrecorded.
	if err := syscall.Kill(agentPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill(%d, 0) = %v, want the agent to have been stopped", agentPID, err)
	}
}
