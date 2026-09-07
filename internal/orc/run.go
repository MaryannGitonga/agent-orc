// Package orc wires the pieces together: it creates a task's worktree, hands
// it to a detached supervisor process, and records what it did.
package orc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/config"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/seed"
	"github.com/MaryannGitonga/agent-orc/internal/source"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
	"github.com/MaryannGitonga/agent-orc/internal/testcmd"
)

// fetchTimeout bounds resolving a task's source at launch.
const fetchTimeout = 60 * time.Second

// Dispatcher launches tasks; build one with [NewDispatcher].
type Dispatcher struct {
	layout paths.Layout
	store  *state.Store
	// self is the agent-orc binary to re-exec as the supervisor.
	self string
	out  io.Writer
	// Resolver fetches a task's source. Replaceable for tests.
	Resolver *source.Resolver
}

// NewDispatcher returns a dispatcher writing progress to out.
func NewDispatcher(layout paths.Layout, out io.Writer) (*Dispatcher, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the agent-orc binary: %w", err)
	}
	return &Dispatcher{
		layout:   layout,
		store:    state.NewStore(layout.State),
		self:     self,
		out:      out,
		Resolver: source.NewResolver(),
	}, nil
}

// Run prepares a worktree for t and starts a detached supervisor in it. It
// returns as soon as the supervisor is up; the agent keeps going after that.
func (d *Dispatcher) Run(ctx context.Context, t task.Task) error {
	t, err := d.resolve(ctx, t)
	if err != nil {
		return err
	}
	plan, err := d.preflight(t)
	if err != nil {
		return err
	}
	return d.launch(plan)
}

// resolve turns a dispatched task into the one that will actually run: the
// prompt its source describes, the standing instructions around it, and the
// state directories it needs.
func (d *Dispatcher) resolve(ctx context.Context, t task.Task) (task.Task, error) {
	if err := t.ValidateSpec(); err != nil {
		return t, fmt.Errorf("invalid task %q: %w", t.ID, err)
	}
	t, err := d.resolvePrompt(ctx, t)
	if err != nil {
		return t, err
	}
	if t.Instructions, err = d.standingInstructions(t); err != nil {
		return t, err
	}
	if err := t.Validate(); err != nil {
		return t, fmt.Errorf("invalid task %q: %w", t.ID, err)
	}
	return t, d.layout.Ensure()
}

// ResolveGitDefaults fills in the fields that need git or the filesystem to
// work out: the repository's absolute path, the base branch, and the branch
// name. It is shared by the single-task and batch paths so both get the same
// defaults from the same code.
func ResolveGitDefaults(t task.Task) (task.Task, error) {
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

// launchPlan is what preflight settled: the task as it will run, and what
// launch needs so it does not look any of it up again.
type launchPlan struct {
	task     task.Task
	repo     *gitx.Repo
	adapter  adapter.Adapter
	testNote string
}

// preflight answers whether the task can run, before anything on disk is made
// for it: the id is free, the CLI is installed, and the repository has room for
// the branch and worktree it wants.
//
// Ordering is the point. A worktree, a branch and a state file left behind by a
// task that never had a chance to start are worse than the error itself.
func (d *Dispatcher) preflight(t task.Task) (launchPlan, error) {
	fail := func(err error) (launchPlan, error) { return launchPlan{}, err }

	// Only a missing record means the id is free. Any other load failure is a
	// record that exists but cannot be read, and overwriting it would destroy
	// whatever it was tracking.
	switch _, err := d.store.Load(t.ID); {
	case err == nil:
		return fail(fmt.Errorf("task %q already exists; pick another --id or run 'agent-orc cleanup %s'", t.ID, t.ID))
	case !errors.Is(err, state.ErrNotFound):
		return fail(fmt.Errorf("checking for an existing task %q: %w", t.ID, err))
	}

	a, err := adapter.For(t.CLI)
	if err != nil {
		return fail(err)
	}
	bin, err := agentBinary(t)
	if err != nil {
		return fail(err)
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fail(fmt.Errorf("task %q needs %s, but %q is not on PATH: %w", t.ID, t.CLI, bin, err))
	}

	repo, err := gitx.Open(t.Repo)
	if err != nil {
		return fail(err)
	}
	if !repo.RevExists(t.BaseBranch) {
		return fail(fmt.Errorf("base branch %q does not exist in %s", t.BaseBranch, repo.Dir))
	}
	if repo.BranchExists(t.Branch) {
		return fail(fmt.Errorf("branch %q already exists in %s; delete it with %s if it holds nothing you want, or pick another --branch",
			t.Branch, repo.Dir,
			shellCommand("git", "-C", repo.Dir, "branch", "-D", "--", t.Branch)))
	}
	if worktree := d.layout.Worktree(t.ID); !isMissing(worktree) {
		return fail(fmt.Errorf("worktree %s already exists; run 'agent-orc cleanup %s' first", worktree, t.ID))
	}

	// The repository root, whatever --repo said, so discovery reads the
	// repository's own markers and the record names the repository.
	t.Repo = repo.Dir
	t, testNote := resolveTestCommand(t)
	return launchPlan{task: t, repo: repo, adapter: a, testNote: testNote}, nil
}

// launch creates the task's worktree, record and supervisor, undoing whatever
// it has already made if a later step fails.
func (d *Dispatcher) launch(plan launchPlan) error {
	t, repo, a, testNote := plan.task, plan.repo, plan.adapter, plan.testNote
	worktree := d.layout.Worktree(t.ID)
	if err := repo.AddWorktree(worktree, t.Branch, t.BaseBranch); err != nil {
		return err
	}
	// From here on a failure has something to undo, so each one rolls back
	// what it found rather than leaving a half-made task behind.
	undo := func(err error) error {
		_ = repo.RemoveWorktree(worktree, true)
		_ = repo.DeleteBranch(t.Branch)
		return err
	}

	seeded, err := d.seedSubagents(t, a, worktree)
	if err != nil {
		return undo(err)
	}
	// Only for a CLI that takes one at launch, so a CLI like Codex neither
	// carries an unusable id nor fails on generating one.
	var sessionID string
	if len(a.SessionArgs("probe")) > 0 {
		if sessionID, err = adapter.NewSessionID(); err != nil {
			return undo(err)
		}
	}

	_, budgetNote := a.BudgetArgs(t.Budget)
	record := state.Task{
		Task:         t,
		Status:       state.StatusPending,
		Worktree:     worktree,
		LogPath:      d.layout.LogFile(t.ID),
		StartedAt:    time.Now().UTC(),
		BudgetNote:   budgetNote,
		SeededAgents: seeded,
		SessionID:    sessionID,
	}
	if err := d.store.Save(record); err != nil {
		return undo(err)
	}

	// The record exists now, so a failure past this point is recorded on it
	// rather than rolled back: the task is real, it simply never started.
	markFailed := func(err error) error {
		_ = d.store.Update(t.ID, func(k *state.Task) {
			k.Status = state.StatusFailed
			k.Error = err.Error()
		})
		return err
	}
	if err := d.truncateLogs(t.ID); err != nil {
		return markFailed(err)
	}
	if err := d.startSupervisor(t.ID); err != nil {
		return markFailed(err)
	}

	fmt.Fprintf(d.out, "%s  started\n", t.ID)
	fmt.Fprintf(d.out, "  branch    %s (from %s)\n", t.Branch, t.BaseBranch)
	fmt.Fprintf(d.out, "  worktree  %s\n", worktree)
	fmt.Fprintf(d.out, "  log       %s\n", record.LogPath)
	fmt.Fprintf(d.out, "  tests     %s\n", testNote)
	if len(seeded) > 0 {
		fmt.Fprintf(d.out, "  subagents %s\n", strings.Join(seeded, ", "))
	}
	if budgetNote != "" {
		fmt.Fprintf(d.out, "  warning   %s\n", budgetNote)
	}
	return nil
}

// isMissing reports whether nothing exists at path.
func isMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func (d *Dispatcher) seedSubagents(t task.Task, a adapter.Adapter, worktree string) ([]string, error) {
	if !t.Subagents {
		return nil, nil
	}
	dir := a.SubagentDir()
	if dir == "" {
		return nil, fmt.Errorf("task %q asks for subagents but %s has no subagent mechanism to seed", t.ID, t.CLI)
	}
	lib := seed.Library{Root: filepath.Join(d.layout.Root, "agents")}
	src, err := lib.Source(string(t.CLI), t.Repo, dir)
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", t.ID, err)
	}
	copied, err := seed.Into(worktree, dir, src)
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", t.ID, err)
	}
	if err := seed.Ignore(worktree, dir); err != nil {
		return nil, fmt.Errorf("task %q: %w", t.ID, err)
	}
	d.warnIfTracked(t, worktree, dir)
	return copied, nil
}

// warnIfTracked reports seeded definitions that landed on files the repository
// already tracks. The .gitignore seed.Ignore writes cannot hide those, so they
// stay visible as modifications the agent could commit. Warn rather than fail:
// the run is still valid, the user just needs to know the seeding is not
// invisible in this repository.
func (d *Dispatcher) warnIfTracked(t task.Task, worktree, dir string) {
	wt, err := gitx.Open(worktree)
	if err != nil {
		return
	}
	tracked, err := wt.TrackedUnder(dir)
	if err != nil || len(tracked) == 0 {
		return
	}
	fmt.Fprintf(d.out, "warning: %s already tracks %d file(s) under %s; seeded definitions there cannot be ignored and may be committed\n",
		t.Repo, len(tracked), dir)
}

// startSupervisor re-execs agent-orc as a detached supervisor. Its own session
// is what lets the agent survive the dispatching terminal going away, so a
// batch can run unattended without a daemon.
func (d *Dispatcher) startSupervisor(id string) error {
	logFile, err := os.OpenFile(d.layout.SupervisorLogFile(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening supervisor log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(d.self, "supervise", id)
	cmd.Dir = filepath.Dir(d.layout.Root)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting supervisor for %q: %w", id, err)
	}
	// The supervisor is running from here on and owns the task's state. Release
	// only drops this process's handle on it, so a failure is worth reporting
	// but must not be returned: the caller marks the task failed on error, and
	// writing that would race the state the supervisor is already updating.
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(d.out, "warning: could not release supervisor %d: %v\n", cmd.Process.Pid, err)
	}
	return nil
}

// resolvePrompt fetches the task's source, if it has one, and layers the
// task's own prompt on top of it.
//
// This happens before anything is created on disk: a failed fetch must be a
// clean launch-time error, not a half-set-up task with an orphaned worktree.
func (d *Dispatcher) resolvePrompt(ctx context.Context, t task.Task) (task.Task, error) {
	// Trimmed, to agree with ValidateSpec: it already counts a blank source as
	// absent, so treating one as fetchable here fails a task it just accepted.
	if strings.TrimSpace(t.Source) == "" {
		return t, nil
	}
	ref, err := source.Parse(t.Source)
	if err != nil {
		return t, fmt.Errorf("task %q: %w", t.ID, err)
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = source.NewResolver()
	}
	// The JIRA client has its own deadline, but `gh issue view` would otherwise
	// hang the launch indefinitely. Bound the fetch, not the whole run: git
	// work on a large repository is legitimately slow.
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	fetched, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return t, fmt.Errorf("task %q: %w", t.ID, err)
	}
	t.Prompt = source.Compose(fetched, t.Prompt)
	return t, nil
}

// shellCommand renders argv as a command that can be pasted into a shell.
//
// Neither of the values this is used on is safe to interpolate raw: a
// repository path may contain spaces, and git allows characters in a branch
// name that a shell would treat as syntax, so an unquoted suggestion could
// fail or run something else entirely when copied.
func shellCommand(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps s in single quotes, which a POSIX shell takes literally,
// ending and reopening them around any single quote of its own.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]#~!{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveTestCommand fills in how the repository runs its tests when the task
// did not say. A project with a make test target, a go.mod or a pytest layout
// has already stated it; the explicit setting is for projects those
// conventions do not describe, and "none" opts out.
func resolveTestCommand(t task.Task) (task.Task, string) {
	// Trimmed into the task, so the record, the logs and the phase the
	// supervisor enters agree. Deciding on a trimmed copy while storing the
	// original put tasks into verifying for a command that never ran.
	t.TestCommand = strings.TrimSpace(t.TestCommand)
	switch t.TestCommand {
	case testcmd.None:
		t.TestCommand = ""
		t.SkipTests = true
		return t, "not run for this task, and not mentioned to the agent"
	case "":
		command, reason := testcmd.Discover(t.Repo)
		if command == "" {
			return t, "none found; the agent is asked to find them itself"
		}
		t.TestCommand = command
		return t, fmt.Sprintf("%s (from %s), %s", command, reason, timeoutNote(t))
	default:
		return t, fmt.Sprintf("%s (set for this task), %s", t.TestCommand, timeoutNote(t))
	}
}

// timeoutNote says how long the test command gets, so a cap that is about to
// apply is visible before it fires rather than only in the log afterwards.
func timeoutNote(t task.Task) string {
	if d := t.TestRunTimeout(); d > 0 {
		return "killed after " + d.String()
	}
	return "uncapped"
}

// truncateLogs clears what a previous task of the same id left behind, once
// the launch is certain so a failed check leaves the earlier record readable.
//
// Unlinked rather than truncated: an orphaned supervisor may still hold one
// open in append mode, and truncating would leave it writing into the file the
// new run is using.
func (d *Dispatcher) truncateLogs(id string) error {
	for _, path := range []string{
		d.layout.LogFile(id),
		d.layout.SupervisorLogFile(id),
		d.layout.ReviewLogFile(id),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clearing %s: %w", path, err)
		}
	}
	return nil
}

// standingInstructions combines the machine-wide instructions file with
// whatever the batch set, broadest first. They add up rather than override:
// a global rule and a batch rule are both meant to apply, and a batch that
// wanted to drop a global one would be better off not setting it globally.
func (d *Dispatcher) standingInstructions(t task.Task) (string, error) {
	global, err := os.ReadFile(d.layout.InstructionsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("reading %s: %w", d.layout.InstructionsFile(), err)
	}
	parts := make([]string, 0, 2)
	for _, part := range []string{string(global), t.Instructions} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// RunBatch dispatches every task in a batch file.
//
// A task that cannot be launched does not stop the rest: the tasks are
// independent by construction, so failing the whole batch over one bad entry
// would throw away work that was fine. Every failure is reported at the end.
func (d *Dispatcher) RunBatch(ctx context.Context, f *config.File) error {
	var errs []error
	launched := 0
	for _, entry := range f.Tasks {
		t, err := ResolveGitDefaults(f.Resolved(entry))
		if err == nil {
			err = d.Run(ctx, t)
		}
		if err != nil {
			fmt.Fprintf(d.out, "%s  not launched: %v\n", entry.ID, err)
			errs = append(errs, err)
			continue
		}
		launched++
	}
	fmt.Fprintf(d.out, "\n%d of %d tasks launched\n", launched, len(f.Tasks))
	return errors.Join(errs...)
}
