// Package version carries the build-stamped version of the agent-orc binary.
package version

// Version is the released version of agent-orc. Release builds override it
// with -ldflags "-X github.com/MaryannGitonga/agent-orc/internal/version.Version=v1.2.3".
var Version = "dev"

// String returns the version prefixed for display, e.g. "agent-orc dev".
func String() string {
	return "agent-orc " + Version
}
