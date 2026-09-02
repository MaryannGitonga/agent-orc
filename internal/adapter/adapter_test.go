package adapter

import (
	"slices"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// argAfter returns the argument following flag, or "" if it is absent.
func argAfter(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i == -1 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func TestForReturnsAnAdapterPerKnownCLI(t *testing.T) {
	for _, cli := range task.KnownCLIs {
		a, err := For(cli)
		if err != nil {
			t.Errorf("For(%q) = %v, want an adapter", cli, err)
			continue
		}
		if a.Name() != cli {
			t.Errorf("For(%q).Name() = %q, want %q", cli, a.Name(), cli)
		}
	}
}

func TestForRejectsAnUnknownCLI(t *testing.T) {
	_, err := For("gemini")
	if err == nil {
		t.Fatal("For() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error = %q, want it to list the supported CLIs", err)
	}
}

func TestSupportedIsStablyOrdered(t *testing.T) {
	want := []string{"claude", "codex", "copilot"}
	if got := Supported(); !slices.Equal(got, want) {
		t.Errorf("Supported() = %v, want %v", got, want)
	}
}

// TestEveryAdapterCarriesThePromptAndTheModel is the contract every adapter
// has to meet, whatever its own flag names are.
func TestEveryAdapterCarriesThePromptAndTheModel(t *testing.T) {
	tk := task.Task{Prompt: "fix the retry handler", Model: "some-model"}
	for _, cli := range task.KnownCLIs {
		t.Run(string(cli), func(t *testing.T) {
			a, err := For(cli)
			if err != nil {
				t.Fatal(err)
			}
			argv := a.BuildCommand(tk)

			if argv[0] != string(cli) {
				t.Errorf("argv[0] = %q, want %q", argv[0], cli)
			}
			joined := strings.Join(argv, "\x00")
			if !strings.Contains(joined, "fix the retry handler") {
				t.Errorf("argv = %q, want it to carry the prompt", argv)
			}
			if !strings.Contains(joined, "Do NOT push") {
				t.Errorf("argv = %q, want it to carry the rendered operating rules", argv)
			}
			if got := argAfter(argv, "--model"); got != "some-model" {
				t.Errorf("--model = %q, want %q", got, "some-model")
			}
		})
	}
}

// TestEveryAdapterOmitsAnUnsetModel keeps the CLI's own default in play
// instead of passing an empty flag value.
func TestEveryAdapterOmitsAnUnsetModel(t *testing.T) {
	for _, cli := range task.KnownCLIs {
		t.Run(string(cli), func(t *testing.T) {
			a, err := For(cli)
			if err != nil {
				t.Fatal(err)
			}
			if argv := a.BuildCommand(task.Task{Prompt: "x"}); slices.Contains(argv, "--model") {
				t.Errorf("argv = %q, want no --model flag when the task sets none", argv)
			}
		})
	}
}

func TestClaudeUsesPrintModeWithJSONOutput(t *testing.T) {
	argv := Claude{}.BuildCommand(task.Task{Prompt: "x"})
	if argAfter(argv, "--output-format") != "json" {
		t.Errorf("argv = %q, want --output-format json for machine-readable usage", argv)
	}
	if !slices.Contains(argv, "-p") {
		t.Errorf("argv = %q, want print mode", argv)
	}
}

func TestCopilotRunsUnattended(t *testing.T) {
	argv := Copilot{}.BuildCommand(task.Task{Prompt: "x"})
	if !slices.Contains(argv, "--allow-all-tools") {
		t.Errorf("argv = %q, want --allow-all-tools; there is no terminal to approve tool calls", argv)
	}
}

func TestCodexRunsNonInteractivelyInASandbox(t *testing.T) {
	argv := Codex{}.BuildCommand(task.Task{Prompt: "x"})
	if argv[1] != "exec" {
		t.Errorf("argv = %q, want the non-interactive 'codex exec'", argv)
	}
	if argAfter(argv, "--sandbox") != "workspace-write" {
		t.Errorf("argv = %q, want --sandbox workspace-write", argv)
	}
	// Codex takes the prompt as a positional argument, so it must come last.
	if !strings.HasPrefix(argv[len(argv)-1], "x") {
		t.Errorf("argv = %q, want the prompt as the final positional argument", argv)
	}
}
