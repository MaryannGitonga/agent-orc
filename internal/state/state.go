// Package state persists each task as one JSON file. Files, not a database:
// there are only ever a handful of tasks and `cat` is a fine debugger.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Status is where a task is in its lifecycle.
type Status string

// The statuses a task moves through. A task is not done until the sanitize,
// push and draft-PR chain that runs after the agent has also finished.
const (
	StatusPending       Status = "pending" // the supervisor has not started the agent
	StatusRunning       Status = "running"
	StatusVerifying     Status = "verifying" // running the task's own test command
	StatusReviewing     Status = "reviewing" // an automatic review round is under way
	StatusPublishing    Status = "publishing"
	StatusDone          Status = "done"
	StatusPublishFailed Status = "publish_failed" // committed but unpublished; retry with `agent-orc pr`
	// StatusFailed covers everything that stopped the task short of publishing:
	// an agent that exited non-zero or never launched, and a verification pass
	// whose suite never went green.
	StatusFailed  Status = "failed"
	StatusStopped Status = "stopped" // killed by `agent-orc stop`
	// StatusReviewed means the reviewer approved the branch, which may have
	// taken several rounds of comments and fixes to get to.
	StatusReviewed Status = "reviewed"
	// StatusReviewFailed means an automatic review could not be completed: the
	// reviewer errored or its verdict was unreadable. The work is committed on
	// its branch either way, and `agent-orc review` retries.
	StatusReviewFailed Status = "review_failed"
	// StatusInterrupted means the supervisor died before finishing, so nothing
	// will move the task on; the work so far is on its branch.
	StatusInterrupted Status = "interrupted"
	// StatusPolicyViolation means the agent did something it was told not to
	// by pushing its branch or opening its own PR, so the change did not go
	// through agent-orc's sanitize-then-draft path.
	StatusPolicyViolation Status = "policy_violation"
)

// Active reports whether work is still in flight for the task.
func (s Status) Active() bool {
	return s == StatusPending || s == StatusRunning ||
		s == StatusVerifying || s == StatusReviewing || s == StatusPublishing
}

// HasProcess reports whether a live process should back this status.
//
// It covers the phases that run after the agent has exited as well as the
// agent itself. Those phases run children of their own, for as long as it
// takes the tests to pass or the reviewer to approve, and both `stop` and the
// reconciliation in `status` key off this: leaving them out is what made a task
// with a hung test command impossible to stop and impossible to correct.
func (s Status) HasProcess() bool {
	return s == StatusPending || s == StatusRunning ||
		s == StatusVerifying || s == StatusReviewing
}

// SchemaVersion is the newest record format this build writes. Raise it for
// any change to the stored fields: an older build rewrites whole records, so it
// would drop even a field it ignores. Extend versionFor with what the new
// version adds.
const SchemaVersion = 2

// versionFor is the oldest format that can hold t. A record is written in it,
// so an older supervisor that still owns the run can keep reading it.
func versionFor(t Task) int {
	if t.RunID != "" {
		return 2
	}
	return 1
}

// Task is the persisted record of one dispatched task.
type Task struct {
	task.Task `yaml:",inline"`

	// SchemaVersion is the format the record was written in; 0 means before
	// versions existed.
	SchemaVersion int `json:"schema_version"`
	// RunID tells this dispatch apart from any other run under the same id.
	RunID string `json:"run_id,omitempty"`

	Status     Status     `json:"status"`
	PID        int        `json:"pid,omitempty"`
	Worktree   string     `json:"worktree"`
	LogPath    string     `json:"log_path"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`

	// SpentUSD and Tokens are what the run actually cost, read out of the
	// CLI's own output after it exits. Nil means the CLI reported nothing.
	SpentUSD *float64 `json:"spent_usd,omitempty"`
	Tokens   *int     `json:"tokens,omitempty"`
	// BudgetNote records any part of the budget the CLI could not enforce,
	// so `status` can say so rather than implying a cap that is not there.
	BudgetNote string `json:"budget_note,omitempty"`
	// SeededAgents lists the subagent definitions copied into the worktree.
	SeededAgents []string `json:"seeded_agents,omitempty"`
	// PRURL is the draft change opened for the branch.
	PRURL string `json:"pr_url,omitempty"`
	// PushedSHA is the commit agent-orc last pushed for this task. It is what
	// distinguishes its own pushed branch from one the agent pushed itself.
	PushedSHA string `json:"pushed_sha,omitempty"`
	// RewrittenCommits counts the commits the sanitization pass changed.
	RewrittenCommits int `json:"rewritten_commits,omitempty"`
	// SessionID identifies the worker's session, so review feedback can be
	// handed back to it rather than starting over.
	SessionID string `json:"session_id,omitempty"`
	// ReviewRound counts completed worker-reviewer round-trips.
	ReviewRound int `json:"review_round"`
	// TestRuns counts how many times the task's test command was run.
	TestRuns int `json:"test_runs,omitempty"`
	// TestsPassed is whether the last of those runs passed. Nil means the
	// suite was never run, which is what a task with no test command gets.
	TestsPassed *bool `json:"tests_passed,omitempty"`
}

// Run identifies one dispatch of a task id.
type Run struct {
	ID        string
	StartedAt time.Time
}

// Run returns the dispatch this record belongs to.
func (t Task) Run() Run { return Run{ID: t.RunID, StartedAt: t.StartedAt} }

// IsZero reports whether r names no run.
func (r Run) IsZero() bool { return r.ID == "" && r.StartedAt.IsZero() }

// Matches reports whether t is still run r. The run id decides when both have
// one; a run from before run ids existed falls back to its start time.
func (r Run) Matches(t Task) bool {
	switch {
	case r.ID != "" && t.RunID != "":
		return r.ID == t.RunID
	default:
		return t.StartedAt.Equal(r.StartedAt)
	}
}

// ErrNotFound is returned when no state file exists for a task ID.
var ErrNotFound = errors.New("no such task")

// ErrNewerSchema means a newer agent-orc wrote the record. It is refused, not
// read, so this build cannot drop fields it does not know when saving it.
var ErrNewerSchema = errors.New("written by a newer agent-orc")

// Unreadable is a record List found but could not read.
type Unreadable struct {
	ID   string
	Path string
	Err  error
}

// Store reads and writes task state under a directory.
type Store struct {
	Dir string
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// lockFile is where a task's lock lives. It is a sibling of the record rather
// than the record itself, so locking never depends on the record existing, and
// it outlives the record: unlinking it would put a caller that opened it before
// the delete and one that opens it after on two different inodes, each holding
// what it thinks is the same lock. The file is empty, List ignores anything
// that is not a .json, and an id reused after cleanup wants the same lock.
//
// So these accumulate, one empty file per task id ever used, and nothing sweeps
// them. A sweep is not a small thing left undone: removing a lock is the unlink
// described above whenever it happens, and doing it from cleanup would put it
// exactly where a redispatch of that id is most likely to be waiting.
func (s *Store) lockFile(id string) string { return filepath.Join(s.Dir, id+".lock") }

// withLock runs fn while holding a task's lock.
//
// A record is read, changed and written back, and the processes doing that are
// separate: one detached supervisor per task, plus whatever the user is typing.
// Without a lock a supervisor can load a record, have cleanup delete it and a
// new dispatch write its own underneath, and then save its stale copy over the
// top. The generation check inside the write is what refuses that, and it can
// only refuse what it sees, so the check and the save have to be one step.
//
// flock is advisory and per open file description, so every caller takes its
// own descriptor and the kernel serializes them, in one process or many.
func (s *Store) withLock(id string, fn func() error) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	f, err := os.OpenFile(s.lockFile(id), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("locking task %q: %w", id, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking task %q: %w", id, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// Save writes t, replacing any existing record except one from a newer
// agent-orc, which it refuses rather than drop that record's unknown fields.
func (s *Store) Save(t Task) error {
	return s.withLock(t.ID, func() error {
		if _, err := s.Load(t.ID); errors.Is(err, ErrNewerSchema) {
			return err
		}
		return s.save(t)
	})
}

// save writes t without taking the lock, for callers that already hold it. The
// write goes to a temporary file first so a reader never sees a half-written
// record, and the file and directory are synced so it also survives a crash.
func (s *Store) save(t Task) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	t.SchemaVersion = versionFor(t)
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state for %q: %w", t.ID, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.Dir, t.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary state file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing state for %q: %w", t.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing state for %q: %w", t.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing state file for %q: %w", t.ID, err)
	}
	if err := os.Rename(tmp.Name(), s.path(t.ID)); err != nil {
		return fmt.Errorf("saving state for %q: %w", t.ID, err)
	}
	// The error is ignored: the rename already saved the record, and returning
	// an error would tell the caller the save failed.
	_ = syncDir(s.Dir)
	return nil
}

// syncDir flushes a directory's entries, so a rename into it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s to sync it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// Load reads the record for id, returning [ErrNotFound] if there is none.
func (s *Store) Load(id string) (Task, error) {
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Task{}, fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("reading state for %q: %w", id, err)
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return Task{}, fmt.Errorf("parsing state for %q: %w", id, err)
	}
	if t.SchemaVersion > SchemaVersion {
		return Task{}, fmt.Errorf("state for %q is schema %d, %w that reads up to %d; upgrade agent-orc",
			id, t.SchemaVersion, ErrNewerSchema, SchemaVersion)
	}
	if t.SchemaVersion < 0 {
		return Task{}, fmt.Errorf("state for %q has an invalid schema_version %d", id, t.SchemaVersion)
	}
	migrate(&t)
	return t, nil
}

// migrate brings an older record up to SchemaVersion, one version at a time.
// Each new version adds a case filling in what it added.
func migrate(t *Task) {
	for t.SchemaVersion < SchemaVersion {
		switch t.SchemaVersion {
		case 0:
			// Unversioned records are the first format.
		case 1:
			// Version 2 added run_id. Older runs have none and are told apart
			// by started_at instead; see Run.Matches.
		}
		t.SchemaVersion++
	}
}

// Update loads a record, applies mutate to it and saves the result, all while
// holding the task's lock.
func (s *Store) Update(id string, mutate func(*Task)) error {
	return s.UpdateIf(id, func(t *Task) bool {
		mutate(t)
		return true
	})
}

// UpdateIf is Update for a change that may decline to happen: mutate reports
// whether it changed anything, and a false leaves the file alone.
//
// Writing back an unchanged copy is not harmless. It is a whole record written
// from a snapshot, so a caller that declined would still stamp what it read
// over anything written in the meantime.
func (s *Store) UpdateIf(id string, mutate func(*Task) bool) error {
	return s.withLock(id, func() error {
		t, err := s.Load(id)
		if err != nil {
			return err
		}
		if !mutate(&t) {
			return nil
		}
		return s.save(t)
	})
}

// List returns every record, oldest first, and separately each one it could
// not read, so one damaged file cannot hide every other task.
func (s *Store) List() ([]Task, []Unreadable, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("listing state directory: %w", err)
	}
	var tasks []Task
	var bad []Unreadable
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		t, err := s.Load(id)
		// Removed between reading the directory and reading the record, which
		// cleanup does all the time. It is one task fewer, not a failed
		// listing: failing would blank a table that is read every second.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			bad = append(bad, Unreadable{ID: id, Path: s.path(id), Err: err})
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].StartedAt.Before(tasks[j].StartedAt) })
	return tasks, bad, nil
}

// SweepTemp removes temporary files left by interrupted saves and returns their
// names. It carries on past a failure, so names and an error can both come back.
//
// Each file is removed under its task's lock, which a save holds until its
// rename, so a save in progress is never touched.
func (s *Store) SweepTemp() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing state directory: %w", err)
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		id := tempOwner(e.Name())
		if e.IsDir() || id == "" {
			continue
		}
		path := filepath.Join(s.Dir, e.Name())
		err := s.withLock(id, func() error {
			if err := os.Remove(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("removing %s: %w", path, err)
			}
			removed = append(removed, e.Name())
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// tempOwner returns the task id in a save's id.<digits>.tmp name, or "" for any
// other file. Ids may hold dots, so it cuts at the last one.
func tempOwner(name string) string {
	base, ok := strings.CutSuffix(name, ".tmp")
	if !ok {
		return ""
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return ""
	}
	for _, r := range base[dot+1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return base[:dot]
}

// Delete removes the record for id. Deleting a missing record is not an error.
func (s *Store) Delete(id string) error {
	_, err := s.DeleteIf(id, func(Task) bool { return true })
	return err
}

// DeleteIf removes the record for id only if want says so, reporting whether it
// did. The record is read and removed under one lock, so what want judges is
// what gets deleted.
//
// Removing a task is not one write: the caller tears down a worktree and a
// branch first, and only then drops the record. An id freed that way can be
// dispatched again, so a caller that decided to delete some time ago has to say
// which run it meant. Deleting a record that is already gone is not an error.
func (s *Store) DeleteIf(id string, want func(Task) bool) (deleted bool, err error) {
	err = s.withLock(id, func() error {
		cur, err := s.Load(id)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case !want(cur):
			return nil
		}
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting state for %q: %w", id, err)
		}
		if cur.RunID != "" {
			_ = os.Remove(s.liveFile(id, cur.RunID))
		}
		deleted = true
		return nil
	})
	return deleted, err
}

// liveFile is where a run's supervisor holds its liveness lock.
func (s *Store) liveFile(id, runID string) string {
	return filepath.Join(s.Dir, id+"."+runID+".live")
}

// ErrAlreadySupervised means another supervisor holds the run's liveness lock.
var ErrAlreadySupervised = errors.New("already has a supervisor")

// HoldRun takes the liveness lock for t's run, for the supervisor to keep until
// it exits. The kernel drops the lock when the process dies, however it dies.
func (s *Store) HoldRun(t Task) (release func(), err error) {
	if t.RunID == "" {
		return func() {}, nil
	}
	var f *os.File
	// Created and locked under the task's lock, so a check made under it never
	// sees the file before it is held.
	err = s.withLock(t.ID, func() error {
		var err error
		if f, err = os.OpenFile(s.liveFile(t.ID, t.RunID), os.O_CREATE|os.O_RDWR, 0o644); err != nil {
			return fmt.Errorf("creating the liveness lock for %q: %w", t.ID, err)
		}
		// Never waits: blocking here would hold the task's lock against the
		// supervisor already running. Retried briefly, since a check holds a
		// shared lock for an instant.
		for attempt := 0; ; attempt++ {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			switch {
			case err == nil:
				return nil
			case !errors.Is(err, syscall.EWOULDBLOCK):
				f.Close()
				return fmt.Errorf("taking the liveness lock for %q: %w", t.ID, err)
			case attempt == 9:
				f.Close()
				return fmt.Errorf("task %q %w", t.ID, ErrAlreadySupervised)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}

// SupervisorAlive reports whether t's supervisor still holds its liveness lock,
// and whether that can be known: not for a run from before run ids, nor for one
// whose supervisor has not started yet.
func (s *Store) SupervisorAlive(t Task) (alive, known bool) {
	if t.RunID == "" {
		return false, false
	}
	f, err := os.Open(s.liveFile(t.ID, t.RunID))
	if err != nil {
		return false, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, true
}
