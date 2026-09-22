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
	// How far back the start of a single very long line is worth looking for.
	// A CLI that answers through a JSON envelope writes its whole answer as one
	// line, so refusing to read past the limit would blank the agent pane for
	// exactly the runs worth reading. The formatter holds a line this long too.
	logMaxLine = 8 << 20
)

// tailKind says how much of a log a tail covers.
type tailKind int

const (
	tailWhole   tailKind = iota // all of it
	tailCut                     // earlier lines left out
	tailPartial                 // one line longer than logMaxLine, shown from part way
)

// loadTasks reads every task record.
//
// Deliberately not the status command's reconciliation pass: that writes, under
// the task's lock, whenever it finds a record whose process group is gone. This
// runs on a timer, and a dashboard must not take a write lock once a second.
func loadTasks(store *state.Store) ([]state.Task, map[string]bool, error) {
	tasks, err := store.List()
	if err != nil {
		return nil, nil, err
	}
	// Which records claim a process that is no longer there. `agent-orc status`
	// corrects those by writing; this only notes them, so a task whose
	// supervisor was killed is not drawn as though it were still working.
	gone := map[string]bool{}
	for _, t := range tasks {
		if !orc.TaskAlive(t) {
			gone[t.ID] = true
		}
	}
	// Newest first: the task someone just dispatched is the one they are
	// watching, and List sorts oldest first for a table that scrolls off.
	for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
	return tasks, gone, nil
}

// agentLog renders the end of a task's own output the way `agent-orc logs`
// renders it, so the two never disagree about what an agent said.
func agentLog(t state.Task) string {
	data, kind, err := readTail(t.LogPath, logTailBytes, logMaxLine)
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
	return window(buf.String(), kind, t.LogPath)
}

// supervisorLog is what agent-orc did around the agent: the gates, the retries
// and the publish chain.
func supervisorLog(layout paths.Layout, id string) string {
	path := layout.SupervisorLogFile(id)
	data, kind, err := readTail(path, logTailBytes, logMaxLine)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing yet; the supervisor writes here once it starts."
	case err != nil:
		return "could not read the supervisor log: " + err.Error()
	}
	return window(string(data), kind, path)
}

// readTail reads the end of path: at most limit bytes, or as far back as the
// start of the final line when that line is longer than limit, up to maxLine.
//
// A chunk taken from the middle of a file starts part way through a line, and
// half a JSON result renders as garbage, so the partial line is dropped. The
// one line it will not drop is the last: for a CLI that answers in a single
// JSON object, that line is the answer.
func readTail(path string, limit, maxLine int64) ([]byte, tailKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, tailWhole, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, tailWhole, err
	}
	size := info.Size()
	if size <= limit {
		// Limited even here: the file can grow between the stat and the read,
		// and a log being written to would otherwise be read to its new end.
		data, err := readFrom(f, 0, limit)
		return data, tailWhole, err
	}

	data, err := readFrom(f, size-limit, limit)
	if err != nil {
		return nil, tailWhole, err
	}
	// Whole lines in the chunk, and something left once the partial first one
	// is dropped. A chunk that is only the end of one long line, with the
	// newline that terminates it, leaves nothing, and falls through.
	if i := bytes.IndexByte(data, '\n'); i >= 0 && len(bytes.TrimRight(data[i+1:], "\n")) > 0 {
		return data[i+1:], tailCut, nil
	}

	// The last line is longer than limit.
	from := int64(0)
	if size > maxLine {
		from = size - maxLine
	}
	ext, err := readFrom(f, from, maxLine)
	if err != nil {
		return nil, tailWhole, err
	}
	// Its own terminator is not a line break to search from.
	ext = bytes.TrimRight(ext, "\n")
	if i := bytes.LastIndexByte(ext, '\n'); i >= 0 {
		return ext[i+1:], tailCut, nil
	}
	if from == 0 {
		return ext, tailWhole, nil // the file is one line, and this is all of it
	}
	return ext, tailPartial, nil
}

// readFrom reads at most n bytes from off.
func readFrom(f *os.File, off, n int64) ([]byte, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, n))
}

// window keeps the last logTail lines, and says at the top when the log held
// more. Without the note, the top of the pane would look like the start of the
// run, which is where g takes you.
func window(text string, kind tailKind, path string) string {
	text, dropped := tailLines(text, logTail)
	var note string
	switch {
	case kind == tailPartial:
		note = fmt.Sprintf("… this log's last line is too long to show whole; its end follows. All of it is in %s", path)
	case kind == tailCut || dropped:
		note = fmt.Sprintf("… earlier output not shown here; the full log is %s", path)
	default:
		return text
	}
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
