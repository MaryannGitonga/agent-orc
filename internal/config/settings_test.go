package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
