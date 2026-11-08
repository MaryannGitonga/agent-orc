package main

import (
	"bytes"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/config"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/task"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

// isolate points agent-orc's home at an empty directory, so a command that
// resolves its layout never reads or writes the real ~/.agent-orc.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
}

func TestDispatchVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out bytes.Buffer
		if err := dispatch([]string{arg}, &out); err != nil {
			t.Fatalf("dispatch(%q) = %v", arg, err)
		}
		if got := strings.TrimSpace(out.String()); got != version.String() {
			t.Errorf("dispatch(%q) printed %q, want %q", arg, got, version.String())
		}
	}
}

func TestDispatchWithNoArgumentsPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := dispatch(nil, &out); err != nil {
		t.Fatalf("dispatch(nil) = %v", err)
	}
	if !strings.Contains(out.String(), "agent-orc run") {
		t.Errorf("usage = %q, want it to mention the run command", out.String())
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	err := dispatch([]string{"frobnicate"}, &out)
	if err == nil {
		t.Fatal("dispatch() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error = %q, want it to name the unknown command", err)
	}
}

// TestRunHelpIsNotAnError covers the flow the usage text recommends: `run -h`
// prints the flags and exits successfully.
func TestDispatchHelpPrintsUsage(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var out bytes.Buffer
		if err := dispatch([]string{arg}, &out); err != nil {
			t.Errorf("dispatch(%q) = %v, want nil", arg, err)
		}
		if !strings.Contains(out.String(), "agent-orc run") {
			t.Errorf("dispatch(%q) printed %q, want the usage", arg, out.String())
		}
	}
}

// TestSubcommandsRejectTheWrongArguments covers each command's own usage
// check, which runs before it touches any state.
func TestSubcommandsRejectTheWrongArguments(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"pr"}, "agent-orc pr <task-id>"},
		{[]string{"pr", "A", "B"}, "agent-orc pr <task-id>"},
		{[]string{"review"}, "agent-orc review <task-id>"},
		{[]string{"stop"}, "agent-orc stop <task-id>"},
		{[]string{"status", "A"}, "agent-orc status"},
		{[]string{"sanitize-commit", "A"}, "agent-orc sanitize-commit"},
		{[]string{"supervise"}, "agent-orc supervise <task-id>"},
		{[]string{"supervise", "A", "B", "C"}, "agent-orc supervise <task-id>"},
		{[]string{"logs"}, "agent-orc logs <task-id>"},
		{[]string{"logs", "--bogus", "A"}, "agent-orc logs <task-id>"},
		{[]string{"cleanup", "--bogus", "A"}, "agent-orc cleanup <task-id|--all>"},
		// The flag package names an unknown flag with a single dash.
		{[]string{"run", "--bogus"}, "-bogus"},
	} {
		err := dispatch(tc.argv, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("dispatch(%v) = %v, want an error containing %q", tc.argv, err, tc.want)
		}
	}
}

func TestSubcommandHelpIsNotAnError(t *testing.T) {
	for _, argv := range [][]string{{"logs", "-h"}, {"cleanup", "-h"}} {
		if err := dispatch(argv, io.Discard); err != nil {
			t.Errorf("dispatch(%v) = %v, want nil", argv, err)
		}
	}
}

func TestCleanupNeedsExactlyOneTarget(t *testing.T) {
	isolate(t)
	if err := dispatch([]string{"cleanup", "--all", "A"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not both") {
		t.Errorf("cleanup with an id and --all = %v, want a refusal", err)
	}
	if err := dispatch([]string{"cleanup"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "agent-orc cleanup <task-id|--all>") {
		t.Errorf("cleanup with neither = %v, want the usage", err)
	}
}

func TestSuperviseRejectsAMalformedStartTime(t *testing.T) {
	err := dispatch([]string{"supervise", "A", "yesterday"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"yesterday"`) {
		t.Errorf("supervise with a bad started-at = %v, want an error naming it", err)
	}
}

func TestRunReportsAMissingBatchFile(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := dispatch([]string{"run", path}, io.Discard); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("run with a missing batch file = %v, want an error naming %q", err, path)
	}
}

func TestRunHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var out bytes.Buffer
		if err := dispatch([]string{"run", arg}, &out); err != nil {
			t.Errorf("dispatch(run %s) = %v, want nil", arg, err)
		}
		if !strings.Contains(out.String(), "Usage: agent-orc run") {
			t.Errorf("run %s printed %q, want the run usage", arg, out.String())
		}
	}
}

func TestBuildTaskRequiresIDAndSomethingToDo(t *testing.T) {
	if _, err := buildTask(flags{prompt: "p", repo: "."}); err == nil ||
		!strings.Contains(err.Error(), "--id") {
		t.Errorf("buildTask() without an id = %v, want an error naming --id", err)
	}
	if _, err := buildTask(flags{id: "X", prompt: "  ", repo: "."}); err == nil ||
		!strings.Contains(err.Error(), "--source") {
		t.Errorf("buildTask() with neither prompt nor source = %v, want an error naming both", err)
	}
	if _, err := buildTask(flags{id: "X", prompt: "p", repo: "."}); err == nil ||
		!strings.Contains(err.Error(), "--cli") {
		t.Errorf("buildTask() without a cli = %v, want an error naming --cli", err)
	}
}

func TestBuildTaskAcceptsASourceWithNoPrompt(t *testing.T) {
	// The prompt is filled in from the ticket at launch, so a task with only
	// a source must get past flag validation.
	_, err := buildTask(flags{id: "X", source: "github://o/r#1", repo: t.TempDir(), base: "main", cli: "claude"})
	if err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Errorf("buildTask() = %v, want it to fail only on the missing repository", err)
	}
}

func TestBuildTaskRejectsANonRepository(t *testing.T) {
	if _, err := buildTask(flags{id: "X", prompt: "p", repo: t.TempDir(), base: "main", cli: "claude"}); err == nil {
		t.Error("buildTask() outside a git repository = nil, want an error")
	}
}

func TestBuildTaskTreatsAZeroBudgetAsUnset(t *testing.T) {
	// A zero on the command line means the flag was not given, not "cap this
	// run at nothing".
	got, err := buildTask(flags{id: "X", prompt: "p", repo: ".", base: "main", cli: "claude"})
	if err != nil {
		t.Fatalf("buildTask() = %v", err)
	}
	if !got.Budget.IsZero() {
		t.Errorf("Budget = %+v, want no budget set", got.Budget)
	}

	got, err = buildTask(flags{id: "X", prompt: "p", repo: ".", base: "main", cli: "claude", budgetUSD: 2.5})
	if err != nil {
		t.Fatalf("buildTask() = %v", err)
	}
	if got.Budget.USD == nil || *got.Budget.USD != 2.5 {
		t.Errorf("Budget.USD = %v, want 2.5", got.Budget.USD)
	}
}

func TestBuildTaskSetsACreditBudget(t *testing.T) {
	got, err := buildTask(flags{id: "X", prompt: "p", repo: ".", base: "main", cli: "copilot", budgetCredits: 40})
	if err != nil {
		t.Fatalf("buildTask() = %v", err)
	}
	if got.Budget.Credits == nil || *got.Budget.Credits != 40 {
		t.Errorf("Budget.Credits = %v, want 40", got.Budget.Credits)
	}
}

// settingsWithEverything returns a settings file that sets every field a flag
// can also set, each to something no flag below would pick by default.
func settingsWithEverything() config.Settings {
	yes, no := true, false
	usd, credits := 5.0, 50.0
	return config.Settings{
		CLI:           task.CLIClaude,
		Model:         "file-model",
		BaseBranch:    "file-base",
		Subagents:     &yes,
		BudgetUSD:     &usd,
		BudgetCredits: &credits,
		AutoPR:        &no,
		DCOSignoff:    &yes,
	}
}

func TestApplyDefaultsFillsInWhatWasNotTyped(t *testing.T) {
	// As runCmd builds it with no flags typed: autoPR is true because
	// --no-auto-pr was not given.
	f := flags{autoPR: true}
	f.applyDefaults(settingsWithEverything(), map[string]bool{})

	want := flags{
		cli: "claude", model: "file-model", base: "file-base",
		subagents: true, budgetUSD: 5, budgetCredits: 50,
		// auto_pr: false in the file turns the PR off, though the flag that
		// would do the same is spelled the other way round.
		autoPR: false, dcoSignoff: true,
	}
	if f != want {
		t.Errorf("applyDefaults() = %+v, want %+v", f, want)
	}
}

// TestApplyDefaultsNeverOverrulesATypedFlag pins the rule the comment in
// runCmd states: a bool typed as false and one left at false look the same,
// and only the first may stand against a file.
func TestApplyDefaultsNeverOverrulesATypedFlag(t *testing.T) {
	typed := flags{
		cli: "codex", model: "flag-model", base: "flag-base",
		subagents: false, budgetUSD: 1, budgetCredits: 2,
		autoPR: true, dcoSignoff: false,
	}
	f := typed
	given := map[string]bool{
		"cli": true, "model": true, "base-branch": true, "subagents": true,
		"budget-usd": true, "budget-credits": true, "no-auto-pr": true, "dco-signoff": true,
	}
	f.applyDefaults(settingsWithEverything(), given)
	if f != typed {
		t.Errorf("applyDefaults() = %+v, want the typed flags untouched: %+v", f, typed)
	}
}

func TestRepoRootLeavesANonRepositoryAlone(t *testing.T) {
	dir := t.TempDir()
	if got := repoRoot(dir); got != dir {
		t.Errorf("repoRoot(%q) = %q, want it unchanged", dir, got)
	}
	// A batch with no repo of its own must not pick up the working directory's.
	if got := repoRoot("  "); got != "" {
		t.Errorf("repoRoot(blank) = %q, want empty", got)
	}
}

func TestCLINamesCoversEveryKnownCLI(t *testing.T) {
	if got, want := len(cliNames()), 3; got != want {
		t.Errorf("cliNames() returned %d names, want %d", got, want)
	}
	for _, want := range []string{"claude", "copilot", "codex"} {
		if !strings.Contains(strings.Join(cliNames(), ","), want) {
			t.Errorf("cliNames() is missing %q", want)
		}
	}
}

// TestRunRejectsFlagsAlongsideABatchFile keeps batch settings coming from the
// file: a flag passed with it would otherwise be dropped without a word.
func TestRunRejectsFlagsAlongsideABatchFile(t *testing.T) {
	for _, flags := range [][]string{
		{"--cli", "claude"},
		{"--model", "opus-4-6"},
		{"--branch", "b"},
		{"--base-branch", "main"},
		{"--id", "X"},
	} {
		var out bytes.Buffer
		argv := append(append([]string{"run"}, flags...), "tasks.yaml")
		err := dispatch(argv, &out)
		if err == nil {
			t.Errorf("dispatch(%v) = nil, want a refusal", argv)
			continue
		}
		if !strings.Contains(err.Error(), flags[0]) {
			t.Errorf("dispatch(%v) = %q, want it to name %s", argv, err, flags[0])
		}
	}
}

// TestParseAroundAcceptsFlagsOnEitherSide covers the form the usage lines have
// always advertised. Go's flag package stops parsing at the first positional,
// so `logs <id> -f` was rejected while `logs -f <id>` worked.
func TestParseAroundAcceptsFlagsOnEitherSide(t *testing.T) {
	parse := func(argv []string) (string, bool, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f := fs.Bool("f", false, "")
		id, err := parseAround(fs, argv)
		return id, *f, err
	}

	for _, argv := range [][]string{{"TASK-1", "-f"}, {"-f", "TASK-1"}} {
		id, f, err := parse(argv)
		if err != nil {
			t.Errorf("parseAround(%v) = %v", argv, err)
		}
		if id != "TASK-1" || !f {
			t.Errorf("parseAround(%v) = id %q, f %v; want TASK-1, true", argv, id, f)
		}
	}

	if id, f, err := parse([]string{"TASK-1"}); err != nil || id != "TASK-1" || f {
		t.Errorf("parseAround with no flag = %q, %v, %v; want TASK-1, false, nil", id, f, err)
	}
	if id, _, err := parse(nil); err != nil || id != "" {
		t.Errorf("parseAround with nothing = %q, %v; want an empty id and no error", id, err)
	}
	if _, _, err := parse([]string{"A", "B"}); err == nil {
		t.Error("parseAround with two positionals = nil, want an error")
	}

	// An explicit -- means everything after it is positional, which is the only
	// way to name a task whose id starts with a dash.
	if id, f, err := parse([]string{"--", "-weird-id"}); err != nil || id != "-weird-id" || f {
		t.Errorf("parseAround(-- -weird-id) = %q, %v, %v; want -weird-id, false, nil", id, f, err)
	}
	if id, f, err := parse([]string{"-f", "--", "-weird-id"}); err != nil || id != "-weird-id" || !f {
		t.Errorf("parseAround(-f -- -weird-id) = %q, %v, %v; want -weird-id, true, nil", id, f, err)
	}
	// After --, a dash argument is a stray positional, not a flag to obey.
	if _, f, err := parse([]string{"--", "-weird-id", "-f"}); err == nil || f {
		t.Errorf("parseAround(-- -weird-id -f) = f %v, err %v; want an error and no flag set", f, err)
	}
}
