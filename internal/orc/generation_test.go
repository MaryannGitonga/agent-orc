package orc

import (
	"bytes"
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

	stale := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, startedAt: firstRun}
	if err := stale.update("PROJ-1", func(k *state.Task) {
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
	own := &Supervisor{layout: paths.New(dir), store: store, out: io.Discard, startedAt: current.StartedAt}
	if err := own.update("PROJ-1", func(k *state.Task) { k.Status = state.StatusDone }); err != nil {
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

	stale := &Publisher{store: store, out: io.Discard}
	stale.OwnRun(current.StartedAt.Add(-time.Hour))
	if err := stale.update("PROJ-2", func(k *state.Task) { k.PRURL = "https://example.invalid/1" }); err != nil {
		t.Fatalf("update() = %v", err)
	}
	if got, _ := store.Load("PROJ-2"); got.PRURL != "" {
		t.Errorf("pr_url = %q, want a stale publisher's write dropped", got.PRURL)
	}

	unscoped := &Publisher{store: store, out: io.Discard}
	if err := unscoped.update("PROJ-2", func(k *state.Task) { k.PRURL = "https://example.invalid/2" }); err != nil {
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
		layout:    paths.New(dir),
		store:     store,
		out:       &log,
		startedAt: current.StartedAt.Add(-time.Hour),
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
