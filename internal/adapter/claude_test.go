package adapter

import (
	"slices"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func TestClaudeCommandUsesPrintMode(t *testing.T) {
	argv := ClaudeCommand(task.Task{Prompt: "do the thing"})

	if argv[0] != "claude" {
		t.Errorf("argv[0] = %q, want %q", argv[0], "claude")
	}
	if i := slices.Index(argv, "-p"); i == -1 || i+1 >= len(argv) {
		t.Fatalf("argv = %q, want a -p flag with a value", argv)
	} else if !strings.HasPrefix(argv[i+1], "do the thing") {
		t.Errorf("prompt argument = %q, want it to start with the task prompt", argv[i+1])
	}
	if i := slices.Index(argv, "--output-format"); i == -1 || argv[i+1] != "json" {
		t.Errorf("argv = %q, want --output-format json", argv)
	}
}

func TestClaudeCommandPassesTheModelOnlyWhenSet(t *testing.T) {
	withModel := ClaudeCommand(task.Task{Prompt: "x", Model: "opus-4-6"})
	i := slices.Index(withModel, "--model")
	if i == -1 || withModel[i+1] != "opus-4-6" {
		t.Errorf("argv = %q, want --model opus-4-6", withModel)
	}

	// An unset model must leave the flag off entirely so the CLI's own
	// default applies, rather than passing an empty string.
	if got := ClaudeCommand(task.Task{Prompt: "x"}); slices.Contains(got, "--model") {
		t.Errorf("argv = %q, want no --model flag when the task sets no model", got)
	}
}
