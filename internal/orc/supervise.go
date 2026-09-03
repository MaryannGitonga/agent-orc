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

	argv, err := buildCommand(record.Task, record.SessionID)
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

	// The task is not done until the publish chain has run: §12's whole point
	// is that nothing reaches the remote unsanitized, so "done" has to mean
	// "sanitized, pushed and open as a draft", not "the agent stopped".
	status := state.StatusDone
	message := ""
	switch {
	case runErr != nil:
		status = state.StatusFailed
		message = runErr.Error()
	case record.AutoPR:
		status = state.StatusPublishing
	}

	usage := s.readUsage(record)
	sessionID := s.readSessionID(record)

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
		if sessionID != "" {
			k.SessionID = sessionID
		}
	}); err != nil {
		return err
	}

	s.logf("agent exited with code %d; task is %s", code, status)
	if runErr != nil {
		return fmt.Errorf("task %s failed: %w", id, runErr)
	}
	if status == state.StatusPublishing {
		s.publish(id, record)
	} else {
		s.logf("auto_pr is off; run 'agent-orc pr %s' when you want the draft opened", id)
	}
	return nil
}

// publish chains the sanitize, push and draft-PR pass onto the same per-task
// process, the moment the agent exits. This is what makes "automatic on
// completion" work with no daemon: the automation hangs off a process that was
// already running for this task.
//
// A publish failure does not fail the task: the agent's work is committed and
// on its branch either way. It is logged and left for `agent-orc pr` to retry.
func (s *Supervisor) publish(id string, record state.Task) {
	p, err := NewPublisher(s.layout, s.out)
	if err == nil {
		err = p.Publish(id)
	}
	if err == nil {
		s.mark(id, state.StatusDone, "")
		s.logf("task %s is done", id)
		return
	}
	if errors.Is(err, ErrNoRemote) {
		// A local-only repository is a legitimate way to work, not a failure.
		s.mark(id, state.StatusDone, "")
		s.logf("no %s remote; the work is sanitized and on %s, and was not pushed", defaultRemote, record.Branch)
		return
	}
	if errors.Is(err, ErrNothingToPublish) {
		// Not a publish failure: nothing was attempted, because there was
		// nothing to attempt it with. Still not done, because the PR a human is
		// waiting on is never going to arrive.
		s.mark(id, state.StatusPublishFailed, err.Error())
		s.logf("the agent committed nothing; %s is empty and no PR was opened", record.Branch)
		return
	}
	if errors.Is(err, ErrNoCommits) {
		// Nothing to sanitize and nothing to publish. Say so, rather than
		// reporting work on a branch that does not have any.
		s.mark(id, state.StatusDone, "")
		s.logf("the agent committed nothing; %s is empty", record.Branch)
		return
	}

	s.logf("publish failed: %v", err)
	s.logf("the work is committed on %s; retry with 'agent-orc pr %s'", record.Branch, id)
	// Publish may already have recorded something more specific, a policy
	// violation say, and that diagnosis should not be overwritten.
	if current, loadErr := s.store.Load(id); loadErr == nil && current.Status != state.StatusPublishing {
		return
	}
	s.mark(id, state.StatusPublishFailed, err.Error())
}

// mark sets a task's terminal status.
func (s *Supervisor) mark(id string, status state.Status, message string) {
	if err := s.store.Update(id, func(k *state.Task) {
		k.Status = status
		if message != "" {
			k.Error = message
		}
	}); err != nil {
		s.logf("warning: could not record status %s: %v", status, err)
	}
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

// readSessionID falls back to whatever the CLI wrote about its own session,
// for the ones that will not accept an ID at launch.
func (s *Supervisor) readSessionID(record state.Task) string {
	if record.SessionID != "" {
		return record.SessionID
	}
	a, err := adapter.For(record.CLI)
	if err != nil {
		return ""
	}
	id, err := a.ParseSessionID(record.LogPath)
	if err != nil {
		return ""
	}
	return id
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

// agentBinary returns the executable a task's CLI runs as, so a caller can
// check it exists before committing to any on-disk work.
func agentBinary(t task.Task) (string, error) {
	a, err := adapter.For(t.CLI)
	if err != nil {
		return "", err
	}
	argv := a.BuildCommand(t)
	if len(argv) == 0 {
		return "", fmt.Errorf("the %s adapter produced an empty command", t.CLI)
	}
	return argv[0], nil
}

// buildCommand returns the argv for a task's CLI, pinned to the session ID
// agent-orc assigned so the session can be resumed later.
func buildCommand(t task.Task, sessionID string) ([]string, error) {
	a, err := adapter.For(t.CLI)
	if err != nil {
		return nil, err
	}
	argv := append(a.BuildCommand(t), a.SessionArgs(sessionID)...)
	// Supervise runs from persisted state, so it can be reached with an adapter
	// that agentBinary never vetted at dispatch. Fail with a message rather
	// than panicking on argv[0].
	if len(argv) == 0 {
		return nil, fmt.Errorf("the %s adapter produced an empty command", t.CLI)
	}
	return argv, nil
}
