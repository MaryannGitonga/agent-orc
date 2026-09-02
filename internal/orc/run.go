// Package orc wires the pieces together: it creates a task's worktree, hands
// it to a detached supervisor process, and records what it did.
package orc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Dispatcher launches tasks; build one with [NewDispatcher].
type Dispatcher struct {
	layout paths.Layout
	store  *state.Store
	// self is the agent-orc binary to re-exec as the supervisor.
	self string
	out  io.Writer
}

// NewDispatcher returns a dispatcher writing progress to out.
func NewDispatcher(layout paths.Layout, out io.Writer) (*Dispatcher, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the agent-orc binary: %w", err)
	}
	return &Dispatcher{layout: layout, store: state.NewStore(layout.State), self: self, out: out}, nil
}

// Run prepares a worktree for t and starts a detached supervisor in it. It
// returns as soon as the supervisor is up; the agent keeps going after that.
func (d *Dispatcher) Run(t task.Task) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("invalid task: %w", err)
	}
	if err := d.layout.Ensure(); err != nil {
		return err
	}
	if _, err := d.store.Load(t.ID); err == nil {
		return fmt.Errorf("task %q already exists; pick another --id or run 'agent-orc cleanup %s'", t.ID, t.ID)
	}

	// Check the agent's binary before touching the repository. The supervisor
	// would otherwise fail on exec, after a worktree, a branch and a state file
	// already exist for a task that never had a chance to run.
	argv, err := buildCommand(t)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return fmt.Errorf("task %q needs %s, but %q is not on PATH: %w", t.ID, t.CLI, argv[0], err)
	}

	repo, err := gitx.Open(t.Repo)
	if err != nil {
		return err
	}
	if !repo.RevExists(t.BaseBranch) {
		return fmt.Errorf("base branch %q does not exist in %s", t.BaseBranch, repo.Dir)
	}
	if repo.BranchExists(t.Branch) {
		return fmt.Errorf("branch %q already exists in %s; pick another --branch", t.Branch, repo.Dir)
	}

	worktree := d.layout.Worktree(t.ID)
	if _, err := os.Stat(worktree); err == nil {
		return fmt.Errorf("worktree %s already exists; run 'agent-orc cleanup %s' first", worktree, t.ID)
	}
	if err := repo.AddWorktree(worktree, t.Branch, t.BaseBranch); err != nil {
		return err
	}

	record := state.Task{
		Task:      t,
		Status:    state.StatusPending,
		Worktree:  worktree,
		LogPath:   d.layout.LogFile(t.ID),
		StartedAt: time.Now().UTC(),
	}
	if err := d.store.Save(record); err != nil {
		// Nothing is running yet, so undo the worktree rather than orphan it.
		_ = repo.RemoveWorktree(worktree, true)
		return err
	}

	if err := d.startSupervisor(t.ID); err != nil {
		_ = d.store.Update(t.ID, func(k *state.Task) {
			k.Status = state.StatusFailed
			k.Error = err.Error()
		})
		return err
	}

	fmt.Fprintf(d.out, "%s  started\n", t.ID)
	fmt.Fprintf(d.out, "  branch    %s (from %s)\n", t.Branch, t.BaseBranch)
	fmt.Fprintf(d.out, "  worktree  %s\n", worktree)
	fmt.Fprintf(d.out, "  log       %s\n", record.LogPath)
	return nil
}

// startSupervisor re-execs agent-orc as a detached supervisor. Its own session
// is what lets the agent survive the dispatching terminal going away, so a
// batch can run unattended without a daemon.
func (d *Dispatcher) startSupervisor(id string) error {
	logFile, err := os.OpenFile(d.layout.SupervisorLogFile(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening supervisor log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(d.self, "supervise", id)
	cmd.Dir = filepath.Dir(d.layout.Root)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting supervisor for %q: %w", id, err)
	}
	// The supervisor outlives this process; release it so it is not left a
	// zombie when agent-orc exits immediately afterwards.
	return cmd.Process.Release()
}
