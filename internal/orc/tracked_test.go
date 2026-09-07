package orc

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// TestTrackerWarnsWhenItCannotRecordThePID covers the two writes that are the
// only handle `agent-orc stop` has on a tracked child. Neither failure is worth
// killing the work over, but both leave the record wrong in a way somebody has
// to be able to see: one loses the child, the other leaves a number that the
// kernel will hand to a stranger.
func TestTrackerWarnsWhenItCannotRecordThePID(t *testing.T) {
	var warnings []string
	tr := tracker{
		id:     "T",
		update: func(string, func(*state.Task)) error { return errors.New("the state file is unwritable") },
		warn:   func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	}
	if err := tr.run(exec.Command("sh", "-c", "exit 0")); err != nil {
		t.Fatalf("run() = %v, want the command to have run regardless", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want one for recording the pid and one for clearing it", warnings)
	}
	if !strings.Contains(warnings[0], "will not reach it") {
		t.Errorf("first warning = %q, want it to say stop cannot reach the child", warnings[0])
	}
	if !strings.Contains(warnings[1], "still names it") {
		t.Errorf("second warning = %q, want it to say the record kept the pid", warnings[1])
	}
}

// TestTrackerWithoutAWarnerStillRuns keeps the field optional, so a caller that
// has nowhere to report is not a nil dereference.
func TestTrackerWithoutAWarnerStillRuns(t *testing.T) {
	tr := tracker{id: "T", update: func(string, func(*state.Task)) error { return errors.New("nope") }}
	if err := tr.run(exec.Command("sh", "-c", "exit 0")); err != nil {
		t.Errorf("run() = %v, want it to survive having no warner", err)
	}
}
