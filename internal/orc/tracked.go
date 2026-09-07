package orc

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
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

	// The timer and the wait race for the same process, so they share a lock.
	// Once the child has been reaped its pid means nothing and the kernel may
	// hand it to something else, so a timer that fires late has to find out it
	// is too late rather than signal whatever holds that pid now. Stopping the
	// timer on the way out is not enough on its own: Stop does not wait for a
	// callback that has already begun.
	//
	// The obvious alternative, waiting in a goroutine and selecting between the
	// result and a timer channel, is worse rather than better. A timer that has
	// fired leaves its value in the channel, so once both are ready select
	// picks between them at random and takes the timeout branch about half the
	// time for a child that finished on its own. The lock has no such memory: a
	// callback that arrives after the wait finds reaped set and does nothing.
	//
	// What remains is the gap between wait4 returning inside cmd.Wait and the
	// lock being taken on the next line, which is a few instructions and cannot
	// be closed in portable Go, since no userspace flag can be set atomically
	// with the kernel reaping a child. The standard library carries the same
	// residual in Process.Signal after Wait. Nothing is left between those two
	// lines for that reason.
	//
	// The kill covers the whole group, not just the leader: a test command runs
	// a build and a test runner of its own, and killing only the shell would
	// leave those holding the worktree while the task moved on.
	var (
		mu       sync.Mutex
		reaped   bool
		timedOut bool
	)
	if t.timeout > 0 {
		timer := time.AfterFunc(t.timeout, func() {
			mu.Lock()
			defer mu.Unlock()
			if reaped {
				return
			}
			timedOut = true
			_ = signalGroup(pid)
		})
		defer timer.Stop()
	}

	err := cmd.Wait()
	// Blocks until a callback already in flight has finished signalling, which
	// is the point: the group is on its way out and the caller should not be
	// told the command merely failed while that is still happening.
	mu.Lock()
	reaped = true
	killed := timedOut
	mu.Unlock()
	if killed {
		err = fmt.Errorf("%w after %s", ErrTimedOut, t.timeout)
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
