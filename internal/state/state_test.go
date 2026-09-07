package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// TestUpdateIsAtomicAcrossWriters covers the lock around load, change and save.
// One detached supervisor per task plus whatever the user is typing means
// several processes write one record, and without the lock each would save a
// copy read before the others' changes: the last writer wins and the rest are
// lost.
func TestUpdateIsAtomicAcrossWriters(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(Task{
		Task:      task.Task{ID: "RACE-1", CLI: task.CLIClaude},
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const writers = 40
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each opens the store for itself, the way a separate process does.
			if err := NewStore(dir).Update("RACE-1", func(k *Task) { k.ReviewRound++ }); err != nil {
				t.Errorf("Update() = %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Load("RACE-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewRound != writers {
		t.Errorf("review_round = %d, want %d: %d update(s) were lost",
			got.ReviewRound, writers, writers-got.ReviewRound)
	}
}

// TestUpdateIfLeavesTheFileAloneWhenItDeclines covers the other half: a caller
// that decides not to change anything must not write back the copy it read,
// which would undo whatever another writer had done in the meantime.
func TestUpdateIfLeavesTheFileAloneWhenItDeclines(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(Task{
		Task:      task.Task{ID: "SKIP-1", CLI: task.CLIClaude},
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "SKIP-1.json")
	before, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}

	// Another writer moves the record on. Its timestamp is the one a second
	// write would have to disturb, so it is also the yardstick for whether
	// this filesystem can tell two writes apart at all.
	if err := store.Update("SKIP-1", func(k *Task) { k.Status = StatusDone }); err != nil {
		t.Fatal(err)
	}
	mid, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if mid.ModTime().Equal(before.ModTime()) {
		t.Skip("the filesystem's timestamps are too coarse to tell the writes apart")
	}

	if err := store.UpdateIf("SKIP-1", func(k *Task) bool {
		k.Status = StatusFailed // written to the copy, and meant to be discarded
		return false
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the declining update rewrote the record:\n got %s\nwant %s", got, want)
	}
	after, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(mid.ModTime()) {
		t.Errorf("modtime = %v, want the declining update to have left it at %v", after.ModTime(), mid.ModTime())
	}
}

// TestDeleteLeavesTheLockInPlace pins why the lock file outlives the record it
// guards. Unlinking it would let a caller holding the old file and a caller
// opening a fresh one at the same path both believe they hold the task's lock.
func TestDeleteLeavesTheLockInPlace(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(Task{
		Task:      task.Task{ID: "LOCK-1", CLI: task.CLIClaude},
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "LOCK-1.lock")
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("the save should have created the lock: %v", err)
	}

	if err := store.Delete("LOCK-1"); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("the lock should have survived the delete: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the lock file was replaced, so lockers can end up on separate inodes")
	}

	// The record is gone even so, and the leftover lock is not mistaken for one.
	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("List() = %v, want the deleted record to be gone", tasks)
	}
}
