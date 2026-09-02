package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaryannGitonga/agent-orc/internal/task"
)

const batch = `
repo: /home/me/code/data-mesh
base_branch: main

defaults:
  cli: claude
  model: sonnet-4-6

tasks:
  - id: PROJ-1234
    source: jira://PROJ-1234
    prompt: "Focus on the retry handler"
    branch: fix/proj-1234
    model: opus-4-6

  - id: PROJ-1240
    source: github://canonical/data-mesh#87
    branch: chore/proj-1240
    base_branch: release/2.4
    cli: copilot
    model: gpt-5.1
`

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	f, err := Parse([]byte(batch))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if len(f.Tasks) != 2 {
		t.Fatalf("parsed %d tasks, want 2", len(f.Tasks))
	}

	first := f.Resolved(f.Tasks[0])
	if first.CLI != task.CLIClaude {
		t.Errorf("first task cli = %q, want the default %q", first.CLI, task.CLIClaude)
	}
	if first.Model != "opus-4-6" {
		t.Errorf("first task model = %q, want the override %q", first.Model, "opus-4-6")
	}
	if first.BaseBranch != "main" {
		t.Errorf("first task base branch = %q, want the batch default %q", first.BaseBranch, "main")
	}
	if first.Repo != "/home/me/code/data-mesh" {
		t.Errorf("first task repo = %q, want the batch repo", first.Repo)
	}

	second := f.Resolved(f.Tasks[1])
	if second.CLI != task.CLICopilot {
		t.Errorf("second task cli = %q, want the override %q", second.CLI, task.CLICopilot)
	}
	if second.BaseBranch != "release/2.4" {
		t.Errorf("second task base branch = %q, want the override %q", second.BaseBranch, "release/2.4")
	}
	if second.Prompt != "" {
		t.Errorf("second task prompt = %q, want it empty so the source supplies it", second.Prompt)
	}
}

func TestParseRejectsBadBatchFiles(t *testing.T) {
	tests := map[string]struct{ yaml, want string }{
		"no tasks":         {"repo: /x\n", "no tasks"},
		"missing id":       {"tasks:\n  - prompt: do it\n", "id is required"},
		"nothing to do":    {"tasks:\n  - id: A\n", "needs a source"},
		"duplicate id":     {"tasks:\n  - id: A\n    prompt: x\n  - id: A\n    prompt: y\n", "duplicate id"},
		"duplicate branch": {"tasks:\n  - id: A\n    prompt: x\n    branch: b\n  - id: B\n    prompt: y\n    branch: b\n", "already used"},
		"unknown cli":      {"tasks:\n  - id: A\n    prompt: x\n    cli: gemini\n", "unsupported cli"},
		"unknown default":  {"defaults:\n  cli: gemini\ntasks:\n  - id: A\n    prompt: x\n", "unsupported cli"},
		"no cli anywhere":  {"tasks:\n  - id: A\n    prompt: x\n", "no cli set"},
		"unknown field":    {"tasks:\n  - id: A\n    prompt: x\n    base-branch: main\n", "base-branch"},
		"not yaml":         {"tasks: [", "parsing yaml"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadResolvesRelativeRepoPathsAgainstTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.yaml")
	body := "repo: ./checkout\ndefaults:\n  cli: claude\ntasks:\n  - id: A\n    prompt: x\n  - id: B\n    prompt: y\n    repo: ../other\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if want := filepath.Join(dir, "checkout"); f.Repo != want {
		t.Errorf("Repo = %q, want %q", f.Repo, want)
	}
	if want := filepath.Join(filepath.Dir(dir), "other"); f.Tasks[1].Repo != want {
		t.Errorf("task repo = %q, want %q", f.Tasks[1].Repo, want)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("Load() on a missing file = nil, want an error")
	}
}

// TestParseRejectsACaseFoldedDefaultBranchCollision catches ids that differ
// only in case before anything launches: both lowercase to one default branch.
func TestParseRejectsACaseFoldedDefaultBranchCollision(t *testing.T) {
	_, err := Parse([]byte("defaults:\n  cli: claude\ntasks:\n  - id: Foo\n    prompt: x\n  - id: foo\n    prompt: y\n"))
	if err == nil {
		t.Fatal("Parse() = nil, want a branch collision error")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Errorf("Parse() = %q, want it to report the branch collision", err)
	}
}

// TestParseAllowsDistinctDefaultBranches keeps the stricter check from firing
// on ordinary batches where every id maps to its own branch.
func TestParseAllowsDistinctDefaultBranches(t *testing.T) {
	if _, err := Parse([]byte("defaults:\n  cli: claude\ntasks:\n  - id: A\n    prompt: x\n  - id: B\n    prompt: y\n")); err != nil {
		t.Errorf("Parse() = %v, want nil", err)
	}
}
