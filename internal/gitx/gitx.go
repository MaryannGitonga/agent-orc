// Package gitx wraps the handful of git commands agent-orc needs. It shells out
// to the git binary so the repository's own config (signing, hooks,
// credentials) applies exactly as it would for a human.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git repository agent-orc dispatches work from.
type Repo struct {
	Dir string // absolute path to the repository's top level
}

// Open verifies that dir is inside a git repository and returns a Repo rooted
// at its top level.
func Open(dir string) (*Repo, error) {
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%q is not a git repository: %w", dir, err)
	}
	return &Repo{Dir: out}, nil
}

// DefaultBranch reports the branch to cut from when none is given: the
// remote's published HEAD, else the checked-out branch.
func (r *Repo) DefaultBranch() (string, error) {
	if out, err := run(r.Dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(out, "origin/"), nil
	}
	out, err := run(r.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("determining default branch: %w", err)
	}
	if out == "HEAD" {
		return "", fmt.Errorf("repository is in a detached HEAD state; pass --base-branch explicitly")
	}
	return out, nil
}

// RevExists reports whether rev resolves to a commit in the repository.
func (r *Repo) RevExists(rev string) bool {
	_, err := run(r.Dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}

// BranchExists reports whether a local branch of that name already exists.
func (r *Repo) BranchExists(branch string) bool {
	_, err := run(r.Dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// AddWorktree checks branch out at dir, creating the branch from base.
func (r *Repo) AddWorktree(dir, branch, base string) error {
	if _, err := run(r.Dir, "worktree", "add", dir, "-b", branch, base); err != nil {
		return fmt.Errorf("creating worktree for branch %q from %q: %w", branch, base, err)
	}
	return nil
}

// DeleteBranch removes a local branch. It exists to undo a branch agent-orc
// created moments earlier, not to throw away a user's work.
func (r *Repo) DeleteBranch(branch string) error {
	if _, err := run(r.Dir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("deleting branch %q: %w", branch, err)
	}
	return nil
}

// RemoveWorktree deletes the worktree at dir. Its branch is left alone: that
// is where the task's work lives.
func (r *Repo) RemoveWorktree(dir string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := run(r.Dir, append(args, dir)...); err != nil {
		return fmt.Errorf("removing worktree %q: %w", dir, err)
	}
	return nil
}

// run executes git in dir and returns its trimmed stdout.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
