// Package seed copies subagent definitions into a task's worktree before the
// agent launches.
//
// agent-orc deliberately invents no abstraction over "what a subagent is". It
// copies the files each CLI already knows how to read, into the directory that
// CLI already looks in. Turning subagents off means not copying them: absence
// is the off state, so there is no flag to pass the CLI itself.
package seed

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoSubagentSupport means the CLI has no subagent directory to seed.
var ErrNoSubagentSupport = errors.New("this cli has no subagent mechanism to seed")

// Library is where a user keeps subagent definitions, one directory per CLI:
//
//	~/.agent-orc/agents/claude/*.md
//	~/.agent-orc/agents/copilot/*.md
//
// A repository may also carry its own definitions in the directory the CLI
// natively reads; those are used when the library has nothing for that CLI, so
// a repo that already ships subagents works without any setup.
type Library struct {
	// Root is the agents library directory.
	Root string
}

// Source returns the directory holding definitions for a CLI, preferring the
// user's library and falling back to what the repository already ships.
func (l Library) Source(cli, repo, subagentDir string) (string, error) {
	if subagentDir == "" {
		return "", ErrNoSubagentSupport
	}
	candidates := []string{
		filepath.Join(l.Root, cli),
		filepath.Join(repo, subagentDir),
	}
	for _, dir := range candidates {
		if hasDefinitions(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no subagent definitions found in %s", strings.Join(candidates, " or "))
}

// hasDefinitions reports whether dir holds at least one .md definition.
func hasDefinitions(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			return true
		}
	}
	return false
}

// Into copies every .md definition from src into worktree/subagentDir and
// returns the file names it copied.
//
// Only the top level is copied, and only markdown: the point is to hand the
// CLI its definitions, not to mirror an arbitrary directory into a checkout
// the agent is about to commit from.
func Into(worktree, subagentDir, src string) ([]string, error) {
	if subagentDir == "" {
		return nil, ErrNoSubagentSupport
	}
	dst := filepath.Join(worktree, subagentDir)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", src, err)
	}
	var copied []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return copied, err
		}
		copied = append(copied, e.Name())
	}
	if len(copied) == 0 {
		return nil, fmt.Errorf("no subagent definitions (*.md) in %s", src)
	}
	return copied, nil
}

// copyFile copies one file, preserving nothing but its contents.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copying %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	return nil
}

// Ignore keeps the seeded definitions out of the agent's commits by writing a
// .gitignore into the seeded directory itself.
//
// The obvious alternative, the repository's info/exclude, is not usable
// here: git reads that from the *common* git directory, which every worktree
// and the user's own checkout share, so a per-task exclusion would leak out of
// the task. A .gitignore inside the seeded directory stays entirely within the
// disposable worktree, and its "*" pattern covers the file itself.
func Ignore(worktree, subagentDir string) error {
	if subagentDir == "" {
		return nil
	}
	path := filepath.Join(worktree, subagentDir, ".gitignore")
	// This covers untracked files, which is what a seeded definition normally
	// is. It cannot hide one that overwrote a file the repository already
	// tracks: git ignores .gitignore for tracked paths. The dispatcher warns
	// when that happens rather than letting it pass silently.
	body := "# Seeded by agent-orc for this task; not part of the branch.\n*\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
