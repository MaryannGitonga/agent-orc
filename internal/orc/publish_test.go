package orc

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/forge"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// answerRunner answers every command with the same output and error, which is
// all the helpers below ask of git: one command each.
type answerRunner struct {
	out string
	err error
}

func (r answerRunner) Run(string, ...string) (string, error) { return r.out, r.err }

func publisherAnswering(out string, err error) *Publisher {
	return &Publisher{opener: &forge.Opener{Runner: answerRunner{out, err}}, out: io.Discard}
}

func TestTitleNamesTheTaskOnce(t *testing.T) {
	record := state.Task{Task: task.Task{ID: "PROJ-7"}}
	tests := map[string]struct {
		subject string
		err     error
		want    string
	}{
		"a subject without the id":       {"fix: retry the upload\n", nil, "PROJ-7: fix: retry the upload"},
		"a subject that already has it":  {"fix(PROJ-7): retry the upload\n", nil, "fix(PROJ-7): retry the upload"},
		"git log failing":                {"", errors.New("exit status 128"), "PROJ-7"},
		"a subject that is only spacing": {"  \n", nil, "PROJ-7"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := publisherAnswering(tc.subject, tc.err).title(record); got != tc.want {
				t.Errorf("title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBodySaysWhereTheChangeCameFrom(t *testing.T) {
	record := state.Task{Task: task.Task{
		ID: "PROJ-7", CLI: task.CLIClaude, Model: "opus", BaseBranch: "main",
		Source: "github://org/repo#7",
	}}
	got := publisherAnswering("", nil).body(record, 3)
	for _, want := range []string{
		"task `PROJ-7` (claude, opus), 3 commit(s) on `main`",
		"Source: github://org/repo#7",
		"Opened as a draft: not yet human-checked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body is missing %q:\n%s", want, got)
		}
	}
}

func TestBodyLeavesOutWhatIsNotKnown(t *testing.T) {
	record := state.Task{Task: task.Task{ID: "PROJ-8", CLI: task.CLICodex, BaseBranch: "main"}}
	got := publisherAnswering("", nil).body(record, 1)
	if !strings.Contains(got, "(codex), 1 commit(s)") {
		t.Errorf("body = %q, want the CLI alone when no model was set", got)
	}
	if strings.Contains(got, "Source:") {
		t.Errorf("body = %q, want no source line for a task typed as a prompt", got)
	}
}

func TestCountCommits(t *testing.T) {
	record := state.Task{Task: task.Task{ID: "PROJ-7", Branch: "agent-orc/proj-7", BaseBranch: "main"}}

	if n, err := publisherAnswering("4\n", nil).countCommits(record); err != nil || n != 4 {
		t.Errorf("countCommits() = %d, %v, want 4", n, err)
	}
	// Anything but a number means git answered some other question, and
	// treating it as zero would report an agent that did work as one that
	// committed nothing.
	if _, err := publisherAnswering("fatal: bad revision\n", nil).countCommits(record); err == nil ||
		!strings.Contains(err.Error(), "unexpected output") {
		t.Errorf("countCommits() on non-numeric output = %v, want an error quoting it", err)
	}
	if _, err := publisherAnswering("unknown revision", errors.New("exit status 128")).countCommits(record); err == nil ||
		!strings.Contains(err.Error(), "agent-orc/proj-7") {
		t.Errorf("countCommits() on a git failure = %v, want an error naming the branch", err)
	}
}
