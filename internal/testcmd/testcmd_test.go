package testcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscover(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{{
		name:  "a make test target wins over the language underneath",
		files: map[string]string{"Makefile": "test: test-unit\n\t@true\n", "go.mod": "module x\n"},
		want:  "make test",
	}, {
		// The anchor matters: these are real targets in this repository, and
		// `make test` against them fails on a missing rule.
		name:  "test-unit is not a test target",
		files: map[string]string{"Makefile": "test-unit:\n\t@true\ntest-integration:\n\t@true\n"},
		want:  "",
	}, {
		name:  "a go module",
		files: map[string]string{"go.mod": "module example.com/x\n"},
		want:  "go test ./...",
	}, {
		name:  "a package.json with a test script",
		files: map[string]string{"package.json": `{"scripts":{"test":"jest"}}`},
		want:  "npm test",
	}, {
		// npm test on a package without one exits non-zero, which would fail
		// every task in a repository that simply has no tests.
		name:  "a package.json without one is not a match",
		files: map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`},
		want:  "",
	}, {
		name:  "pytest configuration",
		files: map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"},
		want:  "python -m pytest -q",
	}, {
		name:  "a bare pytest file, which is all a small project has",
		files: map[string]string{"greeter.py": "", "test_greeter.py": ""},
		want:  "python -m pytest -q",
	}, {
		name:  "tests under a tests directory",
		files: map[string]string{"tests/test_thing.py": ""},
		want:  "python -m pytest -q",
	}, {
		name:  "a cargo crate",
		files: map[string]string{"Cargo.toml": "[package]\n"},
		want:  "cargo test",
	}, {
		// Not a failure: plenty of repositories have nothing to run.
		name:  "a repository with no tests",
		files: map[string]string{"README.md": "hello\n"},
		want:  "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, reason := Discover(dir)
			if got != tt.want {
				t.Errorf("Discover() = %q, want %q", got, tt.want)
			}
			if (reason != "") != (got != "") {
				t.Errorf("reason = %q for command %q; they go together", reason, got)
			}
		})
	}
}
