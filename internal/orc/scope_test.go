package orc

import (
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

		var wg sync.WaitGroup
		wg.Add(2)
		// The supervisor of the run that is being replaced, finishing up.
		go func() {
			defer wg.Done()
			_ = stale.update("X", func(k *state.Task) {
				k.Status = state.StatusDone
				k.PID = 0
			})
		}()
		// cleanup, then a new task taking the id.
		go func() {
			defer wg.Done()
			_ = store.Delete("X")
			_ = store.Save(state.Task{
				Task:      task.Task{ID: "X", CLI: task.CLIClaude},
				Status:    state.StatusRunning,
				PID:       222,
				StartedAt: secondRun,
			})
		}()
		wg.Wait()

		got, err := store.Load("X")
		if err != nil {
			continue // the delete won; nothing to check
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
