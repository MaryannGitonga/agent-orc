// Package bounded runs tool commands such as git and gh with a time limit, so a
// hung one cannot hold a task forever. Agent CLIs are deliberately not run
// through it: their run time is the work.
package bounded

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// Limit is how long a tool command may run. It is generous because it exists
// to end a hang, such as a push stalled on the network or a signature waiting
// on a pinentry nobody sees, not to rush slow work.
const Limit = 10 * time.Minute

// waitDelay is how long Wait keeps reading output after a kill: a child the
// command started, such as ssh under git push, can hold it open.
const waitDelay = 5 * time.Second

// Cmd is an exec.Cmd that is killed once its limit passes.
type Cmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
	limit  time.Duration
}

// Command is exec.Command with Limit applied.
func Command(name string, args ...string) *Cmd {
	return Within(Limit, name, args...)
}

// Within is exec.Command with the given limit, counted from now.
//
// Only the command itself is killed, not a process group: one run from a
// terminal, such as a push asking for credentials, must still be able to read it.
func Within(limit time.Duration, name string, args ...string) *Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- callers pass fixed tool names
	cmd.WaitDelay = waitDelay
	return &Cmd{Cmd: cmd, ctx: ctx, cancel: cancel, limit: limit}
}

// Run is exec.Cmd.Run, saying so when the limit ended it.
func (c *Cmd) Run() error {
	defer c.cancel()
	return c.explain(c.Cmd.Run())
}

// Output is exec.Cmd.Output, saying so when the limit ended it.
func (c *Cmd) Output() ([]byte, error) {
	defer c.cancel()
	out, err := c.Cmd.Output()
	return out, c.explain(err)
}

// CombinedOutput is exec.Cmd.CombinedOutput, saying so when the limit ended it.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	defer c.cancel()
	out, err := c.Cmd.CombinedOutput()
	return out, c.explain(err)
}

func (c *Cmd) explain(err error) error {
	if err != nil && errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s: %w", filepath.Base(c.Path), c.limit, err)
	}
	return err
}
