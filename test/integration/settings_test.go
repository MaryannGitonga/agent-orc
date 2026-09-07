//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSettingsLayering covers the three places a setting can come from and the
// order they win in: the machine-wide file, the repository's own file, then
// the flags actually typed. It exists so nobody has to retype --cli, --model
// and the whole review block on every run.
func TestSettingsLayering(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "receipt"), "true")

	write(t, filepath.Join(home, "defaults.yaml"), `
cli: claude
model: claude-sonnet-5
dco_signoff: true
review:
  enabled: true
  cli: claude
  model: claude-opus-5
`)

	// Nothing but the id and the prompt: everything else is inherited.
	if out, err := orcRun(t, home, stub, "run",
		"--id", "SET-1", "--repo", repo, "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got := waitForStatus(t, home, "SET-1", "done", "failed")
	if got.CLI != "claude" || got.Model != "claude-sonnet-5" {
		t.Errorf("cli/model = %q/%q, want them inherited from the machine-wide file", got.CLI, got.Model)
	}
	if !got.DCOSignoff {
		t.Error("dco_signoff was not inherited")
	}
	if !got.Review.Enabled || got.Review.Model != "claude-opus-5" {
		t.Errorf("review = %+v, want it inherited whole", got.Review)
	}

	// A repository says what is true of that repository, and wins.
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "model: claude-opus-5\nreview:\n  cli: copilot\n")
	if out, err := orcRun(t, home, stub, "run",
		"--id", "SET-2", "--repo", repo, "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got = waitForStatus(t, home, "SET-2", "done", "failed")
	if got.Model != "claude-opus-5" || got.Review.CLI != "copilot" {
		t.Errorf("model/review cli = %q/%q, want the repository's overrides", got.Model, got.Review.CLI)
	}
	if got.CLI != "claude" || !got.Review.Enabled || got.Review.Model != "claude-opus-5" {
		t.Errorf("cli/review = %q/%+v, want what the repository did not override left alone", got.CLI, got.Review)
	}

	// A flag beats both, and a bool flag left off is not an override.
	if out, err := orcRun(t, home, stub, "run",
		"--id", "SET-3", "--repo", repo, "--prompt", "do it", "--no-auto-pr",
		"--cli", "copilot", "--review-model", "gpt-5.1"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	got = waitForStatus(t, home, "SET-3", "done", "failed")
	if got.CLI != "copilot" || got.Review.Model != "gpt-5.1" {
		t.Errorf("cli/review model = %q/%q, want the flags to win", got.CLI, got.Review.Model)
	}
	if !got.DCOSignoff {
		t.Error("dco_signoff was dropped by an unrelated flag")
	}
}

// TestSettingsRejectsATypo keeps a settings file from silently doing nothing.
func TestSettingsRejectsATypo(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(home, "defaults.yaml"), "dco_signof: true\n")

	out, err := orcRun(t, home, stub, "run",
		"--id", "SET-4", "--repo", repo, "--cli", "claude", "--prompt", "do it")
	if err == nil {
		t.Fatalf("agent-orc run = nil, want a refusal\n%s", out)
	}
	if !strings.Contains(out, "dco_signof") {
		t.Errorf("run = %q, want it to name the unknown field", out)
	}
}

// TestSettingsSupplyTheCLI checks the file can satisfy the one flag that is
// otherwise required, since not retyping --cli is most of the point.
func TestSettingsSupplyTheCLI(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	// With nothing set anywhere it is still required.
	out, err := orcRun(t, home, stub, "run", "--id", "SET-5", "--repo", repo, "--prompt", "do it")
	if err == nil {
		t.Fatalf("agent-orc run = nil, want --cli to still be required\n%s", out)
	}
	if !strings.Contains(out, "--cli is required") {
		t.Errorf("run = %q, want it to say --cli is required", out)
	}

	if err := os.WriteFile(filepath.Join(repo, ".agent-orc.yaml"), []byte("cli: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := orcRun(t, home, stub, "run",
		"--id", "SET-5", "--repo", repo, "--prompt", "do it", "--no-auto-pr"); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if got := waitForStatus(t, home, "SET-5", "done", "failed"); got.CLI != "claude" {
		t.Errorf("cli = %q, want it supplied by the repository file", got.CLI)
	}
}

// TestSettingsFoundFromASubdirectory covers where the repository's own file is
// looked for. --repo defaults to the working directory, so a run started
// anywhere but the top of the repository would read settings from a directory
// that does not have them, while the dispatch that follows resolves the same
// path to the git root and works on the right repository regardless.
func TestSettingsFoundFromASubdirectory(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	write(t, filepath.Join(repo, ".agent-orc.yaml"), "cli: claude\nmodel: from-repo-file\n")

	deep := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	// No --repo and no --cli: both have to come from the repository's file,
	// found from a directory well inside the tree.
	cmd := exec.Command(buildBinary(t), "run", "--id", "SUBDIR-1", "--prompt", "do it", "--no-auto-pr")
	cmd.Dir = deep
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Env = append(cmd.Env,
		"AGENT_ORC_HOME="+home,
		"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent-orc run from %s = %v\n%s", deep, err, out)
	}

	got := waitForStatus(t, home, "SUBDIR-1", "done", "failed")
	if got.CLI != "claude" || got.Model != "from-repo-file" {
		t.Errorf("cli/model = %q/%q, want them read from the repository root", got.CLI, got.Model)
	}
}

// TestBatchWithoutARepoTakesNoRepoSettings covers a boundary between
// repositories. A batch file that names no repo of its own has no repository
// layer to read, and resolving that emptiness to the directory agent-orc was
// run from would have its tasks inherit settings from whatever repository the
// user happened to be standing in, which is not theirs.
func TestBatchWithoutARepoTakesNoRepoSettings(t *testing.T) {
	home := t.TempDir()
	target := initRepo(t)
	// An unrelated repository, with settings of its own, that the command is
	// run from.
	elsewhere := initRepo(t)
	write(t, filepath.Join(elsewhere, ".agent-orc.yaml"), "model: from-somewhere-else\n")

	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	// No top-level repo; the task names its own.
	write(t, batch, "defaults:\n  cli: claude\ntasks:\n  - id: NOREPO-1\n    prompt: do it\n    auto_pr: false\n    repo: "+target+"\n")

	cmd := exec.Command(buildBinary(t), "run", batch)
	cmd.Dir = elsewhere
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Env = append(cmd.Env,
		"AGENT_ORC_HOME="+home,
		"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}

	if got := waitForStatus(t, home, "NOREPO-1", "done", "failed"); got.Model != "" {
		t.Errorf("model = %q, want nothing inherited from the directory it was run in", got.Model)
	}
}

// TestBatchWithARepoTakesItsSettings is the other half: naming a repo is what
// asks for that repository's file, and it still arrives.
func TestBatchWithARepoTakesItsSettings(t *testing.T) {
	home := t.TempDir()
	target := initRepo(t)
	write(t, filepath.Join(target, ".agent-orc.yaml"), "model: from-the-target\n")
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")

	batch := filepath.Join(t.TempDir(), "tasks.yaml")
	write(t, batch, "repo: "+target+"\ndefaults:\n  cli: claude\ntasks:\n  - id: REPO-1\n    prompt: do it\n    auto_pr: false\n")

	if out, err := orcRun(t, home, stub, "run", batch); err != nil {
		t.Fatalf("agent-orc run = %v\n%s", err, out)
	}
	if got := waitForStatus(t, home, "REPO-1", "done", "failed"); got.Model != "from-the-target" {
		t.Errorf("model = %q, want the named repository's own setting", got.Model)
	}
}

// TestReviewFlagsBeatADefaultsFile covers a flag that a file could overrule.
// review.auto implies review.enabled, so a machine-wide file saying reviews
// here are automatic could turn one back on after an explicit --review=false,
// and make automatic a review that --review asked for by hand.
func TestReviewFlagsBeatADefaultsFile(t *testing.T) {
	repo := initRepo(t)
	home := t.TempDir()
	stub := stubAgent(t, "claude", filepath.Join(t.TempDir(), "receipt"), "true")
	stubInto(t, stub, "copilot", filepath.Join(t.TempDir(), "reviewer"), "true")
	write(t, filepath.Join(home, "defaults.yaml"), "cli: claude\nreview:\n  auto: true\n  cli: copilot\n")

	tests := []struct {
		id            string
		flag          string
		enabled, auto bool
	}{
		{"RF-1", "", true, true},                 // the file applies when nothing was typed
		{"RF-2", "--review=false", false, false}, // off means off, auto included
		{"RF-3", "--review=true", true, false},   // asked for by hand, so it stays by hand
		{"RF-4", "--auto-review", true, true},    // asked for automatically
		{"RF-5", "--auto-review=false", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			args := []string{"run", "--id", tt.id, "--repo", repo, "--prompt", "do it", "--no-auto-pr"}
			if tt.flag != "" {
				args = append(args, tt.flag)
			}
			if out, err := orcRun(t, home, stub, args...); err != nil {
				t.Fatalf("agent-orc run %s = %v\n%s", tt.flag, err, out)
			}
			got := waitForStatus(t, home, tt.id, "done", "failed", "reviewed", "review_failed")
			if got.Review.Enabled != tt.enabled || got.Review.Auto != tt.auto {
				t.Errorf("with %q: enabled/auto = %v/%v, want %v/%v",
					tt.flag, got.Review.Enabled, got.Review.Auto, tt.enabled, tt.auto)
			}
		})
	}
}
