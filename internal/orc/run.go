// Package orc wires the pieces together: it creates a task's worktree, hands
// it to a detached supervisor process, and records what it did.
package orc

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if !repo.RevExists(t.BaseBranch) {
		return fmt.Errorf("base branch %q does not exist in %s", t.BaseBranch, repo.Dir)
	}
	if repo.BranchExists(t.Branch) {
		return fmt.Errorf("branch %q already exists in %s; pick another --branch", t.Branch, repo.Dir)
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
	sessionID, err := adapter.NewSessionID()
	if err != nil {
		_ = repo.RemoveWorktree(worktree, true)
		return err
	}
	if len(a.SessionArgs(sessionID)) == 0 {
		sessionID = ""
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
