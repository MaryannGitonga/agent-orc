package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	built := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}
	}
	cases := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"stamp wins over the module version", "v0.3.0", built("v0.4.0"), true, "v0.3.0"},
		{"go install takes the module version", "dev", built("v0.4.0"), true, "v0.4.0"},
		{"a local checkout stays dev", "dev", built("(devel)"), true, "dev"},
		{"no module version stays dev", "dev", built(""), true, "dev"},
		{"no build info stays dev", "dev", nil, false, "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolve(c.stamped, c.info, c.ok); got != c.want {
				t.Errorf("resolve(%q) = %q, want %q", c.stamped, got, c.want)
			}
		})
	}
}

func TestStringIncludesVersion(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, "agent-orc ") {
		t.Errorf("String() = %q, want it to start with %q", got, "agent-orc ")
	}
	if !strings.Contains(got, Version) {
		t.Errorf("String() = %q, want it to contain Version %q", got, Version)
	}
}

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Error("Version is empty, want a non-empty default")
	}
}
