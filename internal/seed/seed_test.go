package seed

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestSourceSkipsALibraryWithoutMarkdown(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	// The library directory exists, but nothing in it is a top-level definition.
	tree(t, root, map[string]string{
		"claude/notes.txt":        "not a definition",
		"claude/nested/deeper.md": "not top level",
	})
	tree(t, repo, map[string]string{".claude/agents/reviewer.md": "repo"})

	got, err := Library{Root: root}.Source("claude", repo, ".claude/agents")
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

func TestIntoReportsAMissingSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing")
	_, err := Into(t.TempDir(), ".claude/agents", src)
	if err == nil || !strings.Contains(err.Error(), src) {
		t.Errorf("Into() = %v, want an error naming %q", err, src)
	}
}

func TestIntoReportsAWorktreeItCannotWriteTo(t *testing.T) {
	// A file where the worktree should be: the subagent directory cannot be
	// created inside it.
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.WriteFile(worktree, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Into(worktree, ".claude/agents", seedSource(t)); err == nil {
		t.Error("Into() into a file = nil, want an error")
	}
}

func TestIntoReturnsWhatItCopiedBeforeAFailure(t *testing.T) {
	src := seedSource(t)
	// Read in name order, so reviewer.md is copied before this one fails: a
	// definition name that is really a directory is not a file to hand the CLI.
	if err := os.Symlink(t.TempDir(), filepath.Join(src, "zz-linked.md")); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()

	copied, err := Into(worktree, ".claude/agents", src)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Into() = %v, want a not-a-regular-file error", err)
	}
	if len(copied) != 1 || copied[0] != "reviewer.md" {
		t.Errorf("copied = %v, want [reviewer.md], the files written before the failure", copied)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".claude/agents/reviewer.md")); err != nil {
		t.Errorf("reviewer.md is missing from the worktree: %v", err)
	}
}

func TestIntoRefusesANamedPipeWithoutBlocking(t *testing.T) {
	src := t.TempDir()
	// Opening a pipe for reading waits for a writer that never comes, so a
	// repository that ships one as a definition could hang the dispatch.
	if err := syscall.Mkfifo(filepath.Join(src, "pipe.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Into(t.TempDir(), ".claude/agents", src)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Into() = %v, want a not-a-regular-file error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Into() is still blocked on the named pipe after 5s")
	}
}

func TestIntoRefusesToReplaceADirectory(t *testing.T) {
	worktree := t.TempDir()
	// The worktree already has a directory where a definition would go.
	if err := os.MkdirAll(filepath.Join(worktree, ".claude/agents/reviewer.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Into(worktree, ".claude/agents", seedSource(t)); err == nil {
		t.Error("Into() over a directory = nil, want an error")
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

func TestIgnoreReportsAMissingDirectory(t *testing.T) {
	// Ignore runs after Into; with nothing seeded there is nowhere to write.
	worktree := t.TempDir()
	err := Ignore(worktree, ".claude/agents")
	if want := filepath.Join(worktree, ".claude/agents/.gitignore"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Ignore() = %v, want an error naming %q", err, want)
	}
}

func TestCopyFileReportsAMissingSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "gone.md")
	err := copyFile(src, filepath.Join(t.TempDir(), "gone.md"))
	if err == nil || !strings.Contains(err.Error(), src) {
		t.Errorf("copyFile() = %v, want an error naming %q", err, src)
	}
}

func TestIgnoreIsANoOpWithoutASubagentDir(t *testing.T) {
	if err := Ignore(t.TempDir(), ""); err != nil {
		t.Errorf("Ignore() = %v, want nil", err)
	}
}
