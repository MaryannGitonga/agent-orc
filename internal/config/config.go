// Package config parses the YAML batch file that dispatches many tasks at
// once.
//
// The format is flat on purpose: `defaults` plus per-task overrides and no
// DSL, so anyone on the team can read and write one without learning new
// syntax. It
// shares its field names with the single-task command-line flags, so there is
// one set of concepts, not two.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// File is a parsed batch file.
type File struct {
	// Repo is the repository every task in the batch works in, unless a task
	// overrides it.
	Repo string `yaml:"repo"`
	// BaseBranch is what tasks branch from unless they override it.
	BaseBranch string `yaml:"base_branch"`
	// DCOSignoff adds a Signed-off-by trailer to commits missing one. Set it
	// for repositories that require DCO.
	DCOSignoff bool `yaml:"dco_signoff"`
	// Defaults are applied to every task that does not set the field itself.
	Defaults Defaults `yaml:"defaults"`
	// Tasks are the units of work to dispatch.
	Tasks []Entry `yaml:"tasks"`
}

// Defaults are the batch-wide settings a task inherits.
type Defaults struct {
	CLI           task.CLI `yaml:"cli"`
	Model         string   `yaml:"model"`
	Subagents     *bool    `yaml:"subagents"`
	BudgetUSD     *float64 `yaml:"budget_usd"`
	BudgetCredits *float64 `yaml:"budget_credits"`
	AutoPR        *bool    `yaml:"auto_pr"`
	Review        *Review  `yaml:"review"`
	// TestCommand is run in each task's worktree once its agent has finished.
	TestCommand string `yaml:"test_command"`
	// TestTimeout caps one run of it, as a duration such as "10m", or "none".
	TestTimeout string `yaml:"test_timeout"`
	// Instructions apply to every task in the batch, on top of anything in
	// ~/.agent-orc/instructions.md.
	Instructions string `yaml:"instructions"`
}

// Review is the review block as written in a batch file.
type Review struct {
	Enabled *bool    `yaml:"enabled"`
	Auto    *bool    `yaml:"auto"`
	CLI     task.CLI `yaml:"cli"`
	Model   string   `yaml:"model"`
}

// Entry is one task as written in the batch file. Every field is optional
// except `id` and having at least one of `source` or `prompt`; anything left
// out falls back to the batch defaults.
type Entry struct {
	ID         string   `yaml:"id"`
	Source     string   `yaml:"source"`
	Prompt     string   `yaml:"prompt"`
	Repo       string   `yaml:"repo"`
	Branch     string   `yaml:"branch"`
	BaseBranch string   `yaml:"base_branch"`
	CLI        task.CLI `yaml:"cli"`
	Model      string   `yaml:"model"`
	// Subagents and the budget fields are pointers so "unset" is
	// distinguishable from "explicitly false or zero", so a task can turn a
	// batch default off rather than only leave it alone.
	Subagents     *bool    `yaml:"subagents"`
	BudgetUSD     *float64 `yaml:"budget_usd"`
	BudgetCredits *float64 `yaml:"budget_credits"`
	AutoPR        *bool    `yaml:"auto_pr"`
	Review        *Review  `yaml:"review"`

	TestCommand string `yaml:"test_command"`
	TestTimeout string `yaml:"test_timeout"`
}

// Load reads and validates a batch file. Paths inside it are resolved relative
// to the file's own directory, so a batch file is portable between machines
// that check the repo out in different places.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading batch file: %w", err)
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}
	f.resolvePaths(base)
	return f, nil
}

// Parse decodes a batch file. Unknown fields are rejected rather than ignored:
// a typo in a batch file should not silently dispatch a task configured
// differently from what was written.
func Parse(data []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	if len(f.Tasks) == 0 {
		return nil, errors.New("no tasks defined")
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// resolvePaths makes every repo path absolute relative to base.
func (f *File) resolvePaths(base string) {
	if f.Repo != "" && !filepath.IsAbs(f.Repo) {
		f.Repo = filepath.Join(base, f.Repo)
	}
	for i := range f.Tasks {
		if r := f.Tasks[i].Repo; r != "" && !filepath.IsAbs(r) {
			f.Tasks[i].Repo = filepath.Join(base, r)
		}
	}
}

// validate catches the problems that can be seen without touching git: a
// missing id, a task with nothing to do, or two tasks claiming the same id.
func (f *File) validate() error {
	var errs []error
	seenID := map[string]int{}
	seenBranch := map[string]int{}

	for i, t := range f.Tasks {
		where := fmt.Sprintf("tasks[%d]", i)
		if strings.TrimSpace(t.ID) == "" {
			errs = append(errs, fmt.Errorf("%s: id is required", where))
			continue
		}
		where = fmt.Sprintf("task %q", t.ID)

		if strings.TrimSpace(t.Source) == "" && strings.TrimSpace(t.Prompt) == "" {
			errs = append(errs, fmt.Errorf("%s: needs a source, a prompt, or both", where))
		}
		if prev, dup := seenID[t.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate id, already used by tasks[%d]", where, prev))
		}
		seenID[t.ID] = i

		// Two tasks on one branch would fight over the same worktree; the
		// isolation the whole design rests on only holds if branches differ.
		// Compare the branch each task will actually get rather than only the
		// explicit ones, so ids that differ just in case are caught here and
		// not by git after the first task has already launched.
		branch := t.Branch
		if branch == "" {
			branch = task.DefaultBranch(t.ID)
		}
		if prev, dup := seenBranch[branch]; dup {
			errs = append(errs, fmt.Errorf("%s: branch %q is already used by tasks[%d]", where, branch, prev))
		}
		seenBranch[branch] = i
		if t.CLI != "" && !t.CLI.Known() {
			errs = append(errs, fmt.Errorf("%s: unsupported cli %q, want one of %v", where, t.CLI, task.KnownCLIs))
		}
		// There is no default CLI, so a task that names none and inherits none
		// has nothing to dispatch to.
		if t.CLI == "" && f.Defaults.CLI == "" {
			errs = append(errs, fmt.Errorf("%s: no cli set, and defaults sets none either", where))
		}
		if err := (task.Budget{USD: t.BudgetUSD, Credits: t.BudgetCredits}).Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		if _, err := ParseTestTimeout(t.TestTimeout); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		if r := t.Review; r != nil {
			if r.CLI != "" && !r.CLI.Known() {
				errs = append(errs, fmt.Errorf("%s: unsupported review cli %q, want one of %v", where, r.CLI, task.KnownCLIs))
			}
		}
	}
	if f.Defaults.CLI != "" && !f.Defaults.CLI.Known() {
		errs = append(errs, fmt.Errorf("defaults: unsupported cli %q, want one of %v", f.Defaults.CLI, task.KnownCLIs))
	}
	if err := (task.Budget{USD: f.Defaults.BudgetUSD, Credits: f.Defaults.BudgetCredits}).Validate(); err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	if _, err := ParseTestTimeout(f.Defaults.TestTimeout); err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	return errors.Join(errs...)
}

// Resolved returns the entry's fields with batch defaults filled in. Branch,
// base branch and repo still need git to finish resolving, which is the
// caller's job.
func (f *File) Resolved(e Entry) task.Task {
	pick := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	cli := task.CLI(pick(string(e.CLI), string(f.Defaults.CLI)))
	subagents := false
	if v := firstBool(e.Subagents, f.Defaults.Subagents); v != nil {
		subagents = *v
	}
	// A draft PR opens by default: it still needs a human to mark it ready,
	// so the safety cost is nil and the work becomes visible immediately.
	autoPR := true
	if v := firstBool(e.AutoPR, f.Defaults.AutoPR); v != nil {
		autoPR = *v
	}
	return task.Task{
		ID:           e.ID,
		Source:       e.Source,
		Prompt:       e.Prompt,
		Repo:         pick(e.Repo, f.Repo),
		Branch:       e.Branch,
		BaseBranch:   pick(e.BaseBranch, f.BaseBranch),
		CLI:          cli,
		Model:        pick(e.Model, f.Defaults.Model),
		Instructions: f.Defaults.Instructions,
		Subagents:    subagents,
		Budget: task.Budget{
			USD:     firstFloat(e.BudgetUSD, f.Defaults.BudgetUSD),
			Credits: firstFloat(e.BudgetCredits, f.Defaults.BudgetCredits),
		},
		AutoPR:      autoPR,
		DCOSignoff:  f.DCOSignoff,
		Review:      mergeReview(e.Review, f.Defaults.Review),
		TestCommand: pick(e.TestCommand, f.Defaults.TestCommand),
		TestTimeout: mustTestTimeout(pick(e.TestTimeout, f.Defaults.TestTimeout)),
	}
}

// mustTestTimeout parses a validated timeout. Parse and Load reject a bad one
// before any task is resolved, so a failure here cannot come from a file.
func mustTestTimeout(raw string) time.Duration {
	d, _ := ParseTestTimeout(raw)
	return d
}

// mergeReview layers a task's review block over the batch default, field by
// field, so a task can change the reviewer without restating the whole block.
func mergeReview(entry, defaults *Review) task.Review {
	var out task.Review
	for _, r := range []*Review{defaults, entry} {
		if r == nil {
			continue
		}
		if r.Enabled != nil {
			out.Enabled = *r.Enabled
		}
		if r.Auto != nil {
			out.Auto = *r.Auto
		}
		if r.CLI != "" {
			out.CLI = r.CLI
		}
		if r.Model != "" {
			out.Model = r.Model
		}
	}
	return out
}

// firstBool returns the first value that was actually set.
func firstBool(values ...*bool) *bool {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// firstFloat returns the first value that was actually set.
func firstFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
