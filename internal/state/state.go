// Package state persists what agent-orc knows about a task as one JSON file
// per task. Files, not a database: there are at most a handful of tasks in
// flight and `cat` is a perfectly good debugger.
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
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Status is where a task is in its lifecycle.
type Status string

// The statuses a task moves through.
const (
	// StatusPending is written before the supervisor has started the agent.
	StatusPending Status = "pending"
	// StatusRunning means the agent process is alive.
	StatusRunning Status = "running"
	// StatusDone means the agent exited successfully.
	StatusDone Status = "done"
	// StatusFailed means the agent exited non-zero or could not be launched.
	StatusFailed Status = "failed"
)

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

// Save writes t, replacing any existing record. The write goes to a temporary
// file first so a reader never sees a half-written record.
func (s *Store) Save(t Task) error {
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

// Update loads a record, applies mutate to it and saves the result.
func (s *Store) Update(id string, mutate func(*Task)) error {
	t, err := s.Load(id)
	if err != nil {
		return err
	}
	mutate(&t)
	return s.Save(t)
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

// Delete removes the record for id. Deleting a task that does not exist is not
// an error — the desired end state is the same either way.
func (s *Store) Delete(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting state for %q: %w", id, err)
	}
	return nil
}
