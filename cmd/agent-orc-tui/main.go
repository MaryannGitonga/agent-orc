// Command agent-orc-tui is a terminal dashboard over the tasks agent-orc is
// running. It reads state and logs and starts nothing of its own, so it can be
// opened and closed at any point without affecting a task.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version.String())
			return
		case "help", "-h", "--help":
			fmt.Println("agent-orc-tui: watch agent-orc tasks\n\nUsage: agent-orc-tui")
			return
		default:
			fmt.Fprintf(os.Stderr, "agent-orc-tui: unknown argument %q\n", os.Args[1])
			os.Exit(2)
		}
	}

	layout, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-orc-tui: %v\n", err)
		os.Exit(1)
	}

	// The alternate screen keeps the dashboard out of the scrollback, so
	// quitting leaves the terminal as it was found.
	if _, err := tea.NewProgram(newModel(layout), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-orc-tui: %v\n", err)
		os.Exit(1)
	}
}
