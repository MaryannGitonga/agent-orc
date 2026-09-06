package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// RepoFile is the settings file a repository may carry for itself, checked in
// alongside the code so everyone dispatching against that repository gets the
// same defaults.
const RepoFile = ".agent-orc.yaml"

// Settings are the defaults a task inherits when it does not set a field
// itself. They exist so the same flags are not retyped on every run: what is
// true of every task on this machine belongs in the machine-wide file, and
// what is true of a repository belongs in that repository's own.
//
// The field names are the batch file's `defaults` keys, which are in turn the
// run flags with dashes swapped for underscores, so there is one vocabulary
// rather than three. Every field is optional, and the pointers are what make
// "unset" distinguishable from "set to false or zero": a narrower layer has to
// be able to turn an inherited setting off, not only leave it alone.
//
// What cannot be defaulted is what is inherently per-task: the id, the prompt
// or source, the branch, and the repository itself.
type Settings struct {
	CLI           task.CLI `yaml:"cli"`
	Model         string   `yaml:"model"`
	BaseBranch    string   `yaml:"base_branch"`
	Subagents     *bool    `yaml:"subagents"`
	BudgetUSD     *float64 `yaml:"budget_usd"`
	BudgetCredits *float64 `yaml:"budget_credits"`
	AutoPR        *bool    `yaml:"auto_pr"`
	DCOSignoff    *bool    `yaml:"dco_signoff"`
	Review        *Review  `yaml:"review"`
	// TestCommand is the project's own test command, run in the worktree once
	// the agent has finished. It belongs in a repository's file far more often
	// than in the machine-wide one: how a codebase runs its tests is a fact
	// about that codebase.
	TestCommand string `yaml:"test_command"`
	// TestTimeout caps one run of the test command, as a duration such as
	// "10m". Empty means the default; "none" lets it run for as long as it
	// takes, for a suite that genuinely runs longer than the default.
	TestTimeout string `yaml:"test_timeout"`
}

// NoTimeout is the configured value that lets the test command run uncapped.
const NoTimeout = "none"

// ParseTestTimeout turns a configured timeout into a duration: empty is the
// default, "none" is uncapped, and anything else is a Go duration such as
// "90s" or "10m". A cap of zero or less is rejected rather than read as one of
// those, since a timeout that fires immediately would fail every suite.
func ParseTestTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "":
		return 0, nil
	case NoTimeout:
		return task.NoTestTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("test_timeout %q is not a duration such as \"10m\", or %q", raw, NoTimeout)
	}
	if d <= 0 {
		return 0, fmt.Errorf("test_timeout %q must be positive, or %q to run uncapped", raw, NoTimeout)
	}
	return d, nil
}

// LoadSettings reads a settings file. A file that is not there is not an
// error: no settings file is the normal case, and it means the same thing as
// an empty one.
func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("reading %s: %w", path, err)
	}
	s, err := ParseSettings(data)
	if err != nil {
		return Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// LoadRepoSettings reads the settings a repository carries for itself. An
// empty repo path yields nothing, which is what a batch file that names no
// repository of its own gets.
func LoadRepoSettings(repo string) (Settings, error) {
	if strings.TrimSpace(repo) == "" {
		return Settings{}, nil
	}
	return LoadSettings(filepath.Join(repo, RepoFile))
}

// ParseSettings decodes a settings file. Unknown fields are rejected for the
// same reason a batch file rejects them: a typo that silently changes nothing
// is worse than one that fails at the point it was made.
func ParseSettings(data []byte) (Settings, error) {
	var s Settings
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	// An empty file decodes to io.EOF rather than to an empty document, and
	// an empty settings file means the same thing as no settings file.
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return Settings{}, fmt.Errorf("parsing yaml: %w", err)
	}
	if err := s.validate(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// validate catches what can be checked without touching git or the network.
func (s Settings) validate() error {
	var errs []error
	if s.CLI != "" && !s.CLI.Known() {
		errs = append(errs, fmt.Errorf("unsupported cli %q, want one of %v", s.CLI, task.KnownCLIs))
	}
	if err := (task.Budget{USD: s.BudgetUSD, Credits: s.BudgetCredits}).Validate(); err != nil {
		errs = append(errs, err)
	}
	if _, err := ParseTestTimeout(s.TestTimeout); err != nil {
		errs = append(errs, err)
	}
	if r := s.Review; r != nil {
		if r.CLI != "" && !r.CLI.Known() {
			errs = append(errs, fmt.Errorf("unsupported review cli %q, want one of %v", r.CLI, task.KnownCLIs))
		}
	}
	return errors.Join(errs...)
}

// Merge returns s with narrower layered on top: every field narrower sets wins,
// and every field it leaves alone keeps s's value. That is what lets a
// repository change one setting without restating the machine-wide file.
//
// Empty means "not set here", never "reset this to the built-in default", so a
// narrower layer overrides an inherited value by naming the one it wants rather
// than by blanking the field. A repository under a machine-wide
// `test_timeout: none` gets a cap back by writing the duration it wants, and
// one under `test_command: none` by writing the command. There is deliberately
// no third value meaning "the default": it would have to be understood by every
// field to be worth having, and the layering is easier to reason about when a
// value present in a file is the value that applies.
func (s Settings) Merge(narrower Settings) Settings {
	out := s
	if narrower.CLI != "" {
		out.CLI = narrower.CLI
	}
	if narrower.Model != "" {
		out.Model = narrower.Model
	}
	if narrower.BaseBranch != "" {
		out.BaseBranch = narrower.BaseBranch
	}
	if narrower.Subagents != nil {
		out.Subagents = narrower.Subagents
	}
	if narrower.BudgetUSD != nil {
		out.BudgetUSD = narrower.BudgetUSD
	}
	if narrower.BudgetCredits != nil {
		out.BudgetCredits = narrower.BudgetCredits
	}
	if narrower.AutoPR != nil {
		out.AutoPR = narrower.AutoPR
	}
	if narrower.DCOSignoff != nil {
		out.DCOSignoff = narrower.DCOSignoff
	}
	if narrower.TestCommand != "" {
		out.TestCommand = narrower.TestCommand
	}
	if narrower.TestTimeout != "" {
		out.TestTimeout = narrower.TestTimeout
	}
	out.Review = mergeReviewBlocks(s.Review, narrower.Review)
	return out
}

// mergeReviewBlocks layers one review block over another, field by field, so a
// layer can change the reviewer without restating the rest of the block.
func mergeReviewBlocks(broader, narrower *Review) *Review {
	if broader == nil {
		return narrower
	}
	if narrower == nil {
		return broader
	}
	out := *broader
	if narrower.Enabled != nil {
		out.Enabled = narrower.Enabled
	}
	if narrower.Auto != nil {
		out.Auto = narrower.Auto
	}
	if narrower.CLI != "" {
		out.CLI = narrower.CLI
	}
	if narrower.Model != "" {
		out.Model = narrower.Model
	}
	return &out
}

// LayerUnder puts s beneath the batch file's own settings, so a batch only has
// to state what it wants to differ from the machine-wide and repository files.
//
// The repository layer a caller passes here is the one belonging to the batch
// file's own `repo`. A task that overrides `repo` to point somewhere else does
// not pick up that other repository's file: the batch is resolved once, before
// any task is dispatched.
func (f *File) LayerUnder(s Settings) {
	merged := s.Merge(Settings{
		CLI:           f.Defaults.CLI,
		Model:         f.Defaults.Model,
		BaseBranch:    f.BaseBranch,
		Subagents:     f.Defaults.Subagents,
		BudgetUSD:     f.Defaults.BudgetUSD,
		BudgetCredits: f.Defaults.BudgetCredits,
		AutoPR:        f.Defaults.AutoPR,
		TestCommand:   f.Defaults.TestCommand,
		// The batch file's dco_signoff is a plain bool, so it can only turn
		// the setting on. Leaving it nil when false is what keeps it from
		// silently overriding a repository that asked for sign-off.
		DCOSignoff: trueOrNil(f.DCOSignoff),
		Review:     f.Defaults.Review,
	})

	f.BaseBranch = merged.BaseBranch
	f.Defaults.CLI = merged.CLI
	f.Defaults.Model = merged.Model
	f.Defaults.Subagents = merged.Subagents
	f.Defaults.BudgetUSD = merged.BudgetUSD
	f.Defaults.BudgetCredits = merged.BudgetCredits
	f.Defaults.AutoPR = merged.AutoPR
	f.Defaults.Review = merged.Review
	f.Defaults.TestCommand = merged.TestCommand
	f.Defaults.TestTimeout = merged.TestTimeout
	f.DCOSignoff = merged.DCOSignoff != nil && *merged.DCOSignoff
}

// trueOrNil turns a plain bool into the tri-state the layers merge on, mapping
// false to "unset" rather than to "explicitly off".
func trueOrNil(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}
