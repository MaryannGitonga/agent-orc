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

// Task is the persisted record of one dispatched task.
type Task struct {
	task.Task `yaml:",inline"`

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

// ErrNotFound is returned when no state file exists for a task ID.
var ErrNotFound = errors.New("no such task")

// Store reads and writes task state under a directory.
type Store struct {
	Dir string
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// lockFile is where a task's lock lives. It is a sibling of the record rather
// than the record itself, so locking never depends on the record existing.
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

// Save writes t, replacing any existing record.
func (s *Store) Save(t Task) error {
	return s.withLock(t.ID, func() error { return s.save(t) })
}

// save writes t without taking the lock, for callers that already hold it. The
// write goes to a temporary file first so a reader never sees a half-written
// record.
func (s *Store) save(t Task) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing state file for %q: %w", t.ID, err)
	}
	if err := os.Rename(tmp.Name(), s.path(t.ID)); err != nil {
		return fmt.Errorf("saving state for %q: %w", t.ID, err)
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
	return t, nil
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

// List returns every record in the store, ordered by start time, newest last.
func (s *Store) List() ([]Task, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing state directory: %w", err)
	}
	var tasks []Task
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		t, err := s.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].StartedAt.Before(tasks[j].StartedAt) })
	return tasks, nil
}

// Delete removes the record for id. Deleting a missing record is not an error.
func (s *Store) Delete(id string) error {
	err := s.withLock(id, func() error {
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting state for %q: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The lock outlives the record it guarded, so it goes too. Any holder
	// still has it open, and unlinking does not disturb them. A caller that
	// opens the file after this creates a new one, so a delete landing between
	// another caller's open and its flock leaves the two on separate inodes.
	// The lock serializes the common case; the generation check in the write
	// is what actually refuses a lost update.
	if err := os.Remove(s.lockFile(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the lock for %q: %w", id, err)
	}
	return nil
}
