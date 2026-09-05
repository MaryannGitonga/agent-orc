package orc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// tracker runs the children that work on a task's behalf after its agent has
// exited: the test command, the reviewer, and the worker resumed to fix either.
//
// Two things have to be true of those children that were not true before. They
// need their own process group, because a test command runs a build and a test
// runner of its own and signalling only the leader would orphan them, exactly
// as it would for the agent. And the pid has to be on the record while it runs,
// because that is the only handle `agent-orc stop` has: the agent's pid is long
// gone by this point, and without a live one a task in these phases cannot be
// stopped at all. Both loops run until they succeed, so that is not a small
// gap: a test command that hangs would hang the task with nothing to kill.
type tracker struct {
	id string
	// update is the owning caller's guarded write, so a pid recorded here is
	// dropped if the id has since been dispatched again.
	update func(string, func(*state.Task)) error
	// timeout kills the child if it has not finished by then. Zero leaves it
	// alone, which is right for a child whose own caller ends it: only the
	// test command can hang in a way nothing else would ever notice.
	timeout time.Duration
}

// ErrTimedOut means a tracked child was killed for running too long.
var ErrTimedOut = errors.New("timed out")

// run starts cmd in its own process group, records its pid for as long as it
// runs, and waits for it.
func (t tracker) run(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = t.update(t.id, func(k *state.Task) { k.PID = pid })

	// The whole group, not just the leader: a test command runs a build and a
	// test runner of its own, and killing only the shell would leave those
	// holding the worktree while the task moved on. signalGroup asks first and
	// escalates, which is what the same timer would have to do anyway.
	timedOut := make(chan struct{})
	if t.timeout > 0 {
		timer := time.AfterFunc(t.timeout, func() {
			close(timedOut)
			_ = signalGroup(pid)
		})
		defer timer.Stop()
	}

	err := cmd.Wait()
	select {
	case <-timedOut:
		err = fmt.Errorf("%w after %s", ErrTimedOut, t.timeout)
	default:
	}

	// Clear it only if it is still ours. Anything else means another writer
	// has moved on and this pid is no longer what the record is tracking.
	_ = t.update(t.id, func(k *state.Task) {
		if k.PID == pid {
			k.PID = 0
		}
	})
	return err
}
