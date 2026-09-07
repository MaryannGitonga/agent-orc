package orc

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// TestSupervisorWritesOnlyToItsOwnRun covers the window a reusable task id
// opens. `stop` signals the agent and returns without waiting for it to die,
// and a forced cleanup does not look at the process at all, so a supervisor can
// still be in its final write when the id is dispatched again. An unguarded
// write would stamp the finished run's status, exit code and a zero pid onto a
// task that is running, leaving it recorded as done and impossible to stop.
func TestSupervisorWritesOnlyToItsOwnRun(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)

	// The task the supervisor was launched for, and then the one that took its
	// id over, which is what the store now holds.
	firstRun := time.Now().UTC().Add(-time.Hour)
	current := state.Task{
		Task:      task.Task{ID: "PROJ-1", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		PID:       4242,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}

	stale := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, scope: scopeTo(store, firstRun)}
	if err := stale.scope.update("PROJ-1", func(k *state.Task) {
		k.Status = state.StatusDone
		k.PID = 0
	}); err != nil {
		t.Fatalf("update() = %v", err)
	}

	got, err := store.Load("PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusRunning || got.PID != 4242 {
		t.Errorf("a stale supervisor overwrote the live task: status=%q pid=%d", got.Status, got.PID)
	}

	// The supervisor that does own the run still writes.
	own := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, scope: scopeTo(store, current.StartedAt)}
	if err := own.scope.update("PROJ-1", func(k *state.Task) { k.Status = state.StatusDone }); err != nil {
		t.Fatalf("update() = %v", err)
	}
	if got, _ := store.Load("PROJ-1"); got.Status != state.StatusDone {
		t.Errorf("status = %q, want the owning supervisor's write to land", got.Status)
	}
}

// TestPublisherWritesOnlyToItsOwnRun is the same guard on the publish chain,
// which runs after the agent exits and so sits in the same window. A publisher
// with no run scoped, which is what `agent-orc pr` builds, writes either way.
func TestPublisherWritesOnlyToItsOwnRun(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	current := state.Task{
		Task:      task.Task{ID: "PROJ-2", CLI: task.CLIClaude},
		Status:    state.StatusRunning,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}

	stale := &Publisher{store: store, out: io.Discard, scope: scopeTo(store, current.StartedAt.Add(-time.Hour))}
	if err := stale.scope.update("PROJ-2", func(k *state.Task) { k.PRURL = "https://example.invalid/1" }); err != nil {
		t.Fatalf("update() = %v", err)
	}
	if got, _ := store.Load("PROJ-2"); got.PRURL != "" {
		t.Errorf("pr_url = %q, want a stale publisher's write dropped", got.PRURL)
	}

	unscoped := &Publisher{store: store, out: io.Discard, scope: runScope{store: store}}
	if err := unscoped.scope.update("PROJ-2", func(k *state.Task) { k.PRURL = "https://example.invalid/2" }); err != nil {
		t.Fatalf("update() = %v", err)
	}
	if got, _ := store.Load("PROJ-2"); got.PRURL == "" {
		t.Error("an unscoped publisher's write was dropped; 'agent-orc pr' depends on it landing")
	}
}

// TestSupervisorDoesNotPublishALaterRun covers the one step that acts on the id
// rather than on the record the supervisor already holds. Scoping the
// publisher's writes is not enough: Publish loads the record for itself, so a
// stale supervisor would sanitize, force-push and open a pull request against
// whatever branch the id names now, and only then have its state writes
// dropped. The damage would already be on the remote.
func TestSupervisorDoesNotPublishALaterRun(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	current := state.Task{
		Task:      task.Task{ID: "PROJ-3", CLI: task.CLIClaude, Branch: "agent-orc/proj-3", AutoPR: true},
		Status:    state.StatusRunning,
		PID:       999,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}

	// A supervisor from an earlier task that had this id. Its worktree and repo
	// are nonsense on purpose: reaching git at all would be the bug.
	//
	// The state has to be read out of the log rather than out of the record.
	// Every write the chain would make is dropped by the generation guard
	// anyway, so the record looks the same either way; what distinguishes the
	// two is whether the chain was attempted at all.
	var log bytes.Buffer
	stale := &Supervisor{
		layout: paths.New(dir),
		store:  store,
		out:    &log,
		scope:  scopeTo(store, current.StartedAt.Add(-time.Hour)),
	}
	stale.publish("PROJ-3", state.Task{
		Task:     task.Task{ID: "PROJ-3", Repo: filepath.Join(dir, "no-such-repo")},
		Worktree: filepath.Join(dir, "no-such-worktree"),
	})

	if got := log.String(); !strings.Contains(got, "belongs to a later run") {
		t.Errorf("log = %q, want the publish declined before it started", got)
	} else if strings.Contains(got, "publish failed") || strings.Contains(got, "is done") {
		t.Errorf("log = %q, want no sign the chain was attempted", got)
	}

	got, err := store.Load("PROJ-3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.StatusRunning || got.PID != 999 {
		t.Errorf("a stale supervisor published over the live task: status=%q pid=%d", got.Status, got.PID)
	}
}

// TestPublishRefusesARecordFromAnotherRun covers the gap between the caller's
// ownership check and the load inside Publish. The supervisor asks first, but
// the id can be dispatched again in between, and everything the chain does
// works from the record loaded here: checking the earlier read would leave a
// branch sanitized, force-pushed and opened as a pull request before any state
// write was dropped.
func TestPublishRefusesARecordFromAnotherRun(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	current := state.Task{
		Task: task.Task{
			ID: "PROJ-4", CLI: task.CLIClaude, AutoPR: true,
			Branch: "agent-orc/proj-4",
			// A repository that does not exist, so any git the chain reached
			// would fail with something else entirely.
			Repo: filepath.Join(dir, "no-such-repo"),
		},
		Status:    state.StatusRunning,
		Worktree:  filepath.Join(dir, "no-such-worktree"),
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}

	p := &Publisher{store: store, out: io.Discard, scope: scopeTo(store, current.StartedAt.Add(-time.Hour))}
	err := p.Publish("PROJ-4")
	if !errors.Is(err, ErrRunReplaced) {
		t.Fatalf("Publish() = %v, want %v before any git ran", err, ErrRunReplaced)
	}

	// And a publisher scoped to this run, or to none at all, gets past the
	// check and fails on the repository instead, which is how we know the
	// guard is the reason for the refusal above and not the missing repo.
	p = &Publisher{store: store, out: io.Discard, scope: scopeTo(store, current.StartedAt)}
	if err := p.Publish("PROJ-4"); errors.Is(err, ErrRunReplaced) {
		t.Errorf("Publish() = %v, want the owning run to get past the check", err)
	}
}

// TestMarkKeepsFinishedAtHonest covers what `status` reports as elapsed. The
// phases after the agent exits run until the tests pass or the reviewer
// approves, so stamping a finish time when the agent stopped would freeze the
// elapsed column there and hide every minute of them.
func TestMarkKeepsFinishedAtHonest(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	finished := time.Now().UTC().Add(-time.Hour)
	rec := state.Task{
		Task:       task.Task{ID: "PROJ-5", CLI: task.CLIClaude},
		Status:     state.StatusRunning,
		StartedAt:  time.Now().UTC().Add(-2 * time.Hour),
		FinishedAt: &finished,
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, scope: scopeTo(store, rec.StartedAt)}

	// A phase that is still working has not finished, whatever was stamped
	// when its agent exited.
	for _, active := range []state.Status{state.StatusVerifying, state.StatusReviewing, state.StatusPublishing} {
		s.mark("PROJ-5", active, "")
		got, err := store.Load("PROJ-5")
		if err != nil {
			t.Fatal(err)
		}
		if got.FinishedAt != nil {
			t.Errorf("%s left finished_at at %v, so elapsed stops counting mid-flight", active, got.FinishedAt)
		}
	}

	// And a terminal status finishes it, now rather than an hour ago.
	before := time.Now().UTC().Add(-time.Second)
	s.mark("PROJ-5", state.StatusDone, "")
	got, err := store.Load("PROJ-5")
	if err != nil {
		t.Fatal(err)
	}
	if got.FinishedAt == nil {
		t.Fatal("done left finished_at unset, so elapsed would keep growing forever")
	}
	if got.FinishedAt.Before(before) {
		t.Errorf("finished_at = %v, want when the task finished rather than when its agent did", got.FinishedAt)
	}
}

// TestSuccessClearsAnEarlierFailure covers what a retry leaves behind. A task
// that failed and was then put right is not still failing, and a status row
// carrying the old reason alongside the new outcome is worse than one carrying
// no reason at all.
func TestSuccessClearsAnEarlierFailure(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	rec := state.Task{
		Task:      task.Task{ID: "PROJ-6", CLI: task.CLIClaude},
		Status:    state.StatusPublishFailed,
		Error:     "the remote rejected the push",
		StartedAt: time.Now().UTC(),
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, scope: scopeTo(store, rec.StartedAt)}

	s.mark("PROJ-6", state.StatusDone, "")
	got, err := store.Load("PROJ-6")
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want a done task to carry none", got.Error)
	}

	// And a status that does have something to say still says it.
	s.mark("PROJ-6", state.StatusFailed, "the tests did not pass")
	if got, _ := store.Load("PROJ-6"); got.Error != "the tests did not pass" {
		t.Errorf("error = %q, want the new reason recorded", got.Error)
	}
}

// TestMarkDoesNotUndoAStop covers the window a stop can land in. The phases
// after the agent clear the pid between commands, and stop is allowed to record
// itself there, so the next phase's own mark would otherwise write over it and
// the loops would carry on as though nothing had been asked.
func TestMarkDoesNotUndoAStop(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	stoppedAt := time.Now().UTC()
	rec := state.Task{
		Task:       task.Task{ID: "PROJ-7", CLI: task.CLIClaude},
		Status:     state.StatusStopped,
		Error:      "stopped by agent-orc stop",
		StartedAt:  time.Now().UTC().Add(-time.Hour),
		FinishedAt: &stoppedAt,
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, scope: scopeTo(store, rec.StartedAt)}

	// Everything the supervisor would go on to record after the gates.
	for _, next := range []state.Status{
		state.StatusVerifying, state.StatusReviewing, state.StatusPublishing,
		state.StatusDone, state.StatusFailed, state.StatusReviewFailed,
	} {
		s.mark("PROJ-7", next, "")
		got, err := store.Load("PROJ-7")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != state.StatusStopped {
			t.Fatalf("mark(%s) overwrote the stop; status = %q", next, got.Status)
		}
		if got.Error != "stopped by agent-orc stop" {
			t.Errorf("mark(%s) changed the reason to %q", next, got.Error)
		}
	}
}

// TestReviewRoundsStopForALaterRun covers the loop that runs longest. Scoping
// the writes is not enough here: a round checks out whatever branch the id
// names now and pays a reviewer and a worker to work on it, so a supervisor
// whose task has been cleaned up and replaced has to stop before it starts one,
// not merely have its bookkeeping dropped afterwards.
func TestReviewRoundsStopForALaterRun(t *testing.T) {
	dir := t.TempDir()
	layout := paths.New(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.State)
	current := state.Task{
		Task:      task.Task{ID: "PROJ-8", CLI: task.CLIClaude, Branch: "agent-orc/proj-8"},
		Status:    state.StatusRunning,
		StartedAt: time.Now().UTC(),
		// Nonsense on purpose: reaching git at all would be the bug.
		Worktree: filepath.Join(dir, "no-such-worktree"),
	}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}

	r := NewReviewer(layout, io.Discard)
	r.scope = scopeTo(store, current.StartedAt.Add(-time.Hour))
	held := current
	approved, err := r.Rounds(&held)
	if approved {
		t.Fatal("a stale reviewer approved a branch it does not own")
	}
	if err == nil || !strings.Contains(err.Error(), "belongs to a later run") {
		t.Fatalf("Rounds() = %v, want it to decline before starting a round", err)
	}

	// A stop is the other reason to stop, from the same read.
	if err := store.Update("PROJ-8", func(k *state.Task) { k.Status = state.StatusStopped }); err != nil {
		t.Fatal(err)
	}
	r = NewReviewer(layout, io.Discard)
	r.scope = scopeTo(state.NewStore(layout.State), current.StartedAt)
	held = current
	if _, err := r.Rounds(&held); err == nil || !strings.Contains(err.Error(), "was stopped") {
		t.Errorf("Rounds() = %v, want it to decline a stopped task", err)
	}

	// And a reviewer nobody scoped, which is what `agent-orc review` builds,
	// is not held back by either: it gets as far as needing a real worktree.
	r = NewReviewer(layout, io.Discard)
	held = current
	if _, err := r.Rounds(&held); err == nil ||
		strings.Contains(err.Error(), "belongs to a later run") ||
		strings.Contains(err.Error(), "was stopped") {
		t.Errorf("Rounds() = %v, want an unscoped reviewer to proceed to the work", err)
	}
}
