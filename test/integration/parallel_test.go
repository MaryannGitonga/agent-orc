//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oneSlot limits home to one running task, and returns a stub directory whose
// claude runs until release is created, and whose codex returns at once.
func oneSlot(t *testing.T, home string) (stubs, release, codexReceipt string) {
	t.Helper()
	write(t, filepath.Join(home, "defaults.yaml"), "max_parallel: 1\n")
	stubs = t.TempDir()
	release = filepath.Join(t.TempDir(), "release")
	codexReceipt = filepath.Join(t.TempDir(), "codex")
	stubInto(t, stubs, "claude", filepath.Join(t.TempDir(), "claude"),
		"while [ ! -f '"+release+"' ]; do sleep 0.1; done")
	stubInto(t, stubs, "codex", codexReceipt, "true")
	return stubs, release, codexReceipt
}

func dispatch(t *testing.T, home, stubs, repo, id, cli string) {
	t.Helper()
	if out, err := orcRun(t, home, stubs, "run",
		"--id", id, "--repo", repo, "--cli", cli, "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run %s = %v\n%s", id, err, out)
	}
}

// TestASecondTaskWaitsForTheOnlySlot covers max_parallel: the second task stays
// pending while the first holds the only slot, then runs once it is free.
func TestASecondTaskWaitsForTheOnlySlot(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stubs, release, _ := oneSlot(t, home)
	t.Cleanup(func() { write(t, release, "") })

	dispatch(t, home, stubs, repo, "SLOT-1", "claude")
	waitForStatus(t, home, "SLOT-1", "running")
	dispatch(t, home, stubs, repo, "SLOT-2", "codex")

	time.Sleep(time.Second)
	if got := loadRecord(t, home, "SLOT-2"); got.Status != "pending" {
		t.Fatalf("SLOT-2 is %q while SLOT-1 holds the only slot, want pending", got.Status)
	}
	if log := readFile(t, filepath.Join(home, "logs", "SLOT-2.supervisor.log")); !strings.Contains(log, "waiting for a slot") {
		t.Errorf("supervisor log = %q, want it to say it is waiting", log)
	}

	write(t, release, "")
	waitForStatus(t, home, "SLOT-1", "done", "failed")
	if got := waitForStatus(t, home, "SLOT-2", "done", "failed"); got.Status != "done" {
		t.Errorf("SLOT-2 = %q after the slot freed, want done", got.Status)
	}
}

// TestStopEndsATaskWaitingForASlot covers stop on a queued task: it is recorded
// as stopped and its agent never starts.
func TestStopEndsATaskWaitingForASlot(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stubs, release, codexReceipt := oneSlot(t, home)
	t.Cleanup(func() { write(t, release, "") })

	dispatch(t, home, stubs, repo, "HOLD-1", "claude")
	waitForStatus(t, home, "HOLD-1", "running")
	dispatch(t, home, stubs, repo, "QUEUED-1", "codex")
	time.Sleep(500 * time.Millisecond)

	out, err := orcRun(t, home, stubs, "stop", "QUEUED-1")
	if err != nil || !strings.Contains(out, "waiting for a slot") {
		t.Fatalf("agent-orc stop = %v, want it to stop the queued task\n%s", err, out)
	}
	write(t, release, "")
	waitForStatus(t, home, "HOLD-1", "done", "failed")
	time.Sleep(time.Second)
	if got := loadRecord(t, home, "QUEUED-1"); got.Status != "stopped" {
		t.Errorf("QUEUED-1 = %q, want stopped", got.Status)
	}
	if _, err := os.Stat(codexReceipt); !os.IsNotExist(err) {
		t.Error("the stopped task's agent ran anyway")
	}
}
