package task

import (
	"reflect"
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

func TestReviewerCLIDefaultsToADifferentCLI(t *testing.T) {
	// An unset reviewer should not share the worker's blind spots, so it
	// defaults to some other CLI rather than the same one.
	for _, worker := range KnownCLIs {
		got := Task{CLI: worker}.ReviewerCLI()
		if got == worker {
			t.Errorf("worker %q got reviewer %q, want a different CLI", worker, got)
		}
		if !got.Known() {
			t.Errorf("worker %q got reviewer %q, which is not a known CLI", worker, got)
		}
	}
}

func TestReviewerCLIHonoursAnExplicitChoice(t *testing.T) {
	tk := Task{CLI: CLIClaude, Review: Review{CLI: CLIClaude}}
	if got := tk.ReviewerCLI(); got != CLIClaude {
		t.Errorf("ReviewerCLI() = %q, want the configured %q even when it matches the worker", got, CLIClaude)
	}
}

func TestReviewRoundsDefaultsToOne(t *testing.T) {
	tests := map[int]int{0: 1, -1: 1, 3: 3}
	for set, want := range tests {
		if got := (Review{MaxRounds: set}).Rounds(); got != want {
			t.Errorf("Review{MaxRounds: %d}.Rounds() = %d, want %d", set, got, want)
		}
	}
}

func TestValidateRejectsABadReviewBlock(t *testing.T) {
	tk := valid()
	tk.Review = Review{Enabled: true, CLI: "gemini"}
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "review cli") {
		t.Errorf("Validate() = %v, want an error naming the review cli", err)
	}

	tk = valid()
	tk.Review = Review{Enabled: true, MaxRounds: -2}
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "max_rounds") {
		t.Errorf("Validate() = %v, want an error naming max_rounds", err)
	}
}

// TestRenderRawOmitsTheOperatingRules covers agent-orc's own prompts. The
// reviewer is told not to edit anything, so appending rules that tell it to
// commit would contradict its instructions inside a checkout of the branch it
// is reviewing.
func TestRenderRawOmitsTheOperatingRules(t *testing.T) {
	raw := Task{Prompt: "review this diff", Raw: true}.Render()
	if raw != "review this diff" {
		t.Errorf("Render() with Raw = %q, want the prompt untouched", raw)
	}
	for _, rule := range []string{"Commit your work locally", "Operating rules for this run"} {
		if strings.Contains(raw, rule) {
			t.Errorf("Render() with Raw still carries %q", rule)
		}
	}
	// A worker still gets them; Raw is not a way to opt out of the policy.
	worker := Task{Prompt: "do the thing"}.Render()
	if !strings.Contains(worker, "Commit your work locally") {
		t.Error("Render() without Raw dropped the operating rules")
	}
}

// TestRawIsNotConfigurable keeps Raw an internal switch: a task file must not
// be able to shed the operating rules.
func TestRawIsNotConfigurable(t *testing.T) {
	f, ok := reflect.TypeOf(Task{}).FieldByName("Raw")
	if !ok {
		t.Fatal("Task has no Raw field")
	}
	if got := f.Tag.Get("yaml"); got != "-" {
		t.Errorf("Raw yaml tag = %q, want \"-\" so a batch file cannot set it", got)
	}
	if got := f.Tag.Get("json"); got != "-" {
		t.Errorf("Raw json tag = %q, want \"-\" so it is never persisted", got)
	}
}
