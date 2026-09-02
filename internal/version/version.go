// Package version carries the build-stamped version of the agent-orc binary.
package version

// Version is the version of agent-orc. The Makefile stamps it at build time
// from `git describe`; a plain `go build` leaves it "dev".
var Version = "dev"

// String returns the version prefixed for display, e.g. "agent-orc dev".
func String() string {
	return "agent-orc " + Version
}
