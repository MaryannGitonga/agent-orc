package orc

import (
	"errors"
	"sync"
	"testing"
	"time"

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
