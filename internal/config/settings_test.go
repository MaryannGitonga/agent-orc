package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func boolPtr(b bool) *bool      { return &b }
func fltPtr(f float64) *float64 { return &f }

func TestParseSettings(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    Settings
		wantErr string
	}{{
		name: "a full file",
		yaml: `
cli: claude
model: claude-sonnet-5
base_branch: develop
dco_signoff: true
auto_pr: false
budget_usd: 2.5
review:
  enabled: true
  cli: copilot
  model: claude-opus-5
`,
		want: Settings{
			CLI: task.CLIClaude, Model: "claude-sonnet-5", BaseBranch: "develop",
			DCOSignoff: boolPtr(true), AutoPR: boolPtr(false), BudgetUSD: fltPtr(2.5),
			Review: &Review{
				Enabled: boolPtr(true), CLI: task.CLICopilot, Model: "claude-opus-5",
			},
		},
	}, {
		// An empty file has to mean the same as no file, or dropping every
		// setting out of one would start failing runs.
		name: "an empty file is not an error",
		yaml: "",
		want: Settings{},
	}, {
		name:    "a typo is rejected rather than ignored",
		yaml:    "dco_signof: true\n",
		wantErr: "dco_signof",
	}, {
		name:    "an unknown cli is caught before anything is dispatched",
		yaml:    "cli: gemini\n",
		wantErr: `unsupported cli "gemini"`,
	}, {
		name:    "an unknown review cli is caught too",
		yaml:    "review:\n  cli: gemini\n",
		wantErr: `unsupported review cli "gemini"`,
	}, {
		// The cap is gone; a file still naming it is a file that means
		// something it will not get, which is what strict fields are for.
		name:    "a round cap is no longer a field",
		yaml:    "review:\n  max_rounds: 2\n",
		wantErr: "max_rounds",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSettings([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseSettings() error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSettings() = %v", err)
			}
			if got.CLI != tt.want.CLI || got.Model != tt.want.Model || got.BaseBranch != tt.want.BaseBranch {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if !samePtrBool(got.DCOSignoff, tt.want.DCOSignoff) || !samePtrBool(got.AutoPR, tt.want.AutoPR) {
				t.Errorf("bools: got dco=%v auto_pr=%v", got.DCOSignoff, got.AutoPR)
			}
			if (got.Review == nil) != (tt.want.Review == nil) {
				t.Fatalf("review block: got %v, want %v", got.Review, tt.want.Review)
			}
			if got.Review != nil && got.Review.CLI != tt.want.Review.CLI {
				t.Errorf("review: got %+v, want %+v", got.Review, tt.want.Review)
			}
		})
	}
}

func samePtrBool(a, b *bool) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// TestSettingsMerge is the whole point of the layering: the narrower file says
// only what differs, and can turn an inherited setting off as well as on.
func TestSettingsMerge(t *testing.T) {
	global := Settings{
		CLI: task.CLIClaude, Model: "claude-sonnet-5", DCOSignoff: boolPtr(true),
		Review: &Review{Enabled: boolPtr(true), CLI: task.CLIClaude, Model: "claude-opus-5"},
	}
	repo := Settings{
		Model:      "claude-opus-5",
		DCOSignoff: boolPtr(false),
		Review:     &Review{CLI: task.CLICopilot},
	}

	got := global.Merge(repo)
	if got.CLI != task.CLIClaude {
		t.Errorf("cli = %q, want the inherited claude", got.CLI)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("model = %q, want the repository's override", got.Model)
	}
	if got.DCOSignoff == nil || *got.DCOSignoff {
		t.Errorf("dco_signoff = %v, want an explicit false to win", got.DCOSignoff)
	}
	if got.Review.CLI != task.CLICopilot {
		t.Errorf("review cli = %q, want the repository's override", got.Review.CLI)
	}
	if got.Review.Enabled == nil || !*got.Review.Enabled {
		t.Error("review enabled was lost; a partial review block must not drop the rest")
	}
	if got.Review.Model != "claude-opus-5" {
		t.Errorf("review model = %q, want the inherited claude-opus-5", got.Review.Model)
	}
}

func TestLoadSettingsMissingFileIsNotAnError(t *testing.T) {
	got, err := LoadSettings(filepath.Join(t.TempDir(), "nothing.yaml"))
	if err != nil {
		t.Fatalf("LoadSettings(missing) = %v, want no error", err)
	}
	if got.CLI != "" {
		t.Errorf("got %+v, want the zero settings", got)
	}
}

func TestLoadRepoSettings(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, RepoFile), []byte("dco_signoff: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRepoSettings(repo)
	if err != nil {
		t.Fatalf("LoadRepoSettings() = %v", err)
	}
	if got.DCOSignoff == nil || !*got.DCOSignoff {
		t.Errorf("dco_signoff = %v, want true", got.DCOSignoff)
	}
	// A batch file that names no repository has nowhere to read one from.
	if got, err := LoadRepoSettings(""); err != nil || got.DCOSignoff != nil {
		t.Errorf("LoadRepoSettings(\"\") = %+v, %v; want the zero settings", got, err)
	}
}

// TestFileLayerUnder covers the batch path: the file's own settings stay on
// top, and everything it leaves unset comes from the layers beneath.
func TestFileLayerUnder(t *testing.T) {
	f := &File{
		Defaults: Defaults{Model: "claude-opus-5"},
		Tasks:    []Entry{{ID: "a", Prompt: "do it"}},
	}
	f.LayerUnder(Settings{
		CLI: task.CLIClaude, Model: "claude-sonnet-5", BaseBranch: "develop",
		DCOSignoff: boolPtr(true),
		Review:     &Review{Enabled: boolPtr(true), Auto: boolPtr(true)},
	})

	got := f.Resolved(f.Tasks[0])
	if got.CLI != task.CLIClaude {
		t.Errorf("cli = %q, want the inherited claude", got.CLI)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("model = %q, want the batch file's own value to win", got.Model)
	}
	if got.BaseBranch != "develop" {
		t.Errorf("base_branch = %q, want the inherited develop", got.BaseBranch)
	}
	if !got.DCOSignoff {
		t.Error("dco_signoff was not inherited")
	}
	if !got.Review.Enabled || !got.Review.Auto {
		t.Errorf("review = %+v, want it inherited whole", got.Review)
	}
}

// TestParseTestTimeout covers the one value that has to be readable by hand and
// unambiguous to the loop: how long a single run of the suite may take.
func TestParseTestTimeout(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: "", want: 0},                        // unset, so the default applies
		{in: "none", want: task.NoTestTimeout},   // uncapped, for a suite that really is long
		{in: " none ", want: task.NoTestTimeout}, // written with the whitespace people leave
		{in: "90s", want: 90 * time.Second},
		{in: "10m", want: 10 * time.Minute},
		{in: "2h30m", want: 150 * time.Minute},
		// A cap of zero would kill every suite the moment it started, so it is
		// rejected rather than quietly read as "no cap".
		{in: "0", wantErr: "must be positive"},
		{in: "-5m", wantErr: "must be positive"},
		{in: "soon", wantErr: "is not a duration"},
		{in: "30", wantErr: "is not a duration"}, // a bare number has no unit
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseTestTimeout(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseTestTimeout(%q) error = %v, want it to mention %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTestTimeout(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseTestTimeout(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestFileLayerUnderCarriesEveryField guards the batch layer as a whole rather
// than one field at a time. LayerUnder has to feed each of the batch's own
// settings into the merge and take each one back out again, and a field missing
// from either half is silent: the batch value is not merely ignored, it is
// overwritten by the broader layer on the way back.
func TestFileLayerUnderCarriesEveryField(t *testing.T) {
	// Every field set on both sides, with different values, so anything the
	// batch sets has to win.
	broad := Settings{
		CLI: task.CLICopilot, Model: "broad-model", BaseBranch: "broad-base",
		Subagents: boolPtr(false), BudgetUSD: fltPtr(1), BudgetCredits: fltPtr(1),
		AutoPR: boolPtr(false), DCOSignoff: boolPtr(false),
		TestCommand: "broad-tests", TestTimeout: "30m",
		Review: &Review{Enabled: boolPtr(false), CLI: task.CLICopilot, Model: "broad-reviewer"},
	}
	f := &File{
		BaseBranch: "batch-base",
		DCOSignoff: true,
		Defaults: Defaults{
			CLI: task.CLIClaude, Model: "batch-model",
			Subagents: boolPtr(true), BudgetUSD: fltPtr(2), BudgetCredits: fltPtr(2),
			AutoPR:      boolPtr(true),
			TestCommand: "batch-tests", TestTimeout: "5m",
			Review: &Review{Enabled: boolPtr(true), CLI: task.CLIClaude, Model: "batch-reviewer"},
		},
		Tasks: []Entry{{ID: "a", Prompt: "do it"}},
	}
	f.LayerUnder(broad)

	got := f.Resolved(f.Tasks[0])
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"cli", got.CLI, task.CLIClaude},
		{"model", got.Model, "batch-model"},
		{"base_branch", got.BaseBranch, "batch-base"},
		{"subagents", got.Subagents, true},
		{"budget_usd", *got.Budget.USD, 2.0},
		{"budget_credits", *got.Budget.Credits, 2.0},
		{"auto_pr", got.AutoPR, true},
		{"dco_signoff", got.DCOSignoff, true},
		{"test_command", got.TestCommand, "batch-tests"},
		{"test_timeout", got.TestTimeout, 5 * time.Minute},
		{"review.enabled", got.Review.Enabled, true},
		{"review.cli", got.Review.CLI, task.CLIClaude},
		{"review.model", got.Review.Model, "batch-reviewer"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want the batch's %v", c.field, c.got, c.want)
		}
	}

	// The other direction, which is what the second half of LayerUnder carries:
	// a batch that says nothing has to end up with everything the broader layer
	// said. Checking only the first direction would miss a field dropped from
	// the write-back, since the batch's own value survives that untouched.
	empty := &File{Tasks: []Entry{{ID: "a", Prompt: "do it"}}}
	empty.LayerUnder(broad)
	inherited := empty.Resolved(empty.Tasks[0])
	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"cli", inherited.CLI, task.CLICopilot},
		{"model", inherited.Model, "broad-model"},
		{"base_branch", inherited.BaseBranch, "broad-base"},
		{"subagents", inherited.Subagents, false},
		{"budget_usd", *inherited.Budget.USD, 1.0},
		{"budget_credits", *inherited.Budget.Credits, 1.0},
		{"auto_pr", inherited.AutoPR, false},
		{"dco_signoff", inherited.DCOSignoff, false},
		{"test_command", inherited.TestCommand, "broad-tests"},
		{"test_timeout", inherited.TestTimeout, 30 * time.Minute},
		{"review.enabled", inherited.Review.Enabled, false},
		{"review.cli", inherited.Review.CLI, task.CLICopilot},
		{"review.model", inherited.Review.Model, "broad-reviewer"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want the inherited %v", c.field, c.got, c.want)
		}
	}
}

// TestParsersRejectASecondDocument covers the one mistake these parsers used to
// make silently. They reject an unknown field so that a typo fails where it was
// made; dropping a whole document is larger than any typo, and a batch file with
// tasks after a "---" dispatched the first half and never mentioned the rest.
func TestParsersRejectASecondDocument(t *testing.T) {
	t.Run("settings", func(t *testing.T) {
		_, err := ParseSettings([]byte("cli: claude\n---\nmodel: ignored\n"))
		if err == nil || !strings.Contains(err.Error(), "more than one document") {
			t.Errorf("ParseSettings() = %v, want it to refuse a second document", err)
		}
	})

	t.Run("batch", func(t *testing.T) {
		body := "defaults:\n  cli: claude\ntasks:\n  - id: A\n    prompt: x\n---\ntasks:\n  - id: B\n    prompt: y\n"
		if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "more than one document") {
			t.Errorf("Parse() = %v, want it to refuse rather than dispatch half the file", err)
		}
	})

	// A leading separator is one document, not two, and is how plenty of tools
	// write yaml. It has to keep working.
	t.Run("a leading separator is still one document", func(t *testing.T) {
		got, err := ParseSettings([]byte("---\ncli: claude\n"))
		if err != nil {
			t.Fatalf("ParseSettings() = %v", err)
		}
		if got.CLI != task.CLIClaude {
			t.Errorf("cli = %q, want it parsed normally", got.CLI)
		}
	})

	// And an empty file still means an empty settings file, not a failure.
	t.Run("empty", func(t *testing.T) {
		if _, err := ParseSettings(nil); err != nil {
			t.Errorf("ParseSettings(nil) = %v, want no error", err)
		}
	})
}
