package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// logTail is how much of a log is kept in the viewport. A task that ran for an
// hour can leave megabytes behind, and the end is the part being watched.
const logTail = 500

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

// agentLog renders a task's own output the way `agent-orc logs` does, so the
// two never disagree about what an agent said. A CLI that answers in a JSON
// envelope gets its answer unwrapped and a short footer rather than a screenful
// of token accounting.
func agentLog(layout paths.Layout, id string) string {
	var buf bytes.Buffer
	if err := orc.NewReporter(layout.State, &buf).Logs(id, false, false); err != nil {
		return "could not read the agent log: " + err.Error()
	}
	return tailLines(buf.String(), logTail)
}

// supervisorLog is what agent-orc did around the agent: the gates, the retries
// and the publish chain.
func supervisorLog(layout paths.Layout, id string) string {
	data, err := os.ReadFile(layout.SupervisorLogFile(id))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing yet; the supervisor writes here once it starts."
	case err != nil:
		return "could not read the supervisor log: " + err.Error()
	}
	return tailLines(string(data), logTail)
}

// tailLines keeps the last n lines of s.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
