package orc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Supervisor drives one task's agent process to completion and records the
// outcome. One detached process per task is what stands in for a daemon.
type Supervisor struct {
	layout paths.Layout
	store  *state.Store
	out    io.Writer
}

// NewSupervisor returns a supervisor logging its own progress to out.
func NewSupervisor(layout paths.Layout, out io.Writer) *Supervisor {
	return &Supervisor{layout: layout, store: state.NewStore(layout.State), out: out}
}

// Supervise runs the agent for the given task and blocks until it exits.
func (s *Supervisor) Supervise(id string) error {
	record, err := s.store.Load(id)
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(record.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return s.fail(id, fmt.Errorf("opening agent log: %w", err))
	}
	defer logFile.Close()

	argv, err := buildCommand(record.Task)
	if err != nil {
		return s.fail(id, err)
	}

	s.logf("launching %s in %s", argv[0], record.Worktree)
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is built by an adapter, not user shell input
	cmd.Dir = record.Worktree
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	// Its own process group, so `agent-orc stop` can signal the agent and
	// everything it spawned. Without this the agent shares the supervisor's
	// group and a group signal would take the supervisor down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return s.fail(id, fmt.Errorf("starting %s: %w", argv[0], err))
	}

	if err := s.store.Update(id, func(k *state.Task) {
		k.Status = state.StatusRunning
		k.PID = cmd.Process.Pid
	}); err != nil {
		s.logf("warning: could not record running state: %v", err)
	}
	s.logf("agent running as pid %d", cmd.Process.Pid)

	runErr := cmd.Wait()
	return s.finish(id, record, cmd, runErr)
}

// finish records the outcome of a completed agent process, including what the
// run actually cost.
func (s *Supervisor) finish(id string, record state.Task, cmd *exec.Cmd, runErr error) error {
	now := time.Now().UTC()
	code := cmd.ProcessState.ExitCode()

	status := state.StatusDone
	message := ""
	if runErr != nil {
		status = state.StatusFailed
		message = runErr.Error()
	}

	usage := s.readUsage(record)

	if err := s.store.Update(id, func(k *state.Task) {
		// A task a human stopped stays stopped; the non-zero exit that came
		// from the signal is not a failure of the agent's own making.
		if k.Status != state.StatusStopped {
			k.Status = status
			k.Error = message
		}
		k.PID = 0
		k.FinishedAt = &now
		k.ExitCode = &code
		if usage != nil {
			k.SpentUSD = usage.CostUSD
			k.Tokens = usage.Tokens
		}
	}); err != nil {
		return err
	}

	s.logf("agent exited with code %d; task is %s", code, status)
	if runErr != nil {
		return fmt.Errorf("task %s failed: %w", id, runErr)
	}
	return nil
}

// readUsage asks the adapter what the run cost. A CLI that reports nothing is
// normal, not an error; the number is simply left unset.
func (s *Supervisor) readUsage(record state.Task) *adapter.Usage {
	a, err := adapter.For(record.CLI)
	if err != nil {
		return nil
	}
	usage, err := a.ParseUsage(record.LogPath)
	if err != nil {
		if !errors.Is(err, adapter.ErrNoUsage) {
			s.logf("warning: could not read usage: %v", err)
		}
		return nil
	}
	return usage
}

// fail records a task that could not be run at all.
func (s *Supervisor) fail(id string, cause error) error {
	now := time.Now().UTC()
	if err := s.store.Update(id, func(k *state.Task) {
		k.Status = state.StatusFailed
		k.PID = 0
		k.FinishedAt = &now
		k.Error = cause.Error()
	}); err != nil {
		return errors.Join(cause, err)
	}
	s.logf("task failed before the agent started: %v", cause)
	return cause
}

func (s *Supervisor) logf(format string, args ...any) {
	fmt.Fprintf(s.out, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// buildCommand returns the argv for a task's CLI.
func buildCommand(t task.Task) ([]string, error) {
	a, err := adapter.For(t.CLI)
	if err != nil {
		return nil, err
	}
	return a.BuildCommand(t), nil
}
