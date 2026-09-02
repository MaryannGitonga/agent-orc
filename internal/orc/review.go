package orc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/review"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Reviewer runs bounded worker↔reviewer round-trips for a task.
type Reviewer struct {
	layout paths.Layout
	store  *state.Store
	out    io.Writer
}

// NewReviewer returns a reviewer writing progress to out.
func NewReviewer(layout paths.Layout, out io.Writer) *Reviewer {
	return &Reviewer{layout: layout, store: state.NewStore(layout.State), out: out}
}

// Review runs review rounds until the reviewer approves the branch or the
// per-task cap is reached, whichever comes first.
func (r *Reviewer) Review(id string) error {
	record, err := r.store.Load(id)
	if err != nil {
		return err
	}
	if !record.Review.Enabled {
		return fmt.Errorf("task %q does not have review enabled", id)
	}
	if record.Status.HasProcess() {
		return fmt.Errorf("task %q is still %s; review it once the agent has finished", id, record.Status)
	}
	if _, err := os.Stat(record.Worktree); err != nil {
		return fmt.Errorf("task %q has no worktree left to resume the worker in: %w", id, err)
	}

	maxRounds := record.Review.Rounds()
	if record.ReviewRound >= maxRounds {
		return fmt.Errorf("task %q has already used its %d review round(s); the rest is for a human",
			id, maxRounds)
	}

	for record.ReviewRound < maxRounds {
		approved, err := r.round(&record)
		if err != nil {
			return err
		}
		if approved {
			return r.store.Update(id, func(k *state.Task) { k.Status = state.StatusReviewed })
		}
	}

	fmt.Fprintf(r.out, "%s  review cap of %d round(s) reached; leaving the rest to a human\n", id, maxRounds)
	return nil
}

// round runs one review, and hands any comments back to the worker.
func (r *Reviewer) round(record *state.Task) (bool, error) {
	round := record.ReviewRound + 1
	fmt.Fprintf(r.out, "%s  review round %d of %d\n", record.ID, round, record.Review.Rounds())

	output, err := r.runReviewer(*record)
	if err != nil {
		return false, err
	}

	verdict, err := review.ParseVerdict(output)
	if err != nil {
		// §15: an unreadable verdict stops and waits for a human rather than
		// being guessed at in either direction.
		return false, fmt.Errorf("task %q round %d: %w; see %s",
			record.ID, round, err, r.layout.ReviewLogFile(record.ID))
	}

	if verdict.Approved {
		if err := r.bump(record, round); err != nil {
			return false, err
		}
		fmt.Fprintf(r.out, "%s  reviewer approved the branch\n", record.ID)
		return true, nil
	}

	fmt.Fprintf(r.out, "%s  reviewer raised %d comment(s):\n", record.ID, len(verdict.Comments))
	for _, c := range verdict.Comments {
		fmt.Fprintf(r.out, "    - %s\n", c)
	}

	if err := r.resumeWorker(*record, verdict.Comments); err != nil {
		return false, err
	}
	if err := r.bump(record, round); err != nil {
		return false, err
	}
	fmt.Fprintf(r.out, "%s  worker addressed the comments\n", record.ID)
	return false, nil
}

// bump records a completed round.
func (r *Reviewer) bump(record *state.Task, round int) error {
	record.ReviewRound = round
	return r.store.Update(record.ID, func(k *state.Task) { k.ReviewRound = round })
}

// runReviewer checks the branch out into a fresh worktree, runs the reviewing
// CLI there in a clean session, and returns its output.
func (r *Reviewer) runReviewer(record state.Task) (string, error) {
	repo, err := gitx.Open(record.Repo)
	if err != nil {
		return "", err
	}
	dir := r.layout.ReviewWorktree(record.ID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clearing the previous review worktree: %w", err)
	}
	// Removing the directory does not remove git's record of it. A review that
	// was killed before its cleanup ran leaves that record behind, and every
	// later review then fails with "missing but already registered". Pruning
	// clears exactly those orphaned records and leaves live worktrees alone.
	if out, err := runGit(repo.Dir, "worktree", "prune"); err != nil {
		return "", fmt.Errorf("pruning stale worktree records: %w: %s", err, out)
	}
	if err := os.MkdirAll(r.layout.Reviews, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", r.layout.Reviews, err)
	}
	if out, err := runGit(repo.Dir, "worktree", "add", "--detach", dir, record.Branch); err != nil {
		return "", fmt.Errorf("creating the review worktree: %w: %s", err, out)
	}
	// The reviewer's checkout is disposable; only the worker's branch persists.
	defer func() {
		_, _ = runGit(repo.Dir, "worktree", "remove", "--force", dir)
	}()

	cli := record.ReviewerCLI()
	a, err := adapter.For(cli)
	if err != nil {
		return "", err
	}
	reviewTask := task.Task{
		Prompt: review.ReviewerPrompt(record.Prompt, record.BaseBranch),
		Model:  record.Review.Model,
		CLI:    cli,
		// The reviewer draws on the same per-task budget as the worker, so
		// there is one number to watch, not two.
		Budget: record.Budget,
	}
	// The reviewer must see the diff, not the worker's reasoning, so its
	// prompt goes in raw rather than through the worker's operating rules.
	argv := a.BuildCommand(reviewTask)

	fmt.Fprintf(r.out, "%s  reviewing with %s", record.ID, cli)
	if reviewTask.Model != "" {
		fmt.Fprintf(r.out, " (%s)", reviewTask.Model)
	}
	fmt.Fprintln(r.out)

	return r.capture(record.ID, dir, argv, r.layout.ReviewLogFile(record.ID))
}

// resumeWorker hands the reviewer's comments back to the worker's own session.
func (r *Reviewer) resumeWorker(record state.Task, comments []string) error {
	a, err := adapter.For(record.CLI)
	if err != nil {
		return err
	}
	argv, err := a.ResumeCommand(record.SessionID, review.FeedbackPrompt(comments), record.Model)
	if err != nil {
		return fmt.Errorf("task %q: %w", record.ID, err)
	}
	_, err = r.capture(record.ID, record.Worktree, argv, record.LogPath)
	return err
}

// capture runs a command, appends its output to a log, and returns it.
func (r *Reviewer) capture(id, dir string, argv []string, logPath string) (string, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer logFile.Close()

	var buf bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv comes from an adapter
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(&buf, logFile)
	cmd.Stderr = io.MultiWriter(&buf, logFile)
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("task %q: %s exited with an error: %w: %s",
			id, argv[0], err, strings.TrimSpace(lastLines(buf.String(), 5)))
	}
	return buf.String(), nil
}

// lastLines returns the final n non-empty lines, for error messages.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// runGit runs a git command in dir and returns its combined output.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
