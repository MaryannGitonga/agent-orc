// Package paths resolves where agent-orc keeps its state: one root
// (~/.agent-orc, or AGENT_ORC_HOME) so a run leaves nothing behind outside it.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvHome overrides the root directory when set.
const EnvHome = "AGENT_ORC_HOME"

// Layout holds the resolved directory layout for one agent-orc invocation.
type Layout struct {
	Root      string // ~/.agent-orc
	State     string // ~/.agent-orc/state
	Logs      string // ~/.agent-orc/logs
	Worktrees string // ~/.agent-orc/worktrees
}

// Resolve returns the layout without creating anything; see [Layout.Ensure].
func Resolve() (Layout, error) {
	root := os.Getenv(EnvHome)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Layout{}, fmt.Errorf("resolving home directory: %w", err)
		}
		root = filepath.Join(home, ".agent-orc")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolving %s=%q: %w", EnvHome, root, err)
	}
	return New(abs), nil
}

// New builds a layout rooted at root.
func New(root string) Layout {
	return Layout{
		Root:      root,
		State:     filepath.Join(root, "state"),
		Logs:      filepath.Join(root, "logs"),
		Worktrees: filepath.Join(root, "worktrees"),
	}
}

// Ensure creates every directory in the layout.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.State, l.Logs, l.Worktrees} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// StateFile returns the state file path for a task.
func (l Layout) StateFile(id string) string {
	return filepath.Join(l.State, id+".json")
}

// LogFile returns the agent's own log file for a task.
func (l Layout) LogFile(id string) string {
	return filepath.Join(l.Logs, id+".log")
}

// SupervisorLogFile returns the log file for the supervisor wrapping a task.
func (l Layout) SupervisorLogFile(id string) string {
	return filepath.Join(l.Logs, id+".supervisor.log")
}

// Worktree returns the worktree directory for a task.
func (l Layout) Worktree(id string) string {
	return filepath.Join(l.Worktrees, id)
}
