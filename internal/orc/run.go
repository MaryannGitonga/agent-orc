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
	if err := t.ValidateSpec(); err != nil {
		return fmt.Errorf("invalid task %q: %w", t.ID, err)
	}
	t, err := d.resolvePrompt(ctx, t)
	if err != nil {
		return err
	}
	if t.Instructions, err = d.standingInstructions(t); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("invalid task %q: %w", t.ID, err)
	}
	if err := d.layout.Ensure(); err != nil {
		return err
	}
	// Only a missing record means the id is free. Any other load failure is a
	// record that exists but cannot be read, and overwriting it would destroy
	// whatever it was tracking.
	switch _, err := d.store.Load(t.ID); {
	case err == nil:
		return fmt.Errorf("task %q already exists; pick another --id or run 'agent-orc cleanup %s'", t.ID, t.ID)
	case !errors.Is(err, state.ErrNotFound):
		return fmt.Errorf("checking for an existing task %q: %w", t.ID, err)
	}

	// Check the agent's binary before touching the repository. The supervisor
	// would otherwise fail on exec, after a worktree, a branch and a state file
	// already exist for a task that never had a chance to run.
	bin, err := agentBinary(t)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("task %q needs %s, but %q is not on PATH: %w", t.ID, t.CLI, bin, err)
	}

	repo, err := gitx.Open(t.Repo)
	if err != nil {
		return err
	}
	t, testNote := resolveTestCommand(t)
	if !repo.RevExists(t.BaseBranch) {
		return fmt.Errorf("base branch %q does not exist in %s", t.BaseBranch, repo.Dir)
	}
	if repo.BranchExists(t.Branch) {
		// The branch outliving its task is the normal case, since cleanup
		// leaves it behind on purpose, so this fires most often on a retry of
		// an id that was cleaned up. Name the command that clears it: by now
		// the state file is gone, so 'agent-orc cleanup' no longer knows the
		// branch and cannot be the answer.
		return fmt.Errorf("branch %q already exists in %s; delete it with %s if it holds nothing you want, or pick another --branch",
			t.Branch, repo.Dir,
			shellCommand("git", "-C", repo.Dir, "branch", "-D", "--", t.Branch))
	}

	worktree := d.layout.Worktree(t.ID)
	if _, err := os.Stat(worktree); err == nil {
		return fmt.Errorf("worktree %s already exists; run 'agent-orc cleanup %s' first", worktree, t.ID)
	}
	a, err := adapter.For(t.CLI)
	if err != nil {
		return err
	}
	if err := repo.AddWorktree(worktree, t.Branch, t.BaseBranch); err != nil {
		return err
	}

	seeded, err := d.seedSubagents(t, a, worktree)
	if err != nil {
		// Nothing is running yet. A task that asked for subagents and did not
		// get them would run differently from what was asked for, so undo the
		// worktree and fail rather than launching it anyway.
		_ = repo.RemoveWorktree(worktree, true)
		return err
	}

	// The session ID is assigned here rather than discovered afterwards, so a
	// CLI that accepts one is resumable even if it says nothing about its own
	// session. That is what lets review feedback go back to this session.
	// Only mint an ID for a CLI that can actually pin a session to one. The
	// probe value is discarded; it just asks the adapter whether it emits
	// session flags at all, so a CLI like Codex neither carries an unusable id
	// nor can fail a launch on generating one.
	var sessionID string
	if len(a.SessionArgs("probe")) > 0 {
		if sessionID, err = adapter.NewSessionID(); err != nil {
			_ = repo.RemoveWorktree(worktree, true)
			return err
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
		// Nothing is running yet, so undo both halves of what AddWorktree did.
		// The branch has to go too: left behind, it is an empty branch at base
		// that makes a retry with the same id fail on the collision check.
		_ = repo.RemoveWorktree(worktree, true)
		_ = repo.DeleteBranch(t.Branch)
		return err
	}

	// A task id is reusable once its predecessor has been cleaned up, and the
	// log paths are derived from the id alone, so a stale log would otherwise
	// be appended to and 'agent-orc logs' would open with the previous run's
	// output. Everything within one task still appends: the review rounds
	// write into the same files as the worker.
	if err := d.truncateLogs(t.ID); err != nil {
		// The record already exists, so returning here without marking it
		// would leave a task that is pending forever: nothing to stop, since
		// there is no pid, and an id that is taken. Fail it the way the
		// startSupervisor path below does, so ordinary cleanup can recover it.
		_ = d.store.Update(t.ID, func(k *state.Task) {
			k.Status = state.StatusFailed
			k.Error = err.Error()
		})
		return err
	}

	if err := d.startSupervisor(t.ID); err != nil {
		_ = d.store.Update(t.ID, func(k *state.Task) {
			k.Status = state.StatusFailed
			k.Error = err.Error()
		})
		return err
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

// seedSubagents copies the CLI's subagent definitions into the worktree when
// the task asked for them, and returns what it copied.
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
// did not say.
//
// Nobody should have to tell agent-orc something the repository already
// states: a project that has a make test target, or a go.mod, or a pytest
// layout has said how it is tested, and reading that is better than asking for
// it again in a config file. The explicit setting stays for the projects those
// conventions do not describe, and "none" is how a repository whose tests
// agent-orc should not run says so.
func resolveTestCommand(t task.Task) (task.Task, string) {
	switch strings.TrimSpace(t.TestCommand) {
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

// truncateLogs clears whatever a previous task of the same id left behind. It
// is called once the launch is certain, so a run that fails its checks leaves
// the earlier task's record readable.
//
// The files are unlinked rather than truncated in place. An orphaned supervisor
// from the previous task can still hold one of them open, and its handle is in
// append mode, so truncating would leave that writer appending into the file
// the new run is using and interleave two tasks' output. Unlinking leaves the
// old handle writing into an inode nobody can reach, which goes away when it
// closes, and the new run opens a file of its own.
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
func (d *Dispatcher) RunBatch(ctx context.Context, f *config.File, prepare func(task.Task) (task.Task, error)) error {
	var errs []error
	launched := 0
	for _, entry := range f.Tasks {
		t, err := prepare(f.Resolved(entry))
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
