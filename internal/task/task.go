// Package task defines what a unit of dispatched work is: one prompt, run by
// one agentic CLI, on one branch, in one worktree.
package task

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
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
	// Raw makes Render return the prompt alone: no standing instructions and no
	// operating rules. agent-orc sets it for its own prompts, such as the
	// reviewer's, which is not doing the work and must not be told to commit.
	// It is not something a task file can ask for.
	Raw bool `yaml:"-" json:"-"`
	// Instructions are standing rules that apply to the work rather than
	// describing it: house style, a command to run before committing. They are
	// assembled by the dispatcher from the instructions file and the batch
	// defaults, not written into a task by hand.
	Instructions string `yaml:"-" json:"instructions,omitempty"`
	// Review configures the optional agentic review pass.
	Review Review `yaml:"review" json:"review,omitempty"`
	// TestCommand is the project's own test command, run in the worktree once
	// the agent has finished. It is normally filled in at dispatch from what
	// the repository says about itself, so a task carries the command that
	// will actually run rather than a request to work one out later. Empty by
	// the time it gets here means nothing was found and nothing was set, or
	// that verification was turned off, and nothing is checked.
	TestCommand string `yaml:"test_command" json:"test_command,omitempty"`
	// SkipTests is set when the task was told not to run tests at all, as
	// opposed to simply having no command to run. The two are different
	// instructions to the agent: nothing found is a reason to ask it to look,
	// and an opt-out is a reason not to mention tests to it at all, since the
	// suite it would find is the one somebody said not to run.
	SkipTests bool `yaml:"-" json:"skip_tests,omitempty"`
	// TestTimeout caps a single run of that command. Zero means the default;
	// negative means no cap at all. It is the one bound the loop's own stop
	// conditions cannot supply: a command that never returns never passes,
	// never fails, and never gives the agent anything to act on.
	TestTimeout time.Duration `yaml:"-" json:"test_timeout,omitempty"`
}

// DefaultTestTimeout is how long a single run of the test command may take.
// Generous on purpose: it is there to catch a suite that has hung, not to
// hurry one that is slow, and firing on a real suite would be worse than not
// firing on a hung one.
const DefaultTestTimeout = 30 * time.Minute

// NoTestTimeout is the TestTimeout value that lets the command run for as long
// as it likes, for a suite that genuinely takes longer than the default.
const NoTestTimeout = -1

// TestRunTimeout returns the effective cap on one run of the test command, or
// zero when there is none.
func (t Task) TestRunTimeout() time.Duration {
	switch {
	case t.TestTimeout < 0:
		return 0
	case t.TestTimeout == 0:
		return DefaultTestTimeout
	default:
		return t.TestTimeout
	}
}

// Review configures the optional agentic review pass.
//
// It is off by default because it costs real money and real time. Once on, it
// runs until the reviewer approves: a change that has been reviewed but not
// approved is not a reviewed change, and stopping at an arbitrary count would
// only publish it anyway. What bounds it is the budget, and a worker that has
// stopped acting on the comments.
type Review struct {
	// Enabled allows `agent-orc review` to run for this task.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`
	// CLI runs the review. Empty means a different CLI from the worker's, so
	// the reviewer is less likely to share the worker's blind spots.
	CLI CLI `yaml:"cli" json:"cli,omitempty"`
	// Model is the reviewer's model; empty means that CLI's default.
	Model string `yaml:"model" json:"model,omitempty"`
	// Auto runs the review automatically when the agent finishes, before the
	// branch is published, so the PR that opens has already been through it.
	Auto bool `yaml:"auto" json:"auto,omitempty"`
}

// Normalize applies the invariants a review block has to satisfy however it was
// built, from flags or from a file.
//
// Asking for a review when the agent finishes is asking for a review, so auto
// implies enabled. It lives here rather than at each construction site because
// it was written at two of them and not the third, and a batch file that set
// only `auto` silently got no review at all.
func (r Review) Normalize() Review {
	if r.Auto {
		r.Enabled = true
	}
	return r
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
	// A leading dash is the one branch name that turns into a flag when it
	// reaches git. git refuses to create such a branch anyway, but it does so
	// from inside `git worktree add` with "unknown switch", which says nothing
	// about the name that caused it; refusing here says what is wrong.
	if strings.HasPrefix(t.Branch, "-") {
		errs = append(errs, fmt.Errorf("branch %q must not start with '-': git reads such a name as a flag", t.Branch))
	}
	if strings.HasPrefix(t.BaseBranch, "-") {
		errs = append(errs, fmt.Errorf("base branch %q must not start with '-': git reads such a name as a flag", t.BaseBranch))
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

// testRule names the command when one is known, so the agent runs the same
// thing agent-orc is about to run rather than something adjacent to it.
const testRule = "\n- Before committing, run `%s` in this worktree and make it pass.\n" +
	"  Do not finish with it failing; agent-orc runs it after you and will not\n" +
	"  publish a branch that leaves it red."

// genericTestRule is the fallback when no command could be found. Asking the
// agent to look is still worth doing: it is sitting in the repository and can
// read the build files, which is more than agent-orc's own conventions cover.
// Nothing verifies this one, so it is phrased as the instruction it is.
const genericTestRule = "\n- If this project has a test suite, run it before committing and make it\n" +
	"  pass. Do not commit work that leaves it failing."

// Render returns the task's prompt followed by the fixed operating rules.
func (t Task) Render() string {
	out := strings.TrimSpace(t.Prompt)
	// Raw is for agent-orc's own prompts, such as the reviewer's. Those get
	// neither the standing instructions nor the operating rules: the reviewer
	// is not doing the work, so rules about how to do it do not apply to it.
	if t.Raw {
		return out
	}
	if s := strings.TrimSpace(t.Instructions); s != "" {
		out += instructionsHeader + s
	}
	out += policySuffix
	switch cmd := strings.TrimSpace(t.TestCommand); {
	case t.SkipTests:
		// Nothing about tests at all. Asking an agent to find and run a suite
		// that was explicitly turned off would have it spend the task's time
		// and budget on the one thing it was told to leave alone.
	case cmd != "":
		out += fmt.Sprintf(testRule, cmd)
	default:
		out += genericTestRule
	}
	return out
}

// instructionsHeader separates the task from the standing instructions, so an
// agent can tell what it was asked to do from how it was asked to do it.
const instructionsHeader = "\n\n---\nStanding instructions (they apply to every task here):\n"
