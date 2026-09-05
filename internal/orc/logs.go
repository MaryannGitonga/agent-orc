package orc

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Logs writes a task's agent log to out. When follow is set it keeps writing
// as the agent produces more, and stops when the task finishes. It is a tail
// that ends on its own rather than one the user has to interrupt.
//
// The log is summarized unless raw is set, and only for a CLI that reports
// through a JSON envelope: that CLI writes one very long line, and printing it
// verbatim is what raw is for. A CLI that answers in prose is copied straight
// through, because its bytes are the record of the run, and reinterpreting a
// JSON object the agent happened to print would rewrite the agent's own output.
func (r *Reporter) Logs(id string, follow, raw bool) error {
	record, err := r.store.Load(id)
	if err != nil {
		return err
	}

	f, err := os.Open(record.LogPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("task %q has no log yet at %s", id, record.LogPath)
	}
	if err != nil {
		return fmt.Errorf("opening %s: %w", record.LogPath, err)
	}
	defer f.Close()

	if raw || !summarizes(record.CLI) {
		return r.copyLog(f, r.out, id, record.LogPath, follow)
	}
	formatted := &logFormatter{out: r.out}
	err = r.copyLog(f, formatted, id, record.LogPath, follow)
	// The copy stops at whatever the file holds, which need not be a whole
	// line, so the remainder is written before returning either error.
	if ferr := formatted.Flush(); err == nil {
		err = ferr
	}
	return err
}

// summarizes reports whether a CLI's log is worth reshaping. An unknown CLI is
// left alone: passing bytes through is always safe, and guessing at a shape is
// not.
func summarizes(cli task.CLI) bool {
	a, err := adapter.For(cli)
	return err == nil && a.WritesJSONResult()
}

// copyLog drains f into out, and keeps draining while the task runs if follow
// is set.
func (r *Reporter) copyLog(f *os.File, out io.Writer, id, logPath string, follow bool) error {
	if _, err := io.Copy(out, f); err != nil {
		return fmt.Errorf("reading %s: %w", logPath, err)
	}
	if !follow {
		return nil
	}

	for {
		if n, err := io.Copy(out, f); err != nil {
			return fmt.Errorf("reading %s: %w", logPath, err)
		} else if n > 0 {
			continue
		}
		current, err := r.store.Load(id)
		if err != nil || !current.Status.Active() {
			// One last read, so nothing written between the final check and
			// the process exiting is lost.
			if _, cerr := io.Copy(out, f); cerr != nil {
				return fmt.Errorf("reading %s: %w", logPath, cerr)
			}
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
