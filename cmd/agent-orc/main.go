// Command agent-orc dispatches agentic CLI runs across isolated git worktrees.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := dispatch(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "agent-orc:", err)
		os.Exit(1)
	}
}
