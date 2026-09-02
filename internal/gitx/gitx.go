// Package gitx wraps the handful of git commands agent-orc needs. It shells
// out to the git binary rather than linking a library: the tool is a
// dispatcher, and shelling out keeps the parent repo's own config (signing,
// hooks, credentials) in effect exactly as it would be for a human.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git repository agent-orc dispatches work from.
type Repo struct {
	// Dir is the absolute path to the repository's top level.
	Dir string
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

// DefaultBranch reports the branch a task should be cut from when none is
// given. It prefers the remote's published HEAD, falls back to the currently
// checked-out branch, and errors if neither is available.
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

// RemoveWorktree deletes the worktree at dir. The branch it had checked out is
// left alone; it holds the task's work.
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
