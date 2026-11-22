package orc

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// TestRunIDTellsApartRunsThatStartedTogether covers what a start time cannot:
// two dispatches of one id at the same instant are still different runs.
func TestRunIDTellsApartRunsThatStartedTogether(t *testing.T) {
	store := state.NewStore(t.TempDir())
	start := time.Now().UTC()
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "R-1", CLI: task.CLIClaude},
		RunID:     "second-run",
		Status:    state.StatusRunning,
		StartedAt: start,
	}); err != nil {
		t.Fatal(err)
	}

	stale := scopeTo(store, state.Run{ID: "first-run", StartedAt: start})
	if err := stale.update("R-1", func(k *state.Task) { k.Error = "from the first run" }); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Load("R-1"); got.Error != "" {
		t.Errorf("error = %q, want the first run's write dropped", got.Error)
	}
	if reason := stale.halted("R-1"); reason == nil || !strings.Contains(reason.Error(), "later run") {
		t.Errorf("halted() = %v, want the run reported as replaced", reason)
	}
}

func TestSuperviseRefusesAnotherRunID(t *testing.T) {
	layout := testLayout(t)
	store := state.NewStore(layout.State)
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "R-2", CLI: task.CLIClaude},
		RunID:     "current-run",
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	err := NewSupervisor(layout, io.Discard).Supervise("R-2", state.Run{ID: "earlier-run"})
	if err == nil || !strings.Contains(err.Error(), "later run") {
		t.Errorf("Supervise() = %v, want it to refuse a run it was not started for", err)
	}
	if got, _ := store.Load("R-2"); got.Status != state.StatusPending {
		t.Errorf("status = %q, want the current run's record untouched", got.Status)
	}
}
