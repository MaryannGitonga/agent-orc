package orc

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// Stop kills a running task's agent process and records it as stopped.
//
// The worktree and branch are left alone: whatever the agent committed before
// being stopped is still there to look at, and `agent-orc cleanup` is the
// command that throws work away.
func (r *Reporter) Stop(id string) error {
	t, err := r.store.Load(id)
	if err != nil {
		return err
	}
	if !t.Status.HasProcess() {
		return fmt.Errorf("task %q is %s, not running", id, t.Status)
	}
	// Between commands in one of the phases that run after the agent: there is
	// nothing to signal this instant, but the loop is about to start the next
	// test run or review round, and telling the user to try again in a moment
	// is telling them to race it. Recording the stop is what actually ends it,
	// since both loops check for one before starting anything else.
	betweenCommands := t.PID == 0 &&
		(t.Status == state.StatusVerifying || t.Status == state.StatusReviewing)
	if t.PID == 0 && !betweenCommands {
		return fmt.Errorf("task %q has no recorded process; it may not have started yet", id)
	}
	// Record the stop before signalling, not after. The supervisor sits in
	// cmd.Wait() until the agent dies, so it cannot start its own state write
	// until the signal lands; writing first is what guarantees it observes
	// "stopped" and leaves it alone. Signalling first races its write against
	// this one, and when it loses the task is reported as a failure.
	now := time.Now().UTC()
	if err := r.store.Update(id, func(k *state.Task) {
		k.Status = state.StatusStopped
		k.FinishedAt = &now
		k.Error = "stopped by agent-orc stop"
	}); err != nil {
		return err
	}
	if betweenCommands {
		fmt.Fprintf(r.out, "%s  stopped between commands; nothing was running to signal\n", id)
		return nil
	}
	if err := signalGroup(t.PID); err != nil {
		// ESRCH is the process already being gone, which is the end state the
		// caller asked for. Rolling back here would re-assert a running record
		// for a process that does not exist, which is what `status` then has to
		// reconcile away.
		if errors.Is(err, syscall.ESRCH) {
			_ = r.store.Update(id, func(k *state.Task) {
				k.Error = "process was already gone when stop ran"
			})
			fmt.Fprintf(r.out, "%s  was already gone; recorded as stopped\n", id)
			return nil
		}
		// Any other failure is a live process this could not signal, so the
		// task is still running and the record must say so again.
		_ = r.store.Update(id, func(k *state.Task) {
			k.Status = t.Status
			k.FinishedAt = t.FinishedAt
			k.Error = t.Error
		})
		return fmt.Errorf("stopping task %q (pid %d): %w", id, t.PID, err)
	}
	fmt.Fprintf(r.out, "%s  stopped (pid %d); worktree %s left in place\n", id, t.PID, t.Worktree)
	return nil
}

// stopGrace is how long a process is given to exit on SIGTERM before it is
// killed outright. Long enough for an agent to finish the write it is in the
// middle of, short enough that `agent-orc stop` still feels like a command
// rather than a wait.
const stopGrace = 5 * time.Second

// killGrace is how long the group is given to disappear after SIGKILL, which
// it cannot refuse. It is short because the only thing that outlasts it is a
// process stuck in the kernel, which no amount of waiting will fix.
const killGrace = 2 * time.Second

// signalGroup terminates the agent and everything it spawned, and makes sure it
// is actually gone.
//
// An agent runs compilers, test runners and git of its own, and signalling only
// the leader would orphan them. It is started with Setpgid, so the negative pid
// addresses exactly that process's descendants and nothing else, and nothing
// here ever signals a bare pid: see signal for why.
//
// SIGTERM is a request, and a process is free to ignore it. Returning as soon
// as it was sent would record a task as stopped while it carried on running, so
// this waits for the process to go and escalates to SIGKILL if it does not.
func signalGroup(pid int) error {
	if err := signal(pid, syscall.SIGTERM); err != nil {
		return err
	}
	if waitForGroup(pid, stopGrace) {
		return nil
	}
	// Not going to honour the request. SIGKILL cannot be ignored, and ESRCH
	// from here is the group having gone in the meantime, which is the outcome
	// that was wanted.
	if err := signal(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// Confirm it, rather than assuming: a signal is delivered asynchronously,
	// and returning the moment it was sent would let the caller record a task
	// as stopped while its processes were still winding down. Only something
	// stuck in the kernel survives this, and saying so beats claiming success.
	if !waitForGroup(pid, killGrace) {
		return fmt.Errorf("process group %d is still running after SIGKILL", pid)
	}
	return nil
}

// waitForGroup waits up to d for every process in the group to go, and reports
// whether they did.
func waitForGroup(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !groupAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// groupAlive reports whether anything in the process group is still there.
//
// Asking only about the leader is not the same question: an agent's test runner
// or compiler is in the same group, and one of those outliving a leader that
// exited on SIGTERM would end the wait early and never be escalated to, leaving
// it running after the task is recorded as stopped.
//
// ESRCH means the group is gone, and that is the end of it. Falling back to the
// leader's own pid would be asking about whoever holds that number now: a pid
// is reused as soon as its process is reaped, so a record left stale by a
// missed reconcile would have this wait on, and then SIGKILL, something with no
// connection to the task.
func groupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signal sends sig to a process group, and only ever to a group.
//
// Every pid agent-orc records belongs to a process it started with Setpgid, so
// the group always exists while the process does, and it has been that way
// since before the first release: there are no records to be compatible with
// that lack one. Signalling a bare pid as a fallback would therefore never help
// a real record, and would reach an unrelated process holding a reused number.
func signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
