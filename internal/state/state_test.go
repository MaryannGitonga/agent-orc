package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

	got, _, err := s.List()
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
	got, _, err := s.List()
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
	tasks, _, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("List() = %v, want the deleted record to be gone", tasks)
	}
}

// TestDeleteIfOnlyRemovesTheRunItWasAskedFor covers the tail of a cleanup: the
// caller decided to delete some time ago, and by now the id may have been
// dispatched again.
func TestDeleteIfOnlyRemovesTheRunItWasAskedFor(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	firstRun := time.Now().UTC().Add(-time.Hour)
	secondRun := time.Now().UTC()
	wantFirstRun := func(cur Task) bool { return cur.StartedAt.Equal(firstRun) }

	// Nothing there at all is not an error, the way an already-cleaned task is.
	switch deleted, err := store.DeleteIf("GEN-1", wantFirstRun); {
	case err != nil:
		t.Fatal(err)
	case deleted:
		t.Error("DeleteIf() reported deleting a record that was never there")
	}

	// The id has been taken by a newer run, so the old cleanup keeps its hands off.
	if err := store.Save(Task{
		Task:      task.Task{ID: "GEN-1", CLI: task.CLIClaude},
		Status:    StatusRunning,
		StartedAt: secondRun,
	}); err != nil {
		t.Fatal(err)
	}
	switch deleted, err := store.DeleteIf("GEN-1", wantFirstRun); {
	case err != nil:
		t.Fatal(err)
	case deleted:
		t.Error("DeleteIf() removed a newer run's record")
	}
	if got, err := store.Load("GEN-1"); err != nil || !got.StartedAt.Equal(secondRun) {
		t.Errorf("Load() = %v, %v; want the newer run untouched", got.StartedAt, err)
	}

	// The run it was asked for does go.
	switch deleted, err := store.DeleteIf("GEN-1", func(cur Task) bool {
		return cur.StartedAt.Equal(secondRun)
	}); {
	case err != nil:
		t.Fatal(err)
	case !deleted:
		t.Error("DeleteIf() left the run it was asked to remove")
	}
	if _, err := store.Load("GEN-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load() = %v, want ErrNotFound", err)
	}
}

// TestListSkipsARecordThatVanishedMidListing covers a directory read and the
// loads that follow it not being one atomic act. Cleanup removes records all
// the time, and failing the whole listing over one that went would blank a
// table that is read every second.
func TestListSkipsARecordThatVanishedMidListing(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(Task{
		Task:      task.Task{ID: "KEPT-1", CLI: task.CLIClaude},
		Status:    StatusRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A dangling link stands in for the record that was there when the
	// directory was read and gone by the time it was opened.
	if err := os.Symlink(filepath.Join(dir, "no-such-file.json"), filepath.Join(dir, "GONE-1.json")); err != nil {
		t.Fatal(err)
	}

	tasks, _, err := store.List()
	if err != nil {
		t.Fatalf("List() = %v, want the missing record skipped", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "KEPT-1" {
		t.Errorf("List() = %v, want only KEPT-1", tasks)
	}
}

// TestListReportsUnreadableRecordsAndKeepsTheRest covers one damaged file no
// longer hiding every other task.
func TestListReportsUnreadableRecordsAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(record("GOOD-1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, dir, "TORN-1", `{"id": "TORN-1", "stat`)
	writeRecord(t, dir, "NEWER-1", `{"id": "NEWER-1", "schema_version": 99}`)

	tasks, bad, err := store.List()
	if err != nil {
		t.Fatalf("List() = %v, want the bad records reported, not a failure", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "GOOD-1" {
		t.Errorf("tasks = %v, want only GOOD-1", tasks)
	}
	got := map[string]Unreadable{}
	for _, u := range bad {
		got[u.ID] = u
	}
	if len(bad) != 2 || got["TORN-1"].Err == nil || !errors.Is(got["NEWER-1"].Err, ErrNewerSchema) {
		t.Fatalf("unreadable = %v, want TORN-1 and NEWER-1 with their reasons", bad)
	}
	if want := filepath.Join(dir, "TORN-1.json"); got["TORN-1"].Path != want {
		t.Errorf("path = %q, want %q, so the user knows which file to fix", got["TORN-1"].Path, want)
	}
}

func TestSaveStampsTheSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	r := record("V-1", time.Now().UTC())
	r.SchemaVersion = 0
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "V-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"schema_version": 1`)) {
		t.Errorf("record = %s, want it to carry schema_version 1", data)
	}
}

func TestLoadReadsARecordWithoutAVersionAsTheFirst(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "OLD-1", `{"id": "OLD-1", "status": "done"}`)
	got, err := NewStore(dir).Load("OLD-1")
	if err != nil {
		t.Fatalf("Load() = %v, want a record from before versions to load", err)
	}
	if got.SchemaVersion != 1 || got.Status != StatusDone {
		t.Errorf("record = schema %d, %s, want schema 1, done", got.SchemaVersion, got.Status)
	}
}

// TestUpdateLeavesANewerRecordAlone covers why a newer record is refused:
// rewriting it would drop the fields this build does not know.
func TestUpdateLeavesANewerRecordAlone(t *testing.T) {
	dir := t.TempDir()
	const newer = `{"id": "NEW-1", "schema_version": 2, "status": "running", "a_later_field": "kept"}`
	writeRecord(t, dir, "NEW-1", newer)

	err := NewStore(dir).Update("NEW-1", func(k *Task) { k.Status = StatusStopped })
	if !errors.Is(err, ErrNewerSchema) || !strings.Contains(err.Error(), "upgrade agent-orc") {
		t.Fatalf("Update() = %v, want a refusal saying to upgrade", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "NEW-1.json")); string(data) != newer {
		t.Errorf("record = %s, want it untouched", data)
	}
}

func TestSweepTempRemovesOnlyAbandonedSaves(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(record("KEEP.1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	// Named the way save names them, for an id that holds a dot itself.
	abandoned, err := os.CreateTemp(dir, "KEEP.1.*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	abandoned.Close()
	writeFile(t, filepath.Join(dir, "notes.tmp"), "not a save")

	removed, err := store.SweepTemp()
	if err != nil {
		t.Fatalf("SweepTemp() = %v", err)
	}
	if len(removed) != 1 || removed[0] != filepath.Base(abandoned.Name()) {
		t.Errorf("removed = %v, want only %s", removed, filepath.Base(abandoned.Name()))
	}
	for _, kept := range []string{"KEEP.1.json", "notes.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was swept: %v", kept, err)
		}
	}
}

// TestSweepTempWaitsForASaveInProgress covers the sweep taking the task's lock
// before removing a temporary file, so it never races a save.
func TestSweepTempWaitsForASaveInProgress(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	lock, err := os.OpenFile(filepath.Join(dir, "BUSY-1.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	inProgress, err := os.CreateTemp(dir, "BUSY-1.*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	inProgress.Close()

	done := make(chan []string, 1)
	go func() {
		removed, _ := store.SweepTemp()
		done <- removed
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(inProgress.Name()); err != nil {
		t.Fatalf("the sweep removed a file while its save held the lock: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case removed := <-done:
		if len(removed) != 1 {
			t.Errorf("removed = %v, want the file once its save was gone", removed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep never finished after the lock was released")
	}
}

func TestTempOwner(t *testing.T) {
	for name, want := range map[string]string{
		"PROJ-1.123456.tmp": "PROJ-1",
		"a.b.c.42.tmp":      "a.b.c",
		"PROJ-1.json":       "",
		"notes.tmp":         "",
		".42.tmp":           "",
		"PROJ-1..tmp":       "",
		"PROJ-1.12a.tmp":    "",
	} {
		if got := tempOwner(name); got != want {
			t.Errorf("tempOwner(%q) = %q, want %q", name, got, want)
		}
	}
}

func writeRecord(t *testing.T, dir, id, body string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, id+".json"), body)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDirReportsAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if err := syncDir(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("syncDir() = %v, want an error naming %s", err, missing)
	}
}

func TestListAndSweepReportAStateDirTheyCannotRead(t *testing.T) {
	// A file where the directory should be.
	dir := filepath.Join(t.TempDir(), "state")
	writeFile(t, dir, "")
	store := NewStore(dir)
	if _, _, err := store.List(); err == nil {
		t.Error("List() = nil, want an error for a state directory it cannot read")
	}
	if _, err := store.SweepTemp(); err == nil {
		t.Error("SweepTemp() = nil, want an error for a state directory it cannot read")
	}
}

// TestSweepTempCarriesOnPastAFileItCannotRemove covers the sweep reporting
// every failure rather than stopping at the first.
func TestSweepTempCarriesOnPastAFileItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	dir := t.TempDir()
	var names []string
	for _, id := range []string{"A-1", "B-1"} {
		// The locks exist already, so only the removals fail.
		writeFile(t, filepath.Join(dir, id+".lock"), "")
		f, err := os.CreateTemp(dir, id+".*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		names = append(names, filepath.Base(f.Name()))
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	removed, err := NewStore(dir).SweepTemp()
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing from a read-only directory", removed)
	}
	for _, name := range names {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("SweepTemp() = %v, want it to name %s", err, name)
		}
	}
}
