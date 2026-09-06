package orc

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// noopTracker returns a tracker that records nothing, for testing the run
// mechanics on their own.
func noopTracker(timeout time.Duration) tracker {
	return tracker{
		id:      "T",
		update:  func(string, func(*state.Task)) error { return nil },
		timeout: timeout,
	}
}

// TestTrackerDoesNotSignalAReapedChild covers the window between a child
// exiting and its timer being stopped. Stopping a timer does not wait for a
// callback that has already begun, and a callback that ran on would signal a
// pid the kernel is free to have handed to something else: a stray SIGKILL to
// an unrelated process group, from a command that finished normally.
//
// The timeout is set to land around the moment the child exits, so the two
// race on purpose, and repeated so the interleaving varies.
func TestTrackerDoesNotSignalAReapedChild(t *testing.T) {
	for i := 0; i < 60; i++ {
		cmd := exec.Command("sh", "-c", "exit 3")
		err := noopTracker(time.Millisecond).run(cmd)
		if errors.Is(err, ErrTimedOut) {
			// Legitimate: the timer really did win. What must never happen is
			// the opposite, and neither may the race be reported as both.
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("run() = %v, want the command's own exit status 3", err)
		}
	}
}

// TestTrackerReportsATimeout is the other side: a child that outlasts its
// timeout is killed and reported as killed, not as a command that failed.
func TestTrackerReportsATimeout(t *testing.T) {
	start := time.Now()
	err := noopTracker(200 * time.Millisecond).run(exec.Command("sleep", "30"))
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("run() = %v, want %v", err, ErrTimedOut)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("run took %s; the child outlived its timeout", took)
	}
}

// TestTrackerLeavesAnUntimedChildAlone checks that a zero timeout installs no
// timer at all, which is what the review and resume paths rely on.
func TestTrackerLeavesAnUntimedChildAlone(t *testing.T) {
	if err := noopTracker(0).run(exec.Command("sh", "-c", "sleep 0.3; exit 0")); err != nil {
		t.Errorf("run() = %v, want the command to finish on its own", err)
	}
}
