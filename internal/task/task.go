// Package task defines what a unit of dispatched work is: one prompt, run by
// one agentic CLI, on one branch, in one worktree.
package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// CLI names an agentic command-line tool agent-orc can dispatch to.
type CLI string

// The CLIs agent-orc knows about. Phase 0 dispatches to Claude only.
const (
	CLIClaude CLI = "claude"
)

// Task is one unit of dispatched work.
type Task struct {
	// ID names the task's state file, log, worktree and default branch.
	ID         string `yaml:"id" json:"id"`
	Repo       string `yaml:"repo" json:"repo"` // absolute path
	Prompt     string `yaml:"prompt" json:"prompt"`
	Branch     string `yaml:"branch" json:"branch"`
	BaseBranch string `yaml:"base_branch" json:"base_branch"`
	CLI        CLI    `yaml:"cli" json:"cli"`
	Model      string `yaml:"model" json:"model"` // empty means the CLI's default
}

// validID is strict because an ID becomes a file name and a path segment.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Validate checks a task that already has its defaults applied.
func (t Task) Validate() error {
	var errs []error
	if !validID.MatchString(t.ID) {
		errs = append(errs, fmt.Errorf("id %q must be 1-64 chars of letters, digits, '.', '_' or '-' and start alphanumeric", t.ID))
	}
	if strings.TrimSpace(t.Prompt) == "" {
		errs = append(errs, errors.New("prompt is empty"))
	}
	if t.Repo == "" {
		errs = append(errs, errors.New("repo is empty"))
	}
	if t.Branch == "" {
		errs = append(errs, errors.New("branch is empty"))
	}
	if t.BaseBranch == "" {
		errs = append(errs, errors.New("base branch is empty"))
	}
	if t.CLI != CLIClaude {
		errs = append(errs, fmt.Errorf("unsupported cli %q, want %q", t.CLI, CLIClaude))
	}
	return errors.Join(errs...)
}

// DefaultBranch is the branch name used when a task does not specify one.
func DefaultBranch(id string) string {
	return "agent-orc/" + strings.ToLower(id)
}

// policySuffix is appended to every prompt. Publishing is agent-orc's job: an
// agent that pushed would bypass the commit sanitization that has to run first.
const policySuffix = `

---
Operating rules for this run (set by agent-orc, not by the task author):
- Do the work on the branch that is already checked out in this worktree.
- Commit your work locally, with a one-line conventional commit subject.
- Do NOT push, do NOT open a pull request, do NOT create or switch branches.
  Pushing and opening a draft PR is handled for you after this session ends.
- Do not add any Co-authored-by or "Generated with" trailer to your commits.`

// Render returns the task's prompt followed by the fixed operating rules.
func (t Task) Render() string {
	return strings.TrimSpace(t.Prompt) + policySuffix
}
