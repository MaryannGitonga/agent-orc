package orc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// The branch is left behind unless deleteBranch is set: it holds the work, and
// by this point it is normally pushed with a draft PR open against it. Cleanup
// reclaims the checkout, not the results.
func (c *Cleaner) Clean(id string, force, deleteBranch bool) error {
	record, err := c.store.Load(id)
	if err != nil {
		return err
	}
	if record.Status.Active() && !force {
		return fmt.Errorf("task %q is %s; stop it first or pass --force", id, record.Status)
	}

	// Only a missing worktree counts as gone. Any other stat failure, a
	// permission or an I/O error, means the state of that directory is unknown,
	// and carrying on would delete the record and possibly the branch while
	// leaving a checkout nobody can reach and nothing now points at. Forcing
	// past it stays possible, because a worktree that cannot be read is not a
	// reason to be unable to clean up the record forever.
	hasWorktree := false
	switch _, statErr := os.Stat(record.Worktree); {
	case statErr == nil:
		hasWorktree = true
	case errors.Is(statErr, fs.ErrNotExist):
	case !force:
		return fmt.Errorf("checking whether %s is still there: %w\npass --force to remove the record anyway and leave the worktree", record.Worktree, statErr)
	default:
		fmt.Fprintf(c.out, "%s  warning: %s could not be inspected (%v); leaving it in place\n",
			id, record.Worktree, statErr)
	}

	// The repository is opened up front because the branch may need deleting
	// even when the worktree is already gone, which is what a half-finished
	// cleanup or a manually removed checkout leaves behind.
	var repo *gitx.Repo
	if hasWorktree || deleteBranch {
		var err error
		if repo, err = gitx.Open(record.Repo); err != nil {
			return err
		}
	}

	if hasWorktree {
		if err := repo.RemoveWorktree(record.Worktree, force); err != nil {
			// Only offer --force when uncommitted work is what is actually in
			// the way. git refuses for plenty of other reasons, and naming the
			// wrong one sends people down a dead end.
			if dirty, dirtyErr := gitx.HasUncommittedChanges(record.Worktree); dirtyErr == nil && dirty {
				return fmt.Errorf("%w\nthe worktree has uncommitted changes; pass --force to discard them", err)
			}
			return err
		}
	}

	// Before the state file goes, since it is what names the branch: a failure
	// here has to leave the record behind for another attempt.
	branch := "left in place"
	if deleteBranch {
		if !force {
			if reason := unsafeToDelete(repo, record); reason != "" {
				return fmt.Errorf("branch %q %s; pass --force to delete it regardless",
					record.Branch, reason)
			}
		}
		branch = "already gone"
		if repo.BranchExists(record.Branch) {
			if err := repo.DeleteBranch(record.Branch); err != nil {
				return err
			}
			branch = "deleted"
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

	fmt.Fprintf(c.out, "%s  cleaned up; branch %s %s\n", id, record.Branch, branch)
	return nil
}

// unsafeToDelete says why a task's branch still holds work, or "" when it does
// not and can go.
//
// The question is asked against the base the task was cut from, not against
// whatever the main checkout happens to have checked out. git's own safe
// delete asks the latter, which is the wrong question here: a task branch that
// added nothing at all is refused whenever the repository sits on a branch
// that does not contain its base, and the only way past that refusal is
// --force, which would then also throw away branches that do hold work.
func unsafeToDelete(repo *gitx.Repo, record state.Task) string {
	// Already gone, so there is nothing to protect and nothing to do. This is
	// what a manual deletion leaves behind, and what a previous cleanup that
	// deleted the branch and then failed to remove the state leaves behind;
	// treating the missing ref as unpushed work would make the second attempt
	// refuse and keep the id taken.
	if !repo.BranchExists(record.Branch) {
		return ""
	}
	// Nothing of its own: the branch is already contained in its base.
	if repo.IsAncestor(record.Branch, record.BaseBranch) {
		return ""
	}
	// Or its commits are on the remote, where deleting the local branch loses
	// nothing. PushedSHA is what agent-orc last pushed for this task, so it
	// only counts while the branch has not moved since.
	if record.PushedSHA != "" {
		if sha, err := repo.SHA(record.Branch); err == nil && sha == record.PushedSHA {
			return ""
		}
	}
	return fmt.Sprintf("holds commits that are not in %s and were not pushed", record.BaseBranch)
}

// CleanAll removes every task that is no longer running.
func (c *Cleaner) CleanAll(force, deleteBranch bool) error {
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
		if err := c.Clean(t.ID, force, deleteBranch); err != nil {
			fmt.Fprintf(c.out, "%s  not cleaned: %v\n", t.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
