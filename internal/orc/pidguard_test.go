package orc

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// The pids a state file can hold that kill reads as something other than one
// task's group: -1 for 1, our own group for 0, and a bare pid for a negative.
// The kernel keeps only the low 32 bits, so 1<<32+1 wraps to 1 and 1<<33 to 0.
//
// Every check here sends signal 0, which delivers nothing, and nothing here
// stops a task recorded with pid 1: if the guard were lost, that would send
// SIGTERM to every process the user running the tests owns.
var notAGroup = []int{1, 0, -1, -0x7FFFFFF0, 1<<32 + 1, 1 << 33}

func TestNoSignalReachesAPidThatIsNotATaskGroup(t *testing.T) {
	for _, pid := range notAGroup {
		if err := signal(pid, 0); err == nil || !strings.Contains(err.Error(), "state file") {
			t.Errorf("signal(%d) = %v, want a refusal naming the state file", pid, err)
		}
		if groupAlive(pid) {
			t.Errorf("groupAlive(%d) = true, want false for a pid no task group can have", pid)
		}
	}
}

func TestStatusFailsATaskRecordedAsPidOne(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "INIT", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       1,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Asked about pid 1, kill answers for every process there is, so the task
	// looked alive forever and stop was one command away from signalling them.
	if err := NewReporter(dir, &bytes.Buffer{}).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	got, err := store.Load("INIT")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusFailed || got.PID != 0 {
		t.Errorf("record = %s pid=%d, want failed with the pid cleared", got.Status, got.PID)
	}
}

func TestStopRefusesANegativePid(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	// Negated, this is a bare pid nothing holds, so even without the guard the
	// signal would find no one.
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "NEG", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       -0x7FFFFFF0,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	err := NewReporter(dir, &bytes.Buffer{}).Stop("NEG")
	if err == nil || !strings.Contains(err.Error(), "state file") {
		t.Fatalf("Stop() = %v, want a refusal naming the state file", err)
	}
	// Nothing was signalled, so nothing was stopped: the record goes back to
	// what it said, for status to reconcile.
	got, err := store.Load("NEG")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusRunning {
		t.Errorf("status = %q, want it rolled back to running", got.Status)
	}
}
