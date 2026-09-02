package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/config"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/task"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

const usage = `agent-orc: dispatch agentic CLI runs across isolated git worktrees

Usage:
  agent-orc run [flags]         dispatch a single task
  agent-orc run <tasks.yaml>    dispatch every task in a batch file
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

	if path := fs.Arg(0); path != "" {
		if *id != "" || *prompt != "" || *src != "" {
			return errors.New("pass either a batch file or the single-task flags, not both")
		}
		return runBatch(ctx, d, path)
	}

	t, err := buildTask(*id, *src, *prompt, *repo, *branch, *base, *cliName, *model)
	if err != nil {
		return err
	}
	return d.Run(ctx, t)
}

// runBatch loads a batch file and dispatches every task in it.
func runBatch(ctx context.Context, d *orc.Dispatcher, path string) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	return d.RunBatch(ctx, f, resolveGitDefaults)
}

// buildTask applies the defaults for a single command-line task.
func buildTask(id, src, prompt, repo, branch, base, cliName, model string) (task.Task, error) {
	if strings.TrimSpace(id) == "" {
		return task.Task{}, errors.New("--id is required")
	}
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(src) == "" {
		return task.Task{}, errors.New("one of --prompt or --source is required")
	}

	if strings.TrimSpace(cliName) == "" {
		return task.Task{}, errors.New("--cli is required")
	}
	return resolveGitDefaults(task.Task{
		ID:         id,
		Source:     src,
		Prompt:     prompt,
		Repo:       repo,
		Branch:     branch,
		BaseBranch: base,
		CLI:        task.CLI(cliName),
		Model:      model,
	})
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
