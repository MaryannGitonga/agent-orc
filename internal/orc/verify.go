package orc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// ErrTestsFailed means the task's own test command never passed.
var ErrTestsFailed = errors.New("the task's test command did not pass")

// testOutputLines is how much of a failing run is handed back to the agent.
// Test runners put the summary last, so the tail is the part worth sending.
const testOutputLines = 60

// verify runs a task's test command in its worktree, handing failures back to
// the agent to fix, until the suite passes. It is the difference between an
// agent that says the tests pass and a branch where they do.
//
// There is no attempt cap: getting the suite green is part of the work. Three
// real conditions end the loop instead. The tests pass; the agent's CLI ends
// the session, which is how a budget is enforced; or the agent commits nothing
// in a round, a fixed point since the next run would be identical.
//
// A CLI that cannot resume a session gets one run and no loop, there being no
// session to hand the failure back to. It is still checked: blocking a red
// branch is worth more than the iteration.
func (s *Supervisor) verify(record state.Task) error {
	command := strings.TrimSpace(record.TestCommand)
	if command == "" {
		return nil
	}
	a, err := adapter.For(record.CLI)
	if err != nil {
		return err
	}
	resumable := true
	if _, err := a.ResumeCommand(record.SessionID, "probe", ""); err != nil {
		s.logf("%s cannot be resumed, so a failing suite cannot be handed back; checking once", record.CLI)
		resumable = false
	}

	for attempt := 1; ; attempt++ {
		// Asked before each run, not only after one fails: a stop recorded in
		// the gap between attempts has nothing to kill, and the only way it
		// takes effect is the loop declining to start the next command. The
		// same read covers the id having been dispatched again, since this
		// loop runs until the suite passes and that is long enough for a task
		// to be cleaned up and replaced underneath it.
		if s.scope.stopped(record.ID) {
			return fmt.Errorf("task %q was stopped before its tests could be run again", record.ID)
		}
		if !s.scope.ownsID(record.ID) {
			return fmt.Errorf("task %q now belongs to a later run; its tests are not ours to run", record.ID)
		}
		s.logf("running the test command (attempt %d): %s", attempt, command)
		output, runErr := s.runTestCommand(record.ID, record.Worktree, command, record.TestRunTimeout())
		if runErr == nil {
			s.logf("the test command passed")
			s.recordTests(record.ID, attempt, true)
			return nil
		}
		if errors.Is(runErr, ErrTimedOut) {
			// Worth saying plainly: a suite that hangs looks nothing like one
			// that fails, and the output below will be whatever it managed to
			// print before it stopped making progress.
			s.logf("the test command was killed after %s without finishing", record.TestRunTimeout())
			output += fmt.Sprintf("\n[agent-orc killed this command after %s; it did not finish]\n",
				record.TestRunTimeout())
		} else {
			s.logf("the test command failed: %v", runErr)
		}
		s.recordTests(record.ID, attempt, false)

		// The failure may be the stop itself: `agent-orc stop` kills whatever
		// child is running, and a non-zero exit from a killed test command is
		// not a reason to hand it back to the agent and try again.
		if s.scope.stopped(record.ID) {
			return fmt.Errorf("task %q was stopped while its tests were running", record.ID)
		}
		if !resumable {
			return fmt.Errorf("%w: %s", ErrTestsFailed, strings.TrimSpace(lastLines(output, 5)))
		}
		before := headSHA(record.Worktree)
		if err := s.handBackFailure(record, command, output); err != nil {
			return err
		}
		if s.scope.stopped(record.ID) {
			return fmt.Errorf("task %q was stopped while the agent was fixing its tests", record.ID)
		}
		// A round that committed nothing has not changed the code under test,
		// so running it again would fail in exactly the same way. Stopping
		// here is not a quota; it is the point at which repeating stops being
		// an attempt at anything.
		if after := headSHA(record.Worktree); after == before {
			return fmt.Errorf("%w after %d attempt(s): the agent committed nothing in its last round, "+
				"so the suite would keep failing the same way: %s",
				ErrTestsFailed, attempt, strings.TrimSpace(lastLines(output, 5)))
		}
	}
}

// headSHA returns the worktree's current commit, or "" if it cannot be read.
//
// Both loops that hand work back to an agent use it the same way, to tell a
// round that changed something from a round that did not. An unreadable HEAD
// reads as no progress, so the loop stops rather than running on with nothing
// to compare against.
func headSHA(worktree string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handBackFailure resumes the agent's own session with the failing output, so
// it fixes the code it wrote rather than starting from a blank session.
func (s *Supervisor) handBackFailure(record state.Task, command, output string) error {
	a, err := adapter.For(record.CLI)
	if err != nil {
		return err
	}
	argv, err := a.ResumeCommand(record.SessionID, testFailurePrompt(command, output), record.Model)
	if err != nil {
		return fmt.Errorf("task %q: %w", record.ID, err)
	}
	s.logf("handing the failure back to the agent")

	logFile, err := os.OpenFile(record.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", record.LogPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv comes from an adapter
	cmd.Dir = record.Worktree
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if err := s.tracker(record.ID, 0).run(cmd); err != nil {
		return fmt.Errorf("task %q: %s exited with an error while fixing the tests: %w",
			record.ID, argv[0], err)
	}
	return nil
}

// testFailurePrompt is what the agent is resumed with after a failing run.
func testFailurePrompt(command, output string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The test command for this project failed on your branch:\n\n    %s\n\n", command)
	b.WriteString("Its output ended with:\n\n---\n")
	b.WriteString(strings.TrimSpace(lastLines(output, testOutputLines)))
	b.WriteString("\n---\n\n")
	b.WriteString("Fix this in the worktree you are in, on the branch already checked out,\n")
	b.WriteString("and commit the fix locally with a one-line conventional commit subject.\n")
	b.WriteString("Change the code that is wrong, not the test, unless the test is what is\n")
	b.WriteString("wrong. Do not push, do not open a pull request, and do not add any\n")
	b.WriteString("Co-authored-by trailer.\n")
	return b.String()
}

// runTestCommand runs the command through a shell in dir. A shell because a
// test command is written the way it is typed, pipes and all, and quoting it
// into an argv here would only be a worse shell.
func (s *Supervisor) runTestCommand(id, dir, command string, timeout time.Duration) (string, error) {
	cmd := exec.Command("sh", "-c", command) // #nosec G204 -- the command is the user's own config
	cmd.Dir = dir
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := s.tracker(id, timeout).run(cmd)
	return buf.String(), err
}

// tracker runs a child on this task's behalf, in its own process group and
// with its pid on the record while it runs.
func (s *Supervisor) tracker(id string, timeout time.Duration) tracker {
	return tracker{id: id, update: s.scope.update, timeout: timeout, warn: s.logf}
}

// recordTests notes how the verification went, so 'agent-orc status' can say
// whether a branch was checked and what came of it.
func (s *Supervisor) recordTests(id string, attempts int, passed bool) {
	if err := s.scope.update(id, func(k *state.Task) {
		k.TestRuns = attempts
		k.TestsPassed = &passed
	}); err != nil {
		s.logf("warning: could not record the test outcome: %v", err)
	}
}
