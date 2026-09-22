package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// The part of a log the dashboard reads and keeps. A run can leave hundreds of
// megabytes behind, and the log is re-read every second, so it reads a bounded
// chunk from the end rather than the file, however large the file grows.
const (
	logTail      = 500     // lines kept in the viewport
	logTailBytes = 1 << 20 // read from the end of the file to find them
)

// loadTasks reads every task record.
//
// Deliberately not the status command's reconciliation pass: that writes, under
// the task's lock, whenever it finds a record whose process group is gone. This
// runs on a timer, and a dashboard must not take a write lock once a second.
func loadTasks(store *state.Store) ([]state.Task, error) {
	tasks, err := store.List()
	if err != nil {
		return nil, err
	}
	// Newest first: the task someone just dispatched is the one they are
	// watching, and List sorts oldest first for a table that scrolls off.
	for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
	return tasks, nil
}

// agentLog renders the end of a task's own output the way `agent-orc logs`
// renders it, so the two never disagree about what an agent said.
func agentLog(t state.Task) string {
	data, cut, err := readTail(t.LogPath, logTailBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing yet; the agent writes here once it starts."
	case err != nil:
		return "could not read the agent log: " + err.Error()
	}
	var buf bytes.Buffer
	if err := orc.FormatLog(&buf, bytes.NewReader(data), t.CLI); err != nil {
		return "could not render the agent log: " + err.Error()
	}
	return window(buf.String(), cut, t.LogPath)
}

// supervisorLog is what agent-orc did around the agent: the gates, the retries
// and the publish chain.
func supervisorLog(layout paths.Layout, id string) string {
	path := layout.SupervisorLogFile(id)
	data, cut, err := readTail(path, logTailBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing yet; the supervisor writes here once it starts."
	case err != nil:
		return "could not read the supervisor log: " + err.Error()
	}
	return window(string(data), cut, path)
}

// readTail reads at most limit bytes from the end of path, and reports whether
// that left anything out.
//
// A chunk taken from the middle of a file almost always starts part way through
// a line, and half of a JSON result renders as garbage, so the partial line is
// dropped. A chunk with no newline in it at all is one line longer than the
// limit, and none of it is kept.
func readTail(path string, limit int64) (data []byte, cut bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() <= limit {
		data, err = io.ReadAll(f)
		return data, false, err
	}
	if _, err := f.Seek(info.Size()-limit, io.SeekStart); err != nil {
		return nil, false, err
	}
	// Limited as well as sought: the file can grow between the stat and the
	// read, and a busy log would otherwise be read to its new end.
	data, err = io.ReadAll(io.LimitReader(f, limit))
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	} else {
		data = nil
	}
	return data, true, err
}

// window keeps the last logTail lines, and says so at the top when the log had
// more. Without the note, the top of the pane would look like the start of the
// run, which is where g takes you.
func window(text string, cut bool, path string) string {
	text, dropped := tailLines(text, logTail)
	if !cut && !dropped {
		return text
	}
	note := fmt.Sprintf("… earlier output not shown here; the full log is %s", path)
	if text == "" {
		return note
	}
	return note + "\n" + text
}

// tailLines keeps the last n lines of s, and reports whether it dropped any.
func tailLines(s string, n int) (string, bool) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "", false
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s, false
	}
	return strings.Join(lines[len(lines)-n:], "\n"), true
}
