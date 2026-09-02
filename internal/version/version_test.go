package version

import (
	"strings"
	"testing"
)

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
