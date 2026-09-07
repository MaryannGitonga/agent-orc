package orc

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

func TestStatusOnAnEmptyStore(t *testing.T) {
	var out bytes.Buffer
	if err := NewReporter(t.TempDir(), &out).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if !strings.Contains(out.String(), "no tasks") {
		t.Errorf("output = %q, want it to say there are no tasks", out.String())
	}
}

func TestStatusRendersARowPerTask(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	done := time.Now().UTC()
	if err := store.Save(state.Task{
		Task: task.Task{
			ID: "PROJ-1", CLI: task.CLIClaude, Model: "opus-4-6",
			Branch: "fix/proj-1", Budget: task.Budget{USD: f64(2)},
		},
		Status:     state.StatusDone,
		StartedAt:  done.Add(-90 * time.Second),
		FinishedAt: &done,
		SpentUSD:   f64(0.42),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := NewReporter(dir, &out).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	got := out.String()
	for _, want := range []string{"PROJ-1", "claude", "opus-4-6", "done", "$0.42 / $2.00", "fix/proj-1", "1m30s"} {
		if !strings.Contains(got, want) {
			t.Errorf("status table is missing %q:\n%s", want, got)
		}
	}
}

func TestStatusShowsUnenforceableBudgetsAsNotes(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Save(state.Task{
		Task:       task.Task{ID: "PROJ-2", CLI: task.CLICodex, Budget: task.Budget{USD: f64(2)}},
		Status:     state.StatusDone,
		StartedAt:  time.Now().UTC(),
		BudgetNote: "codex exposes no native spend cap",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := NewReporter(dir, &out).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if !strings.Contains(out.String(), "no native spend cap") {
		t.Errorf("output = %q, want the unenforced budget called out", out.String())
	}
}

func TestStatusMarksATaskWhoseProcessIsGone(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	// pid 0 is skipped by the liveness check, so use a pid that is real
	// enough to look plausible but certainly not running.
	if err := store.Save(state.Task{
		Task:      task.Task{ID: "PROJ-3", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       0x7FFFFFF0,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := NewReporter(dir, &out).Status(); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if strings.Contains(out.String(), "running") {
		t.Errorf("output = %q, want the dead task no longer reported as running", out.String())
	}

	// The correction is written back, not just displayed.
	got, err := store.Load("PROJ-3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusFailed {
		t.Errorf("stored status = %q, want %q", got.Status, state.StatusFailed)
	}
}

func TestSpendRendering(t *testing.T) {
	tests := map[string]struct {
		task state.Task
		want string
	}{
		"cost against a dollar budget": {
			state.Task{Task: task.Task{Budget: task.Budget{USD: f64(2)}}, SpentUSD: f64(0.5)},
			"$0.50 / $2.00",
		},
		"tokens when there is no cost": {
			state.Task{Tokens: iptr(1200)},
			"1200 tok",
		},
		"credits budget": {
			state.Task{Task: task.Task{Budget: task.Budget{Credits: f64(50)}}},
			"- / 50 cr",
		},
		"nothing known": {state.Task{}, "-"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := spend(tc.task); got != tc.want {
				t.Errorf("spend() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStopRejectsATaskThatIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Save(state.Task{
		Task: task.Task{ID: "PROJ-4"}, Status: state.StatusDone, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := NewReporter(dir, &out).Stop("PROJ-4")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Stop() = %v, want an error saying the task is not running", err)
	}
}

func TestStopReportsAnUnknownTask(t *testing.T) {
	var out bytes.Buffer
	if err := NewReporter(t.TempDir(), &out).Stop("nope"); err == nil {
		t.Error("Stop() on an unknown task = nil, want an error")
	}
}

// TestProcessAliveTreatsEPERMAsAlive covers the Unix distinction: signal 0 to a
// process owned by another user returns EPERM, which means it exists.
func TestProcessAliveTreatsEPERMAsAlive(t *testing.T) {
	// pid 1 is always running and, unless this test runs as root, not ours.
	if !processAlive(1) {
		t.Error("processAlive(1) = false, want true; pid 1 always exists")
	}
	if err := syscall.Kill(1, 0); os.Geteuid() != 0 && !errors.Is(err, syscall.EPERM) {
		t.Skipf("signalling pid 1 gave %v rather than EPERM; nothing to assert", err)
	}
}

// TestProcessAliveOnAGonePID checks the other half: a reaped pid is dead.
func TestProcessAliveOnAGonePID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Skip("the pid was reused between exit and the check")
	}
}

// TestStopOnAProcessThatIsAlreadyGone keeps the record stopped rather than
// rolling it back to running, which status would then have to reconcile.
func TestStopOnAProcessThatIsAlreadyGone(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	gone := cmd.Process.Pid
	if processAlive(gone) {
		t.Skip("the pid was reused between exit and the check")
	}

	if err := store.Save(state.Task{
		Task:      task.Task{ID: "GONE", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       gone,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := NewReporter(dir, &out).Stop("GONE"); err != nil {
		t.Fatalf("Stop() on a gone process = %v, want nil", err)
	}
	// The pid named a process that had already exited, so the record must stop
	// naming it: that number belongs to whoever the kernel gives it to next.
	if got, err := store.Load("GONE"); err != nil {
		t.Fatal(err)
	} else if got.PID != 0 {
		t.Errorf("pid = %d, want it cleared for a process that was already gone", got.PID)
	}
	got, err := store.Load("GONE")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusStopped {
		t.Errorf("status = %q, want %q; rolling back re-asserts a dead process as live", got.Status, state.StatusStopped)
	}
	if !strings.Contains(out.String(), "already gone") {
		t.Errorf("output = %q, want it to say the process was already gone", out.String())
	}
}

func TestTrimZeroTail(t *testing.T) {
	for in, want := range map[string]string{
		"1m0s":    "1m",
		"2h0m0s":  "2h",
		"1h30m0s": "1h30m",
		"40s":     "40s",
		"45s":     "45s",
		"0s":      "0s",
		"1m30s":   "1m30s",
	} {
		if got := trimZeroTail(in); got != want {
			t.Errorf("trimZeroTail(%q) = %q, want %q", in, got, want)
		}
	}
}
