// Package testcmd works out how a repository runs its own tests.
//
// The point is that nobody should have to tell agent-orc something the
// repository already says. A project states how it is tested in its own build
// files, and the handful of conventions below cover most of them; a project
// that does something unusual sets the command explicitly and this package
// stays out of the way.
package testcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// None is the configured value that turns verification off for a repository
// whose tests agent-orc should not be running: too slow, too flaky, or needing
// something the worktree does not have.
const None = "none"

// candidate is one convention: a marker in the repository and the command that
// marker implies.
type candidate struct {
	// name is what the match is called in the log line, so a discovered
	// command can be traced back to the reason it was picked.
	name    string
	command string
	// matches reports whether this convention applies to the repository.
	matches func(dir string) bool
}

// candidates are tried in order. The Makefile comes first on purpose: a
// repository that wrote a test target has already decided how its tests are
// run, and that decision beats anything inferred from the language underneath.
var candidates = []candidate{
	{"a make test target", "make test", hasMakeTarget},
	{"go.mod", "go test ./...", hasFile("go.mod")},
	{"a package.json test script", "npm test", hasNPMTestScript},
	{"Cargo.toml", "cargo test", hasFile("Cargo.toml")},
	{"a pytest layout", "python -m pytest -q", hasPytest},
}

// Discover returns the command that runs dir's tests, and the marker it was
// inferred from. Both are empty when nothing matched, which is not a failure:
// plenty of repositories have no test suite to run.
func Discover(dir string) (command, reason string) {
	for _, c := range candidates {
		if c.matches(dir) {
			return c.command, c.name
		}
	}
	return "", ""
}

// makeTarget matches a `test:` rule and nothing else. The anchor matters: a
// Makefile with test-unit and test-integration but no plain test target has no
// entry point to call, and `make test` there fails on a missing rule.
var makeTarget = regexp.MustCompile(`(?m)^test[ \t]*:`)

func hasMakeTarget(dir string) bool {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && makeTarget.Match(data) {
			return true
		}
	}
	return false
}

// hasFile reports whether a named file exists in the repository root.
func hasFile(name string) func(string) bool {
	return func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
}

// hasNPMTestScript looks for a real test script. package.json alone is not
// enough: npm test on a package without one exits non-zero, which would fail
// every task in a front-end repository that happens not to have tests.
func hasNPMTestScript(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	return pkg.Scripts["test"] != ""
}

// pytestConfig are the files that can configure pytest, with the section that
// says one of them actually does. Only pytest.ini is pytest's alone; the rest
// belong to packaging and tooling first, and a project can easily have them
// without pytest anywhere in sight.
var pytestConfig = []struct{ file, section string }{
	{"pytest.ini", ""},
	{"pyproject.toml", "[tool.pytest"},
	{"setup.cfg", "[tool:pytest]"},
	{"tox.ini", "[pytest]"},
}

// hasPytest looks for pytest's own configuration, and failing that for test
// files named the way pytest collects them. The second check is what catches a
// small project with tests and no packaging at all.
//
// The configuration check reads the file rather than trusting its name. pytest
// is the one convention here that fails a project for having no tests: it exits
// 5 when it collects nothing, where go test and cargo test both exit 0. So
// taking a pyproject.toml written for Poetry or Ruff as proof of pytest would
// not merely guess wrong, it would fail every task in that repository and
// publish none of them.
func hasPytest(dir string) bool {
	for _, c := range pytestConfig {
		data, err := os.ReadFile(filepath.Join(dir, c.file))
		if err != nil {
			continue
		}
		if c.section == "" || strings.Contains(string(data), c.section) {
			return true
		}
	}
	for _, pattern := range []string{"test_*.py", "*_test.py", "tests/test_*.py"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, pattern)); len(matches) > 0 {
			return true
		}
	}
	return false
}
