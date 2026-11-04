// Package version carries the build-stamped version of the agent-orc binary.
package version

import "runtime/debug"

// Version is the version of agent-orc.
var Version = "dev"

// The linker's -X flag has already replaced "dev" by the time this runs when
// the build stamped a version, so resolve sees the stamped value, not "dev".
func init() {
	info, ok := debug.ReadBuildInfo()
	Version = resolve(Version, info, ok)
}

// resolve keeps a stamped version, and otherwise takes the module version from
// the build info. Go reports "(devel)" for a build of a local checkout, which
// says nothing a reader can use, so that stays "dev".
func resolve(stamped string, info *debug.BuildInfo, ok bool) string {
	if stamped != "dev" || !ok || info == nil {
		return stamped
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return stamped
}

// String returns the version prefixed for display, e.g. "agent-orc dev".
func String() string {
	return "agent-orc " + Version
}
