package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHonoursTheEnvironmentOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvHome, root)

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if got.Root != root {
		t.Errorf("Root = %q, want %q", got.Root, root)
	}
	if want := filepath.Join(root, "state"); got.State != want {
		t.Errorf("State = %q, want %q", got.State, want)
	}
}

func TestResolveDefaultsToTheHomeDirectory(t *testing.T) {
	t.Setenv(EnvHome, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if want := filepath.Join(home, ".agent-orc"); got.Root != want {
		t.Errorf("Root = %q, want %q", got.Root, want)
	}
}

func TestResolveMakesRelativeOverridesAbsolute(t *testing.T) {
	t.Setenv(EnvHome, "relative-root")
	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if !filepath.IsAbs(got.Root) {
		t.Errorf("Root = %q, want an absolute path", got.Root)
	}
}

func TestEnsureCreatesEveryDirectory(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "orc"))
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	for _, dir := range []string{l.Root, l.State, l.Logs, l.Worktrees} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("Stat(%q) = %v, want the directory to exist", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

func TestPerTaskPaths(t *testing.T) {
	l := New("/root")
	tests := map[string]struct{ got, want string }{
		"state":      {l.StateFile("PROJ-1"), "/root/state/PROJ-1.json"},
		"log":        {l.LogFile("PROJ-1"), "/root/logs/PROJ-1.log"},
		"supervisor": {l.SupervisorLogFile("PROJ-1"), "/root/logs/PROJ-1.supervisor.log"},
		"worktree":   {l.Worktree("PROJ-1"), "/root/worktrees/PROJ-1"},
	}
	for name, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s path = %q, want %q", name, tc.got, tc.want)
		}
	}
}
