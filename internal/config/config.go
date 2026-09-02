// Package config parses the YAML batch file that dispatches many tasks at
// once.
//
// The format is flat on purpose — `defaults` plus per-task overrides, no DSL —
// so anyone on the team can read and write one without learning new syntax. It
// shares its field names with the single-task command-line flags, so there is
// one set of concepts, not two.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// Defaults are applied to every task that does not set the field itself.
	Defaults Defaults `yaml:"defaults"`
	// Tasks are the units of work to dispatch.
	Tasks []Entry `yaml:"tasks"`
}

// Defaults are the batch-wide settings a task inherits.
type Defaults struct {
	CLI   task.CLI `yaml:"cli"`
	Model string   `yaml:"model"`
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
		if t.Branch != "" {
			if prev, dup := seenBranch[t.Branch]; dup {
				errs = append(errs, fmt.Errorf("%s: branch %q is already used by tasks[%d]", where, t.Branch, prev))
			}
			seenBranch[t.Branch] = i
		}
		if t.CLI != "" && !t.CLI.Known() {
			errs = append(errs, fmt.Errorf("%s: unsupported cli %q, want one of %v", where, t.CLI, task.KnownCLIs))
		}
		// There is no default CLI, so a task that names none and inherits none
		// has nothing to dispatch to.
		if t.CLI == "" && f.Defaults.CLI == "" {
			errs = append(errs, fmt.Errorf("%s: no cli set, and defaults sets none either", where))
		}
	}
	if f.Defaults.CLI != "" && !f.Defaults.CLI.Known() {
		errs = append(errs, fmt.Errorf("defaults: unsupported cli %q, want one of %v", f.Defaults.CLI, task.KnownCLIs))
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
	return task.Task{
		ID:         e.ID,
		Source:     e.Source,
		Prompt:     e.Prompt,
		Repo:       pick(e.Repo, f.Repo),
		Branch:     e.Branch,
		BaseBranch: pick(e.BaseBranch, f.BaseBranch),
		CLI:        cli,
		Model:      pick(e.Model, f.Defaults.Model),
	}
}
