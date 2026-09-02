//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// gitEnv isolates git from the developer's own configuration so tests behave
// the same on a laptop and on a CI runner.
var gitEnv = []string{
	"GIT_AUTHOR_NAME=agent-orc test",
	"GIT_AUTHOR_EMAIL=test@example.invalid",
	"GIT_COMMITTER_NAME=agent-orc test",
	"GIT_COMMITTER_EMAIL=test@example.invalid",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_CONFIG_SYSTEM=" + os.DevNull,
}

// git runs a git command in dir and fails the test if it errors.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// initRepo creates a repository with one commit on main and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "--initial-branch=main", ".")
	write(t, filepath.Join(repo, "README.md"), "base\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "--no-gpg-sign", "-m", "chore: base")
	return repo
}

// initRepoWithRemote creates a repository whose origin is a real bare
// repository on disk, so pushes actually go somewhere and can be inspected.
func initRepoWithRemote(t *testing.T) (repo, remote string) {
	t.Helper()
	// The path contains "github.com" so the forge layer picks the gh code
	// path, while every git operation still runs against this local bare
	// repository: no network, no credentials, real pushes.
	remote = filepath.Join(t.TempDir(), "github.com", "org", "repo.git")
	git(t, t.TempDir(), "init", "--bare", "--initial-branch=main", remote)
	repo = initRepo(t)
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-q", "origin", "main")
	return repo, remote
}

// write creates a file, failing the test if it cannot.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// buildBinary compiles agent-orc once per test binary and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agent-orc-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "agent-orc")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/MaryannGitonga/agent-orc/cmd/agent-orc")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildFailure{err: err, output: string(out)}
		}
	})
	if buildErr != nil {
		t.Fatalf("building agent-orc: %v", buildErr)
	}
	return binPath
}

type buildFailure struct {
	err    error
	output string
}

func (b *buildFailure) Error() string { return b.err.Error() + "\n" + b.output }
func (b *buildFailure) Unwrap() error { return b.err }

// stubInto adds another fake CLI to an existing stub directory, so one PATH
// entry can hold every tool a test needs.
func stubInto(t *testing.T, dir, name, receipt, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		"{ printf 'cwd=%s\\n' \"$PWD\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; } >> '" + receipt + "'\n" +
		body + "\n"
	write(t, filepath.Join(dir, name), script)
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("making the stub executable: %v", err)
	}
}

// stubAgent installs a fake agentic CLI on PATH. It records its argv and
// working directory to a receipt file, then runs body inside the worktree.
func stubAgent(t *testing.T, name, receipt, body string) string {
	t.Helper()
	dir := t.TempDir()
	stubInto(t, dir, name, receipt, body)
	return dir
}

// systemPath returns a directory holding symlinks to the named binaries and
// nothing else, so a test can control exactly what is on PATH.
func systemPath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		src, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not installed", name)
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil {
			t.Fatalf("linking %s: %v", name, err)
		}
	}
	return dir
}

// readFile returns a file's contents, or "" if it does not exist yet.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
