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
	if !t.Status.Active() {
		return fmt.Errorf("task %q is %s, not running", id, t.Status)
	}
	if t.PID == 0 {
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
	if err := syscall.Kill(t.PID, syscall.SIGTERM); err != nil {
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
