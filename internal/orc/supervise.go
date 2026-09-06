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
	// startedAt identifies the run this supervisor was launched for, so its
	// writes can be told from those of a later task reusing the same id.
	startedAt time.Time
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
	s.startedAt = record.StartedAt

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

	if err := s.update(id, func(k *state.Task) {
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
	// A gate that is about to run is what the task is doing, and has to be
	// recorded before it starts rather than after. Otherwise the window where
	// the suite is running, or the reviewer is, reads as "done" to anyone
	// looking, on a branch that may still end up failing or being changed.
	case record.TestCommand != "":
		status = state.StatusVerifying
	case record.Review.Enabled && record.Review.Auto:
		status = state.StatusReviewing
	case record.AutoPR:
		status = state.StatusPublishing
	}

	usage := s.readUsage(record)
	sessionID := s.readSessionID(record)

	if err := s.update(id, func(k *state.Task) {
		// A task a human stopped stays stopped; the non-zero exit that came
		// from the signal is not a failure of the agent's own making.
		if k.Status != state.StatusStopped {
			k.Status = status
			k.Error = message
		}
		k.PID = 0
		k.ExitCode = &code
		// Only once the task is actually finished. The gates below run after
		// the agent exits and can take as long as the tests and the reviewer
		// need, so stamping this here would freeze the elapsed time `status`
		// reports at the moment the agent stopped and hide all of it.
		if !k.Status.Active() {
			k.FinishedAt = &now
		}
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

	// The gates below are what make the window between an agent exiting and its
	// branch being published a long one: both loops run until they succeed, so
	// this can be minutes or hours rather than the few statements it used to
	// be. That is long enough for the task to be cleaned up and its id
	// dispatched again, and every round of either gate costs money, so ask
	// before starting rather than only before publishing.
	if !s.owns(id) {
		s.logf("this task's id now belongs to a later run; stopping here")
		return nil
	}

	// Tests first, then review, then publish. Each gate is there to stop the
	// next one being wasted: no point paying a reviewer to read a branch whose
	// suite is red, and no point opening a PR over a branch the review is
	// still changing.
	if err := s.verify(record); err != nil {
		s.logf("not publishing: %v", err)
		s.failUnlessStopped(id, err)
		return nil
	}
	if record.Review.Enabled && record.Review.Auto {
		s.mark(id, state.StatusReviewing, "")
		if err := s.autoReview(&record); err != nil {
			s.logf("not publishing: %v", err)
			if s.wasStopped(id) {
				s.logf("task was stopped during review: %v", err)
			} else {
				s.mark(id, state.StatusReviewFailed, err.Error())
			}
			return nil
		}
	}

	if !record.AutoPR {
		s.logf("auto_pr is off; run 'agent-orc pr %s' when you want the draft opened", id)
		s.mark(id, state.StatusDone, "")
		return nil
	}
	s.mark(id, state.StatusPublishing, "")
	s.publish(id, record)
	return nil
}

// autoReview runs the review pass inline, before the branch is published, when
// the task asked for it. Doing it here rather than leaving it to a later
// `agent-orc review` is the point of the flag: the draft PR that opens has
// already been through a round, instead of being opened and then changed under
// whoever started reading it.
func (s *Supervisor) autoReview(record *state.Task) error {
	r := NewReviewer(s.layout, s.out)
	r.OwnRun(s.startedAt)
	approved, err := r.Rounds(record)
	if err != nil {
		return err
	}
	if approved {
		s.logf("the reviewer approved the branch after %d round(s)", record.ReviewRound)
		return nil
	}
	// Not an error, and no longer a cap: the loop ends without an approval only
	// when the worker stopped acting on the comments, which is as far as an
	// automatic round can get on its own. The work is still worth publishing
	// for a human to pick up, so say why it stopped and carry on.
	s.logf("the review ended without an approval because the worker stopped changing anything; publishing anyway")
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
	// Unlike the writes above, this is not covered by scoping the publisher:
	// Publish loads the record itself, so the guard would drop its state writes
	// while it had already sanitized, force-pushed and opened a pull request
	// against whatever branch the id names now. Checking ownership before
	// starting is the only place that catches it.
	if !s.owns(id) {
		s.logf("this task's id now belongs to a later run; not publishing")
		return
	}
	p, err := NewPublisher(s.layout, s.out)
	if err == nil {
		p.OwnRun(s.startedAt)
		err = p.Publish(id)
	}
	if err == nil {
		s.logf("task %s is done", id)
		s.mark(id, state.StatusDone, "")
		return
	}
	if errors.Is(err, ErrRunReplaced) {
		// The id was dispatched again between the check above and the load
		// inside Publish. Nothing was done, and nothing about the run that now
		// owns the id is this supervisor's to record.
		s.logf("this task's id now belongs to a later run; nothing was published")
		return
	}
	if errors.Is(err, ErrNoRemote) {
		// A local-only repository is a legitimate way to work, not a failure.
		s.logf("no %s remote; the work is sanitized and on %s, and was not pushed", defaultRemote, record.Branch)
		s.mark(id, state.StatusDone, "")
		return
	}
	if errors.Is(err, ErrNothingToPublish) {
		// Not a publish failure: nothing was attempted, because there was
		// nothing to attempt it with. Still not done, because the PR a human is
		// waiting on is never going to arrive.
		s.logf("the agent committed nothing; %s is empty and no PR was opened", record.Branch)
		s.mark(id, state.StatusPublishFailed, err.Error())
		return
	}
	if errors.Is(err, ErrNoCommits) {
		// Nothing to sanitize and nothing to publish. Say so, rather than
		// reporting work on a branch that does not have any.
		s.logf("the agent committed nothing; %s is empty", record.Branch)
		s.mark(id, state.StatusDone, "")
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

// update applies mutate only while the stored record is still the run this
// supervisor was started for.
//
// An id becomes reusable the moment its task is cleaned up, and cleanup does
// not wait for the supervisor: `stop` signals the agent and returns without
// waiting for it to die, and a forced cleanup does not look at the process at
// all. So a supervisor can still be inside its final write when a new task is
// dispatched under the same id, and a blind store.Update would then stamp the
// old run's status, exit code and a zero pid onto the new one, leaving a task
// that is running but recorded as finished and cannot be stopped.
//
// StartedAt is the generation marker: it is set once per dispatch and never
// changes afterwards, so a mismatch means this supervisor no longer owns the
// id and its write is dropped.
func (s *Supervisor) update(id string, mutate func(*state.Task)) error {
	stale := false
	err := s.store.Update(id, func(k *state.Task) {
		if !k.StartedAt.Equal(s.startedAt) {
			stale = true
			return
		}
		mutate(k)
	})
	if stale {
		s.logf("this task's id now belongs to a later run; not recording anything against it")
	}
	return err
}

// owns reports whether the id still belongs to the run this supervisor was
// started for. A task can be cleaned up and its id dispatched again while this
// supervisor is still working, and anything that acts on the id rather than on
// the record it already holds has to ask first.
func (s *Supervisor) owns(id string) bool {
	current, err := s.store.Load(id)
	return err == nil && current.StartedAt.Equal(s.startedAt)
}

// wasStopped reports whether a human has stopped the task since it was loaded.
// The phases that run after the agent exits are long, and a stop lands as a
// killed child rather than as anything the loop can see for itself.
func (s *Supervisor) wasStopped(id string) bool {
	current, err := s.store.Load(id)
	return err == nil && current.Status == state.StatusStopped
}

// failUnlessStopped records a phase's failure, unless a human stopped the task,
// in which case the stop is the reason it failed and stays the record of it.
func (s *Supervisor) failUnlessStopped(id string, cause error) {
	if s.wasStopped(id) {
		s.logf("task was stopped: %v", cause)
		return
	}
	s.mark(id, state.StatusFailed, cause.Error())
}

// mark sets a task's terminal status.
//
// Every caller logs before calling this, never after. The state file is what
// anyone waiting on the task reads, so it has to be the last thing a supervisor
// writes: a status observed while the supervisor still had a line to write
// means the watcher can act, and delete the very directory being written to,
// before the process has finished with it.
func (s *Supervisor) mark(id string, status state.Status, message string) {
	now := time.Now().UTC()
	if err := s.update(id, func(k *state.Task) {
		k.Status = status
		// A phase that is still working has no finish time yet, and a task that
		// has one finished when this said so, not when its agent did.
		if status.Active() {
			k.FinishedAt = nil
		} else {
			k.FinishedAt = &now
		}
		// Assigned either way, so a status carrying no message clears whatever
		// the last one left. A task that reached done has no error, and saying
		// it does is worse than saying nothing.
		k.Error = message
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
	s.logf("task failed before the agent started: %v", cause)
	now := time.Now().UTC()
	if err := s.update(id, func(k *state.Task) {
		k.Status = state.StatusFailed
		k.PID = 0
		k.FinishedAt = &now
		k.Error = cause.Error()
	}); err != nil {
		return errors.Join(cause, err)
	}
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
