package orc

import (
	"os/exec"
	"strings"
	"testing"
)

// TestShellCommandSurvivesTheShell covers what the suggestion is for: pasted
// into a shell, every argument arrives as written. A repository path can hold
// spaces and a branch name can hold shell syntax, and quoting either wrongly
// runs something other than what the message said.
func TestShellCommandSurvivesTheShell(t *testing.T) {
	args := []string{
		"plain",
		"",
		"/home/me/my repo",
		"it's",
		"$HOME",
		"x;echo injected",
		"$(echo injected)",
		"`echo injected`",
		"two\nlines",
		`back\slash`,
	}
	out, err := exec.Command("sh", "-c", `printf '%s\0' `+shellCommand(args...)).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(args) {
		t.Fatalf("the shell saw %d arguments %q, want %d", len(got), got, len(args))
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("argument %d arrived as %q, want %q", i, got[i], args[i])
		}
	}
}

func TestShellQuoteLeavesAPlainWordAlone(t *testing.T) {
	for _, s := range []string{"git", "-C", "agent-orc/proj-1", "--"} {
		if got := shellQuote(s); got != s {
			t.Errorf("shellQuote(%q) = %q, want it unquoted", s, got)
		}
	}
}
