package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/config"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/sanitize"
	"github.com/MaryannGitonga/agent-orc/internal/task"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

const usage = `agent-orc: dispatch agentic CLI runs across isolated git worktrees

Usage:
  agent-orc run [flags]         dispatch a single task
  agent-orc run <tasks.yaml>    dispatch every task in a batch file
  agent-orc status              show every task as a table
  agent-orc logs <task-id> [-f] [--raw]
                                print a task's log, or follow it
  agent-orc stop <task-id>      kill a running task
  agent-orc pr <task-id>        sanitize, push and open the draft PR by hand
  agent-orc review <task-id>    run an independent review round, if enabled
  agent-orc cleanup <task-id|--all>
                                remove a task's worktree and local state
  agent-orc version             print the version

Run 'agent-orc run -h' for the run flags.`

// dispatch routes argv to a subcommand.
func dispatch(argv []string, out io.Writer) error {
	if len(argv) == 0 {
		fmt.Fprintln(out, usage)
		return nil
	}
	switch argv[0] {
	case "run":
		return runCmd(argv[1:], out)
	case "status":
		return statusCmd(argv[1:], out)
	case "logs":
		return logsCmd(argv[1:], out)
	case "stop":
		return stopCmd(argv[1:], out)
	case "pr":
		return prCmd(argv[1:], out)
	case "review":
		return reviewCmd(argv[1:], out)
	case "cleanup":
		return cleanupCmd(argv[1:], out)
	case "sanitize-commit":
		return sanitizeCommitCmd(argv[1:])
	case "supervise":
		return superviseCmd(argv[1:], out)
	case "version", "--version", "-v":
		fmt.Fprintln(out, version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", argv[0], usage)
	}
}

// runCmd dispatches either a single task from flags or a whole batch file.
func runCmd(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		id      = fs.String("id", "", "task id; names the branch, worktree, log and state file")
		prompt  = fs.String("prompt", "", "what the agent should do; layered on top of --source when both are given")
		src     = fs.String("source", "", "github://owner/repo#N, jira://KEY-1, or free text; fetched at launch")
		repo    = fs.String("repo", ".", "path to the repository to work in")
		branch  = fs.String("branch", "", "branch to create (default agent-orc/<id>)")
		base    = fs.String("base-branch", "", "branch to cut from (default: the repo's default branch)")
		cliName = fs.String("cli", "", "agentic CLI to dispatch to: "+strings.Join(cliNames(), ", ")+" (required)")
		model   = fs.String("model", "", "model for that CLI (default: the CLI's own default)")
		subs    = fs.Bool("subagents", false, "seed the CLI's subagent definitions into the worktree")
		usd     = fs.Float64("budget-usd", 0, "cap spend in dollars, where the CLI supports it")
		credits = fs.Float64("budget-credits", 0, "cap spend in the CLI's own credit unit, where it supports it")
		noPR    = fs.Bool("no-auto-pr", false, "do not open a draft PR when the agent finishes")
		rev     = fs.Bool("review", false, "allow 'agent-orc review' to run for this task")
		revCLI  = fs.String("review-cli", "", "CLI to review with (default: one other than --cli)")
		revMdl  = fs.String("review-model", "", "model to review with")
		revAuto = fs.Bool("auto-review", false, "review automatically when the agent finishes, before the PR is opened")
		dco     = fs.Bool("dco-signoff", false, "add a Signed-off-by trailer to commits missing one")
	)
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: agent-orc run --id <id> --cli <name> [--prompt <text>] [--source <ref>] [flags]")
		fmt.Fprintln(out, "       agent-orc run <tasks.yaml>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		// -h and --help ask for the usage the flag package has just printed;
		// that is the command doing its job, not a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ctx := context.Background()
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	d, err := orc.NewDispatcher(layout, out)
	if err != nil {
		return err
	}

	// Only the flags actually typed count as set: a bool left at false and a
	// bool passed as false are the same value, and defaults must not be able
	// to overrule the second.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if path := fs.Arg(0); path != "" {
		// A batch file carries every setting itself, so any flag given
		// alongside it would be silently dropped. Visit reports only the flags
		// actually passed, so a default like --repo does not trip this.
		var names []string
		for name := range given {
			names = append(names, "--"+name)
		}
		if len(names) > 0 {
			sort.Strings(names)
			return fmt.Errorf("a batch file takes its settings from the file; drop %s", strings.Join(names, ", "))
		}
		return runBatch(ctx, d, layout, path)
	}

	f := flags{
		id: *id, source: *src, prompt: *prompt, repo: *repo,
		branch: *branch, base: *base, cli: *cliName, model: *model,
		subagents: *subs, budgetUSD: *usd, budgetCredits: *credits,
		autoPR: !*noPR, dcoSignoff: *dco,
		review: task.Review{
			Enabled: *rev || *revAuto, Auto: *revAuto,
			CLI: task.CLI(*revCLI), Model: *revMdl,
		},
	}
	settings, err := loadSettings(layout, f.repo)
	if err != nil {
		return err
	}
	f.applyDefaults(settings, given)

	t, err := buildTask(f)
	if err != nil {
		return err
	}
	return d.Run(ctx, t)
}

// runBatch loads a batch file and dispatches every task in it.
func runBatch(ctx context.Context, d *orc.Dispatcher, layout paths.Layout, path string) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	// The repository layer comes from the batch file's own repo, which is
	// resolved to an absolute path by Load.
	settings, err := loadSettings(layout, f.Repo)
	if err != nil {
		return err
	}
	f.LayerUnder(settings)
	return d.RunBatch(ctx, f, resolveGitDefaults)
}

// loadSettings returns the defaults a task inherits, broadest first: the
// machine-wide file, then the repository's own. Neither has to exist.
func loadSettings(layout paths.Layout, repo string) (config.Settings, error) {
	global, err := config.LoadSettings(layout.DefaultsFile())
	if err != nil {
		return config.Settings{}, err
	}
	local, err := config.LoadRepoSettings(repo)
	if err != nil {
		return config.Settings{}, err
	}
	return global.Merge(local), nil
}

// flags carries the single-task run flags as parsed.
type flags struct {
	id, source, prompt, repo, branch, base, cli, model string
	subagents, autoPR, dcoSignoff                      bool
	budgetUSD, budgetCredits                           float64
	testCommand                                        string
	testTimeout                                        time.Duration
	review                                             task.Review
}

// applyDefaults fills in every flag the user did not type from the settings
// files. Flags win outright: the files are there to stop the same options
// being retyped, not to change what an explicit option means.
//
// The flag names are the ones the FlagSet registers, so the mapping between a
// setting and the flag it stands in for is spelled out here in one place.
func (f *flags) applyDefaults(s config.Settings, given map[string]bool) {
	if !given["cli"] && s.CLI != "" {
		f.cli = string(s.CLI)
	}
	if !given["model"] && s.Model != "" {
		f.model = s.Model
	}
	if !given["base-branch"] && s.BaseBranch != "" {
		f.base = s.BaseBranch
	}
	if !given["subagents"] && s.Subagents != nil {
		f.subagents = *s.Subagents
	}
	if !given["budget-usd"] && s.BudgetUSD != nil {
		f.budgetUSD = *s.BudgetUSD
	}
	if !given["budget-credits"] && s.BudgetCredits != nil {
		f.budgetCredits = *s.BudgetCredits
	}
	// The flag is --no-auto-pr and the setting is auto_pr, so the sense flips.
	if !given["no-auto-pr"] && s.AutoPR != nil {
		f.autoPR = *s.AutoPR
	}
	if !given["dco-signoff"] && s.DCOSignoff != nil {
		f.dcoSignoff = *s.DCOSignoff
	}
	// There is no flag for the test command: it is discovered from the
	// repository, and the setting is only for the projects that discovery does
	// not describe. That is a fact about a repository, not about one run of
	// one task, so it belongs in a file rather than on a command line.
	if s.TestCommand != "" {
		f.testCommand = s.TestCommand
	}
	// Already validated by the loader, so a parse failure here cannot come
	// from a settings file.
	if d, err := config.ParseTestTimeout(s.TestTimeout); err == nil {
		f.testTimeout = d
	}
	if s.Review == nil {
		return
	}
	if !given["review"] && !given["auto-review"] && s.Review.Enabled != nil {
		f.review.Enabled = *s.Review.Enabled
	}
	// Automatic review implies review: a task that reviews itself on finishing
	// is reviewed, and asking for both separately would only be a way to get
	// the combination wrong.
	if !given["auto-review"] && s.Review.Auto != nil {
		f.review.Auto = *s.Review.Auto
		f.review.Enabled = f.review.Enabled || f.review.Auto
	}
	if !given["review-cli"] && s.Review.CLI != "" {
		f.review.CLI = s.Review.CLI
	}
	if !given["review-model"] && s.Review.Model != "" {
		f.review.Model = s.Review.Model
	}
}

// buildTask applies the defaults for a single command-line task.
func buildTask(f flags) (task.Task, error) {
	if strings.TrimSpace(f.id) == "" {
		return task.Task{}, errors.New("--id is required")
	}
	if strings.TrimSpace(f.prompt) == "" && strings.TrimSpace(f.source) == "" {
		return task.Task{}, errors.New("one of --prompt or --source is required")
	}

	if strings.TrimSpace(f.cli) == "" {
		return task.Task{}, errors.New("--cli is required; pass it, or set 'cli' in a defaults file so every run does not have to")
	}

	// A zero budget means "not set" rather than "cap at nothing", so the flag
	// only becomes a budget once it is given a value.
	var budget task.Budget
	if f.budgetUSD != 0 {
		budget.USD = &f.budgetUSD
	}
	if f.budgetCredits != 0 {
		budget.Credits = &f.budgetCredits
	}

	return resolveGitDefaults(task.Task{
		ID:          f.id,
		Source:      f.source,
		Prompt:      f.prompt,
		Repo:        f.repo,
		Branch:      f.branch,
		BaseBranch:  f.base,
		CLI:         task.CLI(f.cli),
		Model:       f.model,
		Subagents:   f.subagents,
		Budget:      budget,
		AutoPR:      f.autoPR,
		DCOSignoff:  f.dcoSignoff,
		Review:      f.review,
		TestCommand: f.testCommand,
		TestTimeout: f.testTimeout,
	})
}

// logsCmd prints or tails a task's log.
func logsCmd(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(out)
	follow := fs.Bool("f", false, "keep printing until the task finishes")
	raw := fs.Bool("raw", false, "print the log verbatim instead of summarizing it")
	id, err := parseAround(fs, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w\nusage: agent-orc logs <task-id> [-f] [--raw]", err)
	}
	if id == "" {
		return errors.New("usage: agent-orc logs <task-id> [-f] [--raw]")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	return orc.NewReporter(layout.State, out).Logs(id, *follow, *raw)
}

// parseAround parses flags that appear on either side of a single positional
// argument, and returns that argument. Go's flag package stops parsing at the
// first non-flag, so `logs <id> -f` would otherwise be rejected even though it
// is the form the usage lines advertise and the one people type. Parsing what
// is left over after the positional picks up the trailing flags, and works for
// flags that take a value as well as boolean ones.
//
// That second parse resets the FlagSet's leftover arguments, so callers must
// use the returned value and not fs.Arg or fs.NArg afterwards: those no longer
// describe the positional this consumed. An explicit -- is honoured: after one,
// nothing is reparsed, so an id beginning with a dash can still be passed.
func parseAround(fs *flag.FlagSet, argv []string) (string, error) {
	if err := fs.Parse(argv); err != nil {
		return "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil
	}
	// An explicit -- means everything after it is positional. Re-parsing those
	// as flags would override what the caller just said, so the second pass is
	// only for the case where no marker was given.
	if slices.Contains(argv, "--") {
		if len(rest) > 1 {
			return "", fmt.Errorf("unexpected argument %q", rest[1])
		}
		return rest[0], nil
	}
	positional := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return positional, nil
}

// prCmd runs the sanitize, push and draft-PR chain by hand.
func prCmd(argv []string, out io.Writer) error {
	if len(argv) != 1 {
		return errors.New("usage: agent-orc pr <task-id>")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	p, err := orc.NewPublisher(layout, out)
	if err != nil {
		return err
	}
	return p.Publish(argv[0])
}

// reviewCmd runs review rounds for a task until the reviewer approves the
// branch or the task's cap is reached.
func reviewCmd(argv []string, out io.Writer) error {
	if len(argv) != 1 {
		return errors.New("usage: agent-orc review <task-id>")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	return orc.NewReviewer(layout, out).Review(argv[0])
}

// cleanupCmd removes a task's worktree and local state.
func cleanupCmd(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(out)
	all := fs.Bool("all", false, "clean up every task that is not running")
	force := fs.Bool("force", false, "discard uncommitted work and remove logs too")
	delBranch := fs.Bool("delete-branch", false, "delete the task's branch too, if its commits are merged or pushed")
	id, err := parseAround(fs, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w\nusage: agent-orc cleanup <task-id|--all> [--force] [--delete-branch]", err)
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	c := orc.NewCleaner(layout, out)
	if *all {
		if id != "" {
			return errors.New("pass either a task id or --all, not both")
		}
		return c.CleanAll(*force, *delBranch)
	}
	if id == "" {
		return errors.New("usage: agent-orc cleanup <task-id|--all> [--force]")
	}
	return c.Clean(id, *force, *delBranch)
}

// sanitizeCommitCmd applies the commit policy to HEAD. The sanitization rebase
// execs it once per commit; it is not meant to be typed by hand.
func sanitizeCommitCmd(argv []string) error {
	if len(argv) != 0 {
		return errors.New("usage: agent-orc sanitize-commit")
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	return sanitize.AmendHead(wd)
}

// statusCmd prints the task table.
func statusCmd(argv []string, out io.Writer) error {
	if len(argv) != 0 {
		return errors.New("usage: agent-orc status")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	return orc.NewReporter(layout.State, out).Status()
}

// stopCmd kills a running task.
func stopCmd(argv []string, out io.Writer) error {
	if len(argv) != 1 {
		return errors.New("usage: agent-orc stop <task-id>")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	return orc.NewReporter(layout.State, out).Stop(argv[0])
}

// resolveGitDefaults fills in the fields that need git or the filesystem to
// work out: the repository's absolute path, the base branch, and the branch
// name. It is shared by the single-task and batch paths so both get the same
// defaults from the same code.
func resolveGitDefaults(t task.Task) (task.Task, error) {
	if t.Repo == "" {
		t.Repo = "."
	}
	abs, err := filepath.Abs(t.Repo)
	if err != nil {
		return t, fmt.Errorf("resolving repo %q: %w", t.Repo, err)
	}
	r, err := gitx.Open(abs)
	if err != nil {
		return t, err
	}
	t.Repo = r.Dir

	if t.BaseBranch == "" {
		if t.BaseBranch, err = r.DefaultBranch(); err != nil {
			return t, err
		}
	}
	if t.Branch == "" {
		t.Branch = task.DefaultBranch(t.ID)
	}
	return t, t.ValidateSpec()
}

// cliNames lists the dispatchable CLIs for help text.
func cliNames() []string {
	names := make([]string, 0, len(task.KnownCLIs))
	for _, c := range task.KnownCLIs {
		names = append(names, string(c))
	}
	return names
}

// superviseCmd runs the per-task supervisor. It is spawned by 'run', so it is
// left out of the usage text.
func superviseCmd(argv []string, out io.Writer) error {
	if len(argv) != 1 {
		return errors.New("usage: agent-orc supervise <task-id>")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	return orc.NewSupervisor(layout, out).Supervise(argv[0])
}
