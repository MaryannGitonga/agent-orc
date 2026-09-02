package orc

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// Cleaner removes finished tasks' worktrees and local state.
type Cleaner struct {
	layout paths.Layout
	store  *state.Store
	out    io.Writer
}

// NewCleaner returns a cleaner writing progress to out.
func NewCleaner(layout paths.Layout, out io.Writer) *Cleaner {
	return &Cleaner{layout: layout, store: state.NewStore(layout.State), out: out}
}

// Clean removes one task's worktree and state.
//
// The branch is deliberately left behind: it holds the work, and by this point
// it is normally pushed with a draft PR open against it. Cleanup reclaims the
// checkout, not the results.
func (c *Cleaner) Clean(id string, force bool) error {
	record, err := c.store.Load(id)
	if err != nil {
		return err
	}
	if record.Status.Active() && !force {
		return fmt.Errorf("task %q is %s; stop it first or pass --force", id, record.Status)
	}

	if _, statErr := os.Stat(record.Worktree); statErr == nil {
		repo, err := gitx.Open(record.Repo)
		if err != nil {
			return err
		}
		if err := repo.RemoveWorktree(record.Worktree, force); err != nil {
			return fmt.Errorf("%w\nthe worktree has uncommitted changes; pass --force to discard them", err)
		}
	}

	if err := c.store.Delete(id); err != nil {
		return err
	}
	// Logs are the record of what happened and outlive the worktree, so they
	// are only removed when explicitly forced.
	if force {
		for _, path := range []string{c.layout.LogFile(id), c.layout.SupervisorLogFile(id)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", path, err)
			}
		}
	}

	fmt.Fprintf(c.out, "%s  cleaned up; branch %s left in place\n", id, record.Branch)
	return nil
}

// CleanAll removes every task that is no longer running.
func (c *Cleaner) CleanAll(force bool) error {
	tasks, err := c.store.List()
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Fprintln(c.out, "no tasks to clean up")
		return nil
	}

	var errs []error
	for _, t := range tasks {
		if t.Status.Active() && !force {
			fmt.Fprintf(c.out, "%s  skipped; still %s\n", t.ID, t.Status)
			continue
		}
		if err := c.Clean(t.ID, force); err != nil {
			fmt.Fprintf(c.out, "%s  not cleaned: %v\n", t.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
