package seed

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes a set of files under root.
func tree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourcePrefersTheUserLibrary(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	tree(t, root, map[string]string{"claude/reviewer.md": "library"})
	tree(t, repo, map[string]string{".claude/agents/reviewer.md": "repo"})

	got, err := Library{Root: root}.Source("claude", repo, ".claude/agents")
	if err != nil {
		t.Fatalf("Source() = %v", err)
	}
	if want := filepath.Join(root, "claude"); got != want {
		t.Errorf("Source() = %q, want the library %q", got, want)
	}
}

func TestSourceFallsBackToTheRepository(t *testing.T) {
	repo := t.TempDir()
	tree(t, repo, map[string]string{".claude/agents/reviewer.md": "repo"})

	got, err := Library{Root: t.TempDir()}.Source("claude", repo, ".claude/agents")
	if err != nil {
		t.Fatalf("Source() = %v", err)
	}
	if want := filepath.Join(repo, ".claude/agents"); got != want {
		t.Errorf("Source() = %q, want the repository's own %q", got, want)
	}
}

func TestSourceReportsWhereItLooked(t *testing.T) {
	_, err := Library{Root: "/lib"}.Source("claude", "/repo", ".claude/agents")
	if err == nil {
		t.Fatal("Source() = nil, want an error")
	}
	for _, want := range []string{"/lib/claude", "/repo/.claude/agents"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestSourceRejectsACLIWithoutSubagents(t *testing.T) {
	_, err := Library{Root: "/lib"}.Source("codex", "/repo", "")
	if !errors.Is(err, ErrNoSubagentSupport) {
		t.Errorf("Source() = %v, want ErrNoSubagentSupport", err)
	}
}

func TestIntoCopiesOnlyTopLevelMarkdown(t *testing.T) {
	src := t.TempDir()
	tree(t, src, map[string]string{
		"reviewer.md":      "a",
		"planner.MD":       "b",
		"notes.txt":        "not a definition",
		"nested/deeper.md": "not top level",
	})
	worktree := t.TempDir()

	copied, err := Into(worktree, ".claude/agents", src)
	if err != nil {
		t.Fatalf("Into() = %v", err)
	}
	if len(copied) != 2 {
		t.Errorf("copied %v, want the two markdown files only", copied)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, ".claude/agents/reviewer.md")); err != nil || string(got) != "a" {
		t.Errorf("reviewer.md = %q, %v, want %q", got, err, "a")
	}
	for _, unwanted := range []string{"notes.txt", "nested"} {
		if _, err := os.Stat(filepath.Join(worktree, ".claude/agents", unwanted)); !os.IsNotExist(err) {
			t.Errorf("%s was copied into the worktree, want only top-level markdown", unwanted)
		}
	}
}

func TestIntoRejectsAnEmptySource(t *testing.T) {
	if _, err := Into(t.TempDir(), ".claude/agents", t.TempDir()); err == nil {
		t.Error("Into() with no definitions = nil, want an error")
	}
}

func TestIntoRejectsACLIWithoutSubagents(t *testing.T) {
	if _, err := Into(t.TempDir(), "", t.TempDir()); !errors.Is(err, ErrNoSubagentSupport) {
		t.Errorf("Into() = %v, want ErrNoSubagentSupport", err)
	}
}

func TestIgnoreKeepsSeededFilesOutOfCommits(t *testing.T) {
	worktree := t.TempDir()
	if _, err := Into(worktree, ".claude/agents", seedSource(t)); err != nil {
		t.Fatal(err)
	}
	if err := Ignore(worktree, ".claude/agents"); err != nil {
		t.Fatalf("Ignore() = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(worktree, ".claude/agents/.gitignore"))
	if err != nil {
		t.Fatalf("reading the seeded .gitignore: %v", err)
	}
	// "*" has to cover the .gitignore itself, or the file agent-orc wrote
	// becomes the one change the agent commits.
	if !strings.Contains(string(got), "*") {
		t.Errorf(".gitignore = %q, want it to ignore everything in the directory", got)
	}
}

// seedSource returns a directory holding one definition.
func seedSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	tree(t, src, map[string]string{"reviewer.md": "a"})
	return src
}

func TestIgnoreIsANoOpWithoutASubagentDir(t *testing.T) {
	if err := Ignore(t.TempDir(), ""); err != nil {
		t.Errorf("Ignore() = %v, want nil", err)
	}
}
