package orc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/review"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Reviewer runs worker and reviewer round-trips for a task, until the reviewer
// approves. What ends the loop short of that is the budget the CLIs enforce, a
// worker that stops acting on the comments, or a stop; there is no round cap.
type Reviewer struct {
	layout paths.Layout
	store  *state.Store
	out    io.Writer
	// scope ties this reviewer to one dispatch; unset for `agent-orc review`,
	// which acts on whatever the id names now.
	scope runScope
}

// NewReviewer returns a reviewer writing progress to out.
func NewReviewer(layout paths.Layout, out io.Writer) *Reviewer {
	store := state.NewStore(layout.State)
	return &Reviewer{layout: layout, store: store, out: out, scope: runScope{store: store}}
}

// mayContinue says why this reviewer should stop working on the task, or nil to
// carry on. It answers nothing for a reviewer that is not scoped to a run,
// which is what `agent-orc review` builds: that one acts on whatever the id
// names now, and there is no earlier run for it to have lost.
func (r *Reviewer) mayContinue(id string) error {
	if r.scope.since.IsZero() {
		return nil
	}
	if reason := r.scope.halted(id); reason != nil {
		return fmt.Errorf("%w; no further review rounds", reason)
	}
	return nil
}

// Review runs review rounds until the reviewer approves the branch.
func (r *Reviewer) Review(id string) error {
	record, err := r.store.Load(id)
	if err != nil {
		return err
	}
	if !record.Review.Enabled {
		return fmt.Errorf("task %q does not have review enabled", id)
	}
	// Active, not HasProcess: publishing means the sanitize, push and draft-PR
	// chain is still rewriting and pushing this branch, so a review then would
	// be reading a moving target.
	if record.Status.Active() {
		return fmt.Errorf("task %q is still %s; review it once the agent has finished", id, record.Status)
	}
	if _, err := os.Stat(record.Worktree); err != nil {
		return fmt.Errorf("task %q has no worktree left to resume the worker in: %w", id, err)
	}

	approved, err := r.Rounds(&record)
	if err != nil {
		return err
	}
	if approved {
		now := time.Now().UTC()
		return r.scope.update(id, func(k *state.Task) {
			k.Status = state.StatusReviewed
			// Reviewed is where the task stops, and `status` measures elapsed
			// to whenever that was. Leaving the time its agent exited would
			// hide however long the review itself took, which for a loop that
			// runs until approval is the part worth seeing.
			k.FinishedAt = &now
			// A retry that succeeds is not still carrying the failure it
			// followed: leaving the text of an earlier review_failed behind
			// would have `status` report an approved branch with a reason it
			// did not work.
			k.Error = ""
		})
	}

	// No approval, but the round itself ran. If it followed an automatic
	// attempt that could not finish, that attempt's reason describes something
	// this run has since superseded, and a status row explaining a failure that
	// has been retried is worse than one explaining nothing. The status stays:
	// the branch still has no approval and was never published, which is what
	// review_failed says and what `agent-orc pr` is for.
	return r.scope.update(id, func(k *state.Task) {
		if k.Status == state.StatusReviewFailed {
			k.Error = ""
			fmt.Fprintf(r.out, "%s  the earlier automatic review failure no longer applies; "+
				"publish with 'agent-orc pr %s' when you are happy with the branch\n", id, id)
		}
	})
}

// Rounds runs review rounds until the reviewer approves, and reports whether
// it did.
//
// There is no round cap: a change reviewed but not approved is not a reviewed
// change, and stopping at a count would publish it anyway. The same pair bounds
// this as the test gate, the budget and a worker that stops acting on comments.
//
// It is the loop without the guards Review applies first, because the caller
// that skips them is the supervisor: it runs the moment the agent exits, when
// every check a human invocation makes would say the task is still mid-flight.
func (r *Reviewer) Rounds(record *state.Task) (bool, error) {
	for {
		// Only the automatic pass asks. Scoping the writes is not enough here:
		// a round checks out whatever branch the id names now and pays a
		// reviewer and a worker to work on it. A human reviewing a stopped
		// task is asking for something reasonable, so this does not apply.
		if err := r.mayContinue(record.ID); err != nil {
			return false, err
		}
		before := headSHA(record.Worktree)
		approved, err := r.round(record)
		if err != nil {
			return false, err
		}
		if approved {
			return true, nil
		}
		// The worker was handed comments and committed nothing, so the branch
		// the reviewer would read next is the branch it just read, and it
		// would raise the same comments again. Stopping is the only thing left
		// that is not a repeat.
		if after := headSHA(record.Worktree); after == before {
			fmt.Fprintf(r.out, "%s  the worker committed nothing in response; leaving the rest to a human\n",
				record.ID)
			return false, nil
		}
	}
}

// round runs one review, and hands any comments back to the worker.
func (r *Reviewer) round(record *state.Task) (bool, error) {
	round := record.ReviewRound + 1
	fmt.Fprintf(r.out, "%s  review round %d\n", record.ID, round)

	output, err := r.runReviewer(*record)
	if err != nil {
		return false, err
	}
	// What the reviewer *said*, not what its CLI printed around it: a CLI that
	// answers in JSON buries the verdict in a field of a very large object,
	// and parsing the envelope as prose finds neither an approval nor a list.
	a, err := adapter.For(record.ReviewerCLI())
	if err != nil {
		return false, err
	}

	verdict, err := review.ParseVerdict(a.ParseResult(output))
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
	return r.scope.update(record.ID, func(k *state.Task) { k.ReviewRound = round })
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
		// Raw, because the worker operating rules Render would otherwise append
		// tell the agent to commit its work. Handing those to a reviewer that
		// has just been told not to edit anything is a direct contradiction,
		// and the reviewer sits in a checkout of the branch under review.
		Raw:   true,
		Model: record.Review.Model,
		CLI:   cli,
		// The reviewer draws on the same per-task budget as the worker, so
		// there is one number to watch, not two.
		Budget: record.Budget,
	}
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
	// Before anything else: argv[0] below would panic on an empty command, and
	// there is no point opening a log for a run that cannot start.
	if len(argv) == 0 {
		return "", fmt.Errorf("task %q: the adapter produced an empty command", id)
	}

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

	// In its own process group with its pid on the record, so an automatic
	// review, which runs with no agent process left to stop, can still be.
	warn := func(format string, args ...any) { fmt.Fprintf(r.out, format+"\n", args...) }
	if err := (tracker{id: id, update: r.scope.update, warn: warn}).run(cmd); err != nil {
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
