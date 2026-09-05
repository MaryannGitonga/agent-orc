package orc

import (
	"os/exec"
	"syscall"

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
}

// run starts cmd in its own process group, records its pid for as long as it
// runs, and waits for it.
func (t tracker) run(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = t.update(t.id, func(k *state.Task) { k.PID = pid })

	err := cmd.Wait()

	// Clear it only if it is still ours. Anything else means another writer
	// has moved on and this pid is no longer what the record is tracking.
	_ = t.update(t.id, func(k *state.Task) {
		if k.PID == pid {
			k.PID = 0
		}
	})
	return err
}
