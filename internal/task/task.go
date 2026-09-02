// Package task defines what a unit of dispatched work is: one prompt, run by
// one agentic CLI, on one branch, in one worktree.
package task

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// CLI names an agentic command-line tool agent-orc can dispatch to.
type CLI string

// The CLIs agent-orc knows about.
const (
	// CLIClaude is Claude Code.
	CLIClaude CLI = "claude"
	// CLICopilot is the GitHub Copilot CLI.
	CLICopilot CLI = "copilot"
	// CLICodex is the OpenAI Codex CLI.
	CLICodex CLI = "codex"
)

// KnownCLIs lists every CLI a task may name, in a stable order.
var KnownCLIs = []CLI{CLIClaude, CLICopilot, CLICodex}

// Known reports whether c is a CLI agent-orc can dispatch to.
func (c CLI) Known() bool {
	return slices.Contains(KnownCLIs, c)
}

// Task is one unit of dispatched work.
type Task struct {
	// ID names the task's state file, log, worktree and default branch.
	ID   string `yaml:"id" json:"id"`
	Repo string `yaml:"repo" json:"repo"` // absolute path
	// Source is where the work is described: a `github://owner/repo#N` or
	// `jira://KEY-1` reference fetched at launch, or free text. It may be
	// empty when Prompt says everything.
	Source string `yaml:"source" json:"source,omitempty"`
	// Prompt is layered on top of a fetched Source as extra instructions.
	Prompt     string `yaml:"prompt" json:"prompt"`
	Branch     string `yaml:"branch" json:"branch"`
	BaseBranch string `yaml:"base_branch" json:"base_branch"`
	CLI        CLI    `yaml:"cli" json:"cli"`
	Model      string `yaml:"model" json:"model"` // empty means the CLI's default
	// Subagents seeds the CLI's subagent definitions into the worktree. Absence
	// of the files is the "off" state, so no flag is passed to the CLI itself.
	Subagents bool   `yaml:"subagents" json:"subagents,omitempty"`
	Budget    Budget `yaml:",inline" json:"budget"` // omitempty does nothing on a struct
	// AutoPR opens a draft PR when the agent finishes. On by default: a draft
	// still needs a human to mark it ready, so it costs nothing in safety.
	AutoPR bool `yaml:"auto_pr" json:"auto_pr"`
	// DCOSignoff adds a Signed-off-by trailer to any commit missing one.
	DCOSignoff bool `yaml:"dco_signoff" json:"dco_signoff,omitempty"`
	// Review configures the optional agentic review pass.
	Review Review `yaml:"review" json:"review,omitempty"`
}

// Review configures the optional, manually triggered review pass.
//
// It is off by default and hard-capped on purpose: unlike opening a draft PR,
// a review round costs real money and real time, and an uncapped worker↔
// reviewer loop is exactly the kind of thing that runs until someone notices.
type Review struct {
	// Enabled allows `agent-orc review` to run for this task.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`
	// CLI runs the review. Empty means a different CLI from the worker's, so
	// the reviewer is less likely to share the worker's blind spots.
	CLI CLI `yaml:"cli" json:"cli,omitempty"`
	// Model is the reviewer's model; empty means that CLI's default.
	Model string `yaml:"model" json:"model,omitempty"`
	// MaxRounds caps worker↔reviewer round-trips. Zero means one round.
	MaxRounds int `yaml:"max_rounds" json:"max_rounds,omitempty"`
}

// Rounds returns the effective cap on review round-trips.
func (r Review) Rounds() int {
	if r.MaxRounds <= 0 {
		return 1
	}
	return r.MaxRounds
}

// ReviewerCLI returns the CLI that should run the review for a worker task.
// With nothing configured it picks a CLI other than the worker's, so the two
// sessions are less likely to make the same mistake.
func (t Task) ReviewerCLI() CLI {
	if t.Review.CLI != "" {
		return t.Review.CLI
	}
	for _, c := range KnownCLIs {
		if c != t.CLI {
			return c
		}
	}
	return t.CLI
}

// Budget caps a task's spend. Each CLI caps cost in its own unit and agent-orc
// does not convert between them: a budget in a unit the CLI does not
// understand is reported as unenforced rather than quietly approximated.
type Budget struct {
	USD     *float64 `yaml:"budget_usd" json:"usd,omitempty"`
	Credits *float64 `yaml:"budget_credits" json:"credits,omitempty"`
}

// IsZero reports whether no budget was set at all.
func (b Budget) IsZero() bool { return b.USD == nil && b.Credits == nil }

// Validate rejects budgets that cannot mean anything.
func (b Budget) Validate() error {
	var errs []error
	if b.USD != nil && *b.USD <= 0 {
		errs = append(errs, fmt.Errorf("budget_usd must be greater than zero, got %v", *b.USD))
	}
	if b.Credits != nil && *b.Credits <= 0 {
		errs = append(errs, fmt.Errorf("budget_credits must be greater than zero, got %v", *b.Credits))
	}
	return errors.Join(errs...)
}

// validID is strict because an ID becomes a file name and a path segment.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateSpec checks a task as written, before its source has been fetched.
// At that point a task needs something to work from (a source, a prompt, or
// both) but not necessarily a prompt.
func (t Task) ValidateSpec() error {
	errs := []error{t.validateCommon()}
	if strings.TrimSpace(t.Prompt) == "" && strings.TrimSpace(t.Source) == "" {
		errs = append(errs, errors.New("needs a source, a prompt, or both"))
	}
	return errors.Join(errs...)
}

// Validate reports whether the task is ready to dispatch: defaults applied and
// any source already fetched into the prompt.
func (t Task) Validate() error {
	errs := []error{t.validateCommon()}
	if strings.TrimSpace(t.Prompt) == "" {
		errs = append(errs, errors.New("prompt is empty"))
	}
	return errors.Join(errs...)
}

func (t Task) validateCommon() error {
	var errs []error
	if !validID.MatchString(t.ID) {
		errs = append(errs, fmt.Errorf("id %q must be 1-64 chars of letters, digits, '.', '_' or '-' and start alphanumeric", t.ID))
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
	if !t.CLI.Known() {
		errs = append(errs, fmt.Errorf("unsupported cli %q, want one of %v", t.CLI, KnownCLIs))
	}
	if err := t.Budget.Validate(); err != nil {
		errs = append(errs, err)
	}
	if t.Review.CLI != "" && !t.Review.CLI.Known() {
		errs = append(errs, fmt.Errorf("unsupported review cli %q, want one of %v", t.Review.CLI, KnownCLIs))
	}
	if t.Review.MaxRounds < 0 {
		errs = append(errs, fmt.Errorf("review max_rounds must not be negative, got %d", t.Review.MaxRounds))
	}
	return errors.Join(errs...)
}

// DefaultBranch is the branch name used when a task does not specify one. It
// lowercases the id, so two ids differing only in case share a branch; the
// second one to run fails on the branch collision check rather than colliding
// silently.
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
