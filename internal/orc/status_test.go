package orc

import (
	"bytes"
	"strings"
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
