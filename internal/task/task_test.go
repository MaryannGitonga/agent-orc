package task

import (
	"strings"
	"testing"
)

func valid() Task {
	return Task{
		ID:         "PROJ-1234",
		Repo:       "/repo",
		Prompt:     "fix the retry handler",
		Branch:     "fix/proj-1234",
		BaseBranch: "main",
		CLI:        CLIClaude,
	}
}

func TestValidateAcceptsAWellFormedTask(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsBadTasks(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Task)
		want   string
	}{
		"empty id":          {func(k *Task) { k.ID = "" }, "id"},
		"path traversal id": {func(k *Task) { k.ID = "../etc" }, "id"},
		"slash in id":       {func(k *Task) { k.ID = "a/b" }, "id"},
		"leading dash id":   {func(k *Task) { k.ID = "-x" }, "id"},
		"blank prompt":      {func(k *Task) { k.Prompt = "   " }, "prompt"},
		"no repo":           {func(k *Task) { k.Repo = "" }, "repo"},
		"no branch":         {func(k *Task) { k.Branch = "" }, "branch"},
		"no base branch":    {func(k *Task) { k.BaseBranch = "" }, "base branch"},
		"unknown cli":       {func(k *Task) { k.CLI = "gemini" }, "unsupported cli"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			task := valid()
			tc.mutate(&task)
			err := task.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	err := Task{}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want errors")
	}
	for _, want := range []string{"id", "prompt", "repo", "branch", "base branch", "cli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q, want it to also mention %q", err, want)
		}
	}
}

func TestDefaultBranchIsNamespacedAndLowercase(t *testing.T) {
	if got, want := DefaultBranch("PROJ-1234"), "agent-orc/proj-1234"; got != want {
		t.Errorf("DefaultBranch() = %q, want %q", got, want)
	}
}

func TestRenderAppendsOperatingRules(t *testing.T) {
	got := valid().Render()
	if !strings.HasPrefix(got, "fix the retry handler") {
		t.Errorf("Render() = %q, want it to start with the task prompt", got)
	}
	for _, want := range []string{"Do NOT push", "do NOT open a pull request", "Co-authored-by"} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() is missing the rule mentioning %q", want)
		}
	}
}
