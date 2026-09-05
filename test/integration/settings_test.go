//go:build integration

package integration

import (
	"os"
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

// TestSettingsRejectATypo keeps a settings file from silently doing nothing.
func TestSettingsRejectATypo(t *testing.T) {
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
