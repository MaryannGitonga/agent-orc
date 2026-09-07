package orc

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// TestStopBetweenCommands covers the window in the phases that run after the
// agent. The test and review loops spend a moment between one child exiting and
// the next starting, and there is no pid to signal then. Refusing there told the
// user to race a loop that was about to spend more of their budget; recording
// the stop is what actually ends it, because both loops check before starting
// anything else.
func TestStopBetweenCommands(t *testing.T) {
	for _, status := range []state.Status{state.StatusVerifying, state.StatusReviewing} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			store := state.NewStore(dir)
			if err := store.Save(state.Task{
				Task:      task.Task{ID: "PROJ-1", CLI: task.CLIClaude},
				Status:    status,
				PID:       0,
				StartedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}

			var out strings.Builder
			if err := NewReporter(dir, &out).Stop("PROJ-1"); err != nil {
				t.Fatalf("Stop() = %v, want the stop recorded", err)
			}
			if got := out.String(); !strings.Contains(got, "nothing was running to signal") {
				t.Errorf("output = %q, want it to say there was nothing to signal", got)
			}
			got, err := store.Load("PROJ-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != state.StatusStopped {
				t.Errorf("status = %q, want stopped so the loop does not start again", got.Status)
			}
		})
	}
}

// TestStopStillRefusesATaskThatNeverStarted keeps the change above narrow: a
// pending task has no process because its agent has not been launched, which is
// a different thing from being between two of them.
func TestStopStillRefusesATaskThatNeverStarted(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "PROJ-2", CLI: task.CLIClaude},
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := NewReporter(dir, &out).Stop("PROJ-2")
	if err == nil || !strings.Contains(err.Error(), "may not have started yet") {
		t.Errorf("Stop() = %v, want it to say the task has no process yet", err)
	}
	if got, _ := store.Load("PROJ-2"); got.Status != state.StatusPending {
		t.Errorf("status = %q, want pending left alone", got.Status)
	}
}

// TestStopClearsThePID covers what the record says once a stop has finished.
// signalGroup does not return until the group is confirmed gone, so by then the
// number the record holds names nothing, and leaving it there points at whoever
// the kernel hands it to next.
func TestStopClearsThePID(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)

	// A real process in a group of its own, the way the supervisor starts one.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	if err := store.Save(state.Task{
		Task:      task.Task{ID: "PID-1", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       pid,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := NewReporter(dir, &out).Stop("PID-1"); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	got, err := store.Load("PID-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if got.PID != 0 {
		t.Errorf("pid = %d, want it cleared once the process is confirmed gone", got.PID)
	}
}
