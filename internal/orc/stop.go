package orc

import (
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
	if err := syscall.Kill(t.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stopping task %q (pid %d): %w", id, t.PID, err)
	}

	now := time.Now().UTC()
	if err := r.store.Update(id, func(k *state.Task) {
		k.Status = state.StatusStopped
		k.FinishedAt = &now
		k.Error = "stopped by agent-orc stop"
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s  stopped (pid %d); worktree %s left in place\n", id, t.PID, t.Worktree)
	return nil
}
