package task

import (
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestValidateRejectsABadReviewBlock(t *testing.T) {
	tk := valid()
	tk.Review = Review{Enabled: true, CLI: "gemini"}
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "review cli") {
		t.Errorf("Validate() = %v, want an error naming the review cli", err)
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

// TestRenderPlacesInstructionsBetweenTaskAndRules pins the prompt's shape: what
// to do, then how it is done here, then the rules agent-orc imposes.
func TestRenderPlacesInstructionsBetweenTaskAndRules(t *testing.T) {
	got := Task{Prompt: "fix the retry handler", Instructions: "run gofmt before committing"}.Render()

	task := strings.Index(got, "fix the retry handler")
	standing := strings.Index(got, "run gofmt before committing")
	rules := strings.Index(got, "Operating rules for this run")
	if task < 0 || standing < 0 || rules < 0 {
		t.Fatalf("Render() lost a section:\n%s", got)
	}
	if !(task < standing && standing < rules) {
		t.Errorf("sections are out of order (task %d, standing %d, rules %d):\n%s", task, standing, rules, got)
	}
}

// TestRenderWithoutInstructionsIsUnchanged keeps the common case clean: no
// empty header for a task that has no standing instructions.
func TestRenderWithoutInstructionsIsUnchanged(t *testing.T) {
	got := Task{Prompt: "do the thing"}.Render()
	if strings.Contains(got, "Standing instructions") {
		t.Errorf("Render() added an empty instructions section:\n%s", got)
	}
	if !strings.Contains(got, "Operating rules for this run") {
		t.Error("Render() dropped the operating rules")
	}
	// Whitespace-only instructions count as absent too.
	if blank := (Task{Prompt: "do the thing", Instructions: "   \n  "}).Render(); blank != got {
		t.Errorf("blank instructions changed the prompt:\n%s", blank)
	}
}

// TestRawSkipsInstructionsToo covers the reviewer: it is not doing the work, so
// rules about how the work is done do not apply to it.
func TestRawSkipsInstructionsToo(t *testing.T) {
	got := Task{Prompt: "review this diff", Instructions: "run gofmt before committing", Raw: true}.Render()
	if got != "review this diff" {
		t.Errorf("Render() with Raw = %q, want the prompt untouched", got)
	}
}

// TestValidateRejectsABranchThatLooksLikeAFlag covers the one name that stops
// being a name by the time it reaches git. Refusing it here is what turns
// "unknown switch `x'" from inside git worktree add, which names nothing, into
// a message about the branch that caused it.
func TestValidateRejectsABranchThatLooksLikeAFlag(t *testing.T) {
	for _, tt := range []struct{ field, offending, branch, base string }{
		{"branch", "-x", "-x", "main"},
		{"base branch", "-x", "agent-orc/ok", "-x"},
	} {
		tk := valid()
		tk.Branch, tk.BaseBranch = tt.branch, tt.base
		err := tk.Validate()
		if err == nil || !strings.Contains(err.Error(), "must not start with") {
			t.Errorf("Validate() with %s %q = %v, want it refused", tt.field, tt.offending, err)
		}
	}
	// An ordinary branch is untouched by the check.
	tk := valid()
	tk.Branch, tk.BaseBranch = "agent-orc/proj-1", "main"
	if err := tk.Validate(); err != nil {
		t.Errorf("Validate() = %v, want an ordinary branch accepted", err)
	}
}

// TestTestRunTimeout covers the three states the field encodes: unset takes the
// default, negative means run uncapped, and anything else is itself.
func TestTestRunTimeout(t *testing.T) {
	tests := map[time.Duration]time.Duration{
		0:                DefaultTestTimeout,
		NoTestTimeout:    0,
		-time.Hour:       0,
		90 * time.Second: 90 * time.Second,
		45 * time.Minute: 45 * time.Minute,
	}
	for set, want := range tests {
		if got := (Task{TestTimeout: set}).TestRunTimeout(); got != want {
			t.Errorf("Task{TestTimeout: %v}.TestRunTimeout() = %v, want %v", set, got, want)
		}
	}
}

// TestRenderTestRuleFollowsTheThreeStates covers what the agent is told about
// tests. Turning verification off and finding no command both leave the command
// empty, but they are opposite instructions: one is a reason to ask the agent to
// go looking, the other is somebody saying not to run the suite at all, and
// asking anyway spends the task's budget on the one thing it was told to skip.
func TestRenderTestRuleFollowsTheThreeStates(t *testing.T) {
	base := Task{Prompt: "do it"}

	named := base
	named.TestCommand = "pytest -q"
	if got := named.Render(); !strings.Contains(got, "run `pytest -q`") {
		t.Errorf("a known command was not named to the agent:\n%s", got)
	}

	// Nothing found, so the agent is the only one who can look.
	if got := base.Render(); !strings.Contains(got, "If this project has a test suite") {
		t.Errorf("no command left the agent unasked:\n%s", got)
	}

	// Turned off, so tests are not mentioned at all.
	off := base
	off.SkipTests = true
	got := off.Render()
	for _, unwanted := range []string{"test suite", "run `", "make it pass"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("an opted-out task was still told about tests (%q):\n%s", unwanted, got)
		}
	}
	// The rest of the operating rules survive.
	if !strings.Contains(got, "Do NOT push") {
		t.Errorf("the operating rules went missing with the test rule:\n%s", got)
	}
}

// TestReviewNormalizeMakesAutoImplyEnabled covers the invariant that used to be
// written at two construction sites and missing from the third, which left a
// batch file setting only `auto` with no review at all.
func TestReviewNormalizeMakesAutoImplyEnabled(t *testing.T) {
	if got := (Review{Auto: true}).Normalize(); !got.Enabled {
		t.Error("auto did not imply enabled, so the supervisor's Enabled && Auto never fires")
	}
	// It only ever adds: a review that was asked for by hand stays that way,
	// and one nobody asked for is not turned on.
	if got := (Review{Enabled: true}).Normalize(); got.Auto {
		t.Error("enabled turned auto on; review is meant to stay manual unless asked")
	}
	if got := (Review{}).Normalize(); got.Enabled || got.Auto {
		t.Errorf("an empty block became %+v, want it left alone", got)
	}
}
