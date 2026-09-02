package orc

import (
	"fmt"
	"io"
	"os"
	"time"
)

// Logs writes a task's agent log to out. When follow is set it keeps writing
// as the agent produces more, and stops when the task finishes — a tail that
// ends on its own rather than one the user has to interrupt.
func (r *Reporter) Logs(id string, follow bool) error {
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

	if _, err := io.Copy(r.out, f); err != nil {
		return fmt.Errorf("reading %s: %w", record.LogPath, err)
	}
	if !follow {
		return nil
	}

	for {
		if n, err := io.Copy(r.out, f); err != nil {
			return fmt.Errorf("reading %s: %w", record.LogPath, err)
		} else if n > 0 {
			continue
		}
		current, err := r.store.Load(id)
		if err != nil || !current.Status.Active() {
			// One last read, so nothing written between the final check and
			// the process exiting is lost.
			if _, cerr := io.Copy(r.out, f); cerr != nil {
				return fmt.Errorf("reading %s: %w", record.LogPath, cerr)
			}
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
