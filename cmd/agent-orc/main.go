// Command agent-orc dispatches agentic CLI runs across isolated git worktrees.
package main

import (
	"fmt"
	"os"

	"github.com/MaryannGitonga/agent-orc/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "agent-orc:", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(out, version.String())
		return nil
	}
	fmt.Fprintln(out, usage)
	return nil
}

const usage = `agent-orc: dispatch agentic CLI runs across isolated git worktrees

Usage:
  agent-orc version   print the version`
