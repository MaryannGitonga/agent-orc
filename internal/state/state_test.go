package state

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func record(id string, started time.Time) Task {
	return Task{
		Task: task.Task{
			ID: id, Repo: "/repo", Prompt: "p",
			Branch: "b", BaseBranch: "main", CLI: task.CLIClaude,
		},
		Status:    StatusRunning,
		PID:       42,
		Worktree:  filepath.Join("/wt", id),
		LogPath:   filepath.Join("/logs", id+".log"),
		StartedAt: started,
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	want := record("PROJ-1", time.Now().UTC().Truncate(time.Second))

	if err := s.Save(want); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	got, err := s.Load("PROJ-1")
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got.ID != want.ID || got.Status != want.Status || got.PID != want.PID {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
}

func TestLoadUnknownTask(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() = %v, want ErrNotFound", err)
	}
}

func TestUpdateAppliesTheMutation(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save(record("PROJ-1", time.Now())); err != nil {
		t.Fatal(err)
	}

	code := 0
	if err := s.Update("PROJ-1", func(k *Task) {
		k.Status = StatusDone
		k.ExitCode = &code
	}); err != nil {
		t.Fatalf("Update() = %v", err)
	}

	got, err := s.Load("PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone {
		t.Errorf("Status = %q, want %q", got.Status, StatusDone)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", got.ExitCode)
	}
}

func TestUpdateUnknownTask(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Update("nope", func(*Task) {}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update() = %v, want ErrNotFound", err)
	}
}

func TestListIsOrderedOldestFirst(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	for _, r := range []Task{
		record("third", now.Add(2*time.Hour)),
		record("first", now),
		record("second", now.Add(time.Hour)),
	} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("List() returned %d tasks, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("List()[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestListOnAMissingDirectoryIsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "not-created-yet"))
	got, err := s.List()
	if err != nil {
		t.Fatalf("List() = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save(record("PROJ-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("PROJ-1"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if err := s.Delete("PROJ-1"); err != nil {
		t.Errorf("second Delete() = %v, want nil", err)
	}
	if _, err := s.Load("PROJ-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() after Delete() = %v, want ErrNotFound", err)
	}
}

// TestHasProcessCoversThePhasesAfterTheAgent guards the property `stop` and the
// reconciliation in `status` both key off. The phases that run once the agent
// has exited have children of their own and run until the tests pass or the
// reviewer approves, so leaving them out makes a task with a hung test command
// impossible to stop and impossible to correct.
func TestHasProcessCoversThePhasesAfterTheAgent(t *testing.T) {
	withProcess := []Status{StatusPending, StatusRunning, StatusVerifying, StatusReviewing}
	for _, s := range withProcess {
		if !s.HasProcess() {
			t.Errorf("%q.HasProcess() = false, want true: it can have a live child", s)
		}
		if !s.Active() {
			t.Errorf("%q.Active() = false, want true", s)
		}
	}
	// Publishing is in flight but runs no child of its own that stop could
	// signal, and the terminal statuses have nothing running at all.
	for _, s := range []Status{
		StatusPublishing, StatusDone, StatusFailed, StatusStopped,
		StatusReviewed, StatusReviewFailed, StatusPublishFailed, StatusPolicyViolation,
	} {
		if s.HasProcess() {
			t.Errorf("%q.HasProcess() = true, want false", s)
		}
	}
}
