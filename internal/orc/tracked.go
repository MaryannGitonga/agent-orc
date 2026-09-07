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

// ErrTimedOut means a tracked child was killed for running too long.
var ErrTimedOut = errors.New("timed out")

// tracker runs the children that work on a task's behalf after its agent has
// exited: the test command, the reviewer, and the worker resumed to fix either.
//
// Each gets its own process group, so signalling reaches the build and test
// runner it spawns rather than only the shell, and its pid goes on the record
// while it runs. That pid is the only handle `agent-orc stop` has once the
// agent is gone, and both loops run until they succeed, so without it a hung
// test command hangs the task with nothing to kill.
type tracker struct {
	id string
	// update is the caller's scoped write, so a pid recorded here is dropped
	// if the id has since been dispatched again.
	update func(string, func(*state.Task)) error
	// timeout kills the child if it has not finished by then. Zero is for a
	// child whose own caller ends it; only the test command can hang unnoticed.
	timeout time.Duration
	// warn reports a record that could not be written. Nothing can be done
	// about it, but losing the handle is worth saying.
	warn func(format string, args ...any)
}

func (t tracker) warnf(format string, args ...any) {
	if t.warn != nil {
		t.warn(format, args...)
	}
}

// run starts cmd in its own process group, records its pid for as long as it
// runs, and waits for it.
func (t tracker) run(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := t.update(t.id, func(k *state.Task) { k.PID = pid }); err != nil {
		// The child is already running; killing it to keep the record honest
		// would throw away the work over an unrelated failure.
		t.warnf("warning: could not record the child's pid (%v); 'agent-orc stop' will not reach it", err)
	}

	// The timer and the wait race for the same pid, so they share a lock: once
	// the child is reaped the kernel may hand its number to anything, and a
	// late callback has to find that out rather than signal a stranger.
	// Stopping the timer is not enough, since Stop does not wait for a callback
	// already running.
	//
	// Waiting in a goroutine and selecting against a timer channel looks
	// tidier and is worse: a fired timer leaves its value in the channel, so
	// select takes the timeout branch about half the time for a child that
	// finished on its own. What remains here is the few instructions between
	// wait4 returning and the lock below, which no userspace flag can close;
	// the standard library carries the same residual in Process.Signal.
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
	// Blocks until a callback in flight has finished signalling, so the caller
	// is not told the command merely failed while its group is still dying.
	mu.Lock()
	reaped = true
	killed := timedOut
	mu.Unlock()
	if killed {
		err = fmt.Errorf("%w after %s", ErrTimedOut, t.timeout)
	}

	// Only if it is still ours: another writer having moved on means this pid
	// is no longer what the record tracks.
	if uerr := t.update(t.id, func(k *state.Task) {
		if k.PID == pid {
			k.PID = 0
		}
	}); uerr != nil {
		t.warnf("warning: could not clear the child's pid %d (%v); the record still names it", pid, uerr)
	}
	return err
}
