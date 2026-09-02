package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/task"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

const usage = `agent-orc: dispatch agentic CLI runs across isolated git worktrees

Usage:
  agent-orc run [flags]     dispatch a single task
  agent-orc version         print the version

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

// runCmd parses the flags for a single task and dispatches it.
func runCmd(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		id      = fs.String("id", "", "task id; names the branch, worktree, log and state file (required)")
		prompt  = fs.String("prompt", "", "what the agent should do (required)")
		repo    = fs.String("repo", ".", "path to the repository to work in")
		branch  = fs.String("branch", "", "branch to create (default agent-orc/<id>)")
		base    = fs.String("base-branch", "", "branch to cut from (default: the repo's default branch)")
		cliName = fs.String("cli", "", "agentic CLI to dispatch to (required)")
		model   = fs.String("model", "", "model for that CLI (default: the CLI's own default)")
	)
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: agent-orc run --id <id> --prompt <text> --cli <name> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}

	t, err := buildTask(*id, *prompt, *repo, *branch, *base, *cliName, *model)
	if err != nil {
		return err
	}

	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	d, err := orc.NewDispatcher(layout, out)
	if err != nil {
		return err
	}
	return d.Run(t)
}

// buildTask applies the defaults that need the filesystem or git to resolve.
func buildTask(id, prompt, repo, branch, base, cliName, model string) (task.Task, error) {
	if strings.TrimSpace(id) == "" {
		return task.Task{}, errors.New("--id is required")
	}
	if strings.TrimSpace(prompt) == "" {
		return task.Task{}, errors.New("--prompt is required")
	}
	// No default CLI: agent-orc dispatches to whichever tool the user actually
	// has, and guessing one produces a task that fails after its worktree and
	// branch already exist.
	if strings.TrimSpace(cliName) == "" {
		return task.Task{}, errors.New("--cli is required")
	}

	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return task.Task{}, fmt.Errorf("resolving --repo %q: %w", repo, err)
	}
	r, err := gitx.Open(absRepo)
	if err != nil {
		return task.Task{}, err
	}

	if base == "" {
		if base, err = r.DefaultBranch(); err != nil {
			return task.Task{}, err
		}
	}
	if branch == "" {
		branch = task.DefaultBranch(id)
	}

	t := task.Task{
		ID:         id,
		Repo:       r.Dir,
		Prompt:     prompt,
		Branch:     branch,
		BaseBranch: base,
		CLI:        task.CLI(cliName),
		Model:      model,
	}
	return t, t.Validate()
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
