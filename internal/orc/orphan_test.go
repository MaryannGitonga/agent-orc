package orc

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// supervisedRecord saves a record whose supervisor has started, and returns the
// release that stands in for the supervisor dying.
func supervisedRecord(t *testing.T, store *state.Store, status state.Status, pid int) func() {
	t.Helper()
	r := state.Task{
		Task:      task.Task{ID: "O-1", CLI: task.CLIClaude, Branch: "agent-orc/o-1"},
		RunID:     "run-o",
		Status:    status,
		PID:       pid,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	release, err := store.HoldRun(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

func statusOf(t *testing.T, dir string) (string, state.Task) {
	t.Helper()
	var out bytes.Buffer
	if err := NewReporter(dir, &out).Status(); err != nil {
		t.Fatal(err)
	}
	got, err := state.NewStore(dir).Load("O-1")
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), got
}

func TestStatusMarksAnOrphanedPublishInterrupted(t *testing.T) {
	dir := t.TempDir()
	supervisedRecord(t, state.NewStore(dir), state.StatusPublishing, 0)()

	out, got := statusOf(t, dir)
	if got.Status != state.StatusInterrupted || !strings.Contains(got.Error, "agent-orc pr O-1") {
		t.Errorf("record = %s %q, want interrupted, pointing at agent-orc pr", got.Status, got.Error)
	}
	if !strings.Contains(out, "agent-orc pr O-1") {
		t.Errorf("status output = %q, want the way to continue shown", out)
	}
}

func TestStatusNamesTheBranchOfAnOrphanedRun(t *testing.T) {
	dir := t.TempDir()
	supervisedRecord(t, state.NewStore(dir), state.StatusVerifying, 0)()

	_, got := statusOf(t, dir)
	if got.Status != state.StatusInterrupted || !strings.Contains(got.Error, "agent-orc/o-1") {
		t.Errorf("record = %s %q, want interrupted, naming the branch", got.Status, got.Error)
	}
}

// TestStatusTrustsALiveSupervisor covers the moment between the agent exiting
// and its supervisor recording that: the task is not failed, just busy.
func TestStatusTrustsALiveSupervisor(t *testing.T) {
	dir := t.TempDir()
	supervisedRecord(t, state.NewStore(dir), state.StatusRunning, 0x7FFFFFF0)

	if _, got := statusOf(t, dir); got.Status != state.StatusRunning {
		t.Errorf("status = %s, want running while the supervisor holds its lock", got.Status)
	}
}

// TestStatusLeavesARunningAgentToStop covers an orphan whose agent still runs:
// marking it would take away the pid stop needs.
func TestStatusLeavesARunningAgentToStop(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	dir := t.TempDir()
	store := state.NewStore(dir)
	supervisedRecord(t, store, state.StatusRunning, pid)()

	if _, got := statusOf(t, dir); got.Status != state.StatusRunning || got.PID != pid {
		t.Errorf("record = %s pid %d, want running with its pid kept", got.Status, got.PID)
	}
	if !TaskAlive(store, state.Task{Task: task.Task{ID: "O-1"}, RunID: "run-o", Status: state.StatusRunning, PID: pid}) {
		t.Error("TaskAlive() = false for an agent that is still running")
	}
}

func TestStatusWaitsForASupervisorThatHasNotStarted(t *testing.T) {
	dir := t.TempDir()
	if err := state.NewStore(dir).Save(state.Task{
		Task:      task.Task{ID: "O-1", CLI: task.CLIClaude},
		RunID:     "run-o",
		Status:    state.StatusPending,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, got := statusOf(t, dir); got.Status != state.StatusPending {
		t.Errorf("status = %s, want pending until the supervisor takes its lock", got.Status)
	}
}

func TestTaskAliveIsFalseForAnOrphan(t *testing.T) {
	store := state.NewStore(t.TempDir())
	supervisedRecord(t, store, state.StatusPublishing, 0)()
	rec, err := store.Load("O-1")
	if err != nil {
		t.Fatal(err)
	}
	if TaskAlive(store, rec) {
		t.Error("TaskAlive() = true for a task whose supervisor is gone")
	}
}

func TestTaskAliveTrustsALiveSupervisor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	supervisedRecord(t, store, state.StatusRunning, 0x7FFFFFF0)
	rec, err := store.Load("O-1")
	if err != nil {
		t.Fatal(err)
	}
	if !TaskAlive(store, rec) {
		t.Error("TaskAlive() = false while the supervisor holds its lock")
	}
}

// TestADuplicateSupervisorLeavesTheRunAlone covers a second supervise for a run
// that already has one: it is refused, and the run is not marked failed.
func TestADuplicateSupervisorLeavesTheRunAlone(t *testing.T) {
	layout := testLayout(t)
	store := state.NewStore(layout.State)
	supervisedRecord(t, store, state.StatusRunning, 0x7FFFFFF0)

	err := NewSupervisor(layout, io.Discard).Supervise("O-1", state.Run{ID: "run-o"})
	if !errors.Is(err, state.ErrAlreadySupervised) {
		t.Fatalf("Supervise() = %v, want ErrAlreadySupervised", err)
	}
	if got, _ := store.Load("O-1"); got.Status != state.StatusRunning {
		t.Errorf("status = %s, want the first supervisor's run untouched", got.Status)
	}
}

// TestARunningWriteDoesNotUndoAStop covers a stop landing between the
// supervisor's last check and its agent starting: the stop must survive.
func TestARunningWriteDoesNotUndoAStop(t *testing.T) {
	layout := testLayout(t)
	store := state.NewStore(layout.State)
	rec := state.Task{
		Task:      task.Task{ID: "R-9", CLI: task.CLIClaude},
		RunID:     "run-9",
		Status:    state.StatusStopped,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(layout, io.Discard)
	s.scope = scopeTo(store, rec.Run())

	stopped, _, err := s.markRunning("R-9", 4242)
	if err != nil || !stopped {
		t.Fatalf("markRunning() = stopped %v, %v; want it to see the stop", stopped, err)
	}
	if got, _ := store.Load("R-9"); got.Status != state.StatusStopped || got.PID != 0 {
		t.Errorf("record = %s pid %d, want it left stopped", got.Status, got.PID)
	}
}
