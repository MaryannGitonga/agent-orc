package orc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/bounded"
	"github.com/MaryannGitonga/agent-orc/internal/config"
	"github.com/MaryannGitonga/agent-orc/internal/forge"
	"github.com/MaryannGitonga/agent-orc/internal/gitx"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// Level is how much a check's result matters.
type Level string

// A fail refuses the task; a warning is shown and the task still runs.
const (
	LevelOK   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// Check is the result of one probe of the environment a task needs.
type Check struct {
	Name   string
	Level  Level
	Detail string
}

// probeLimit bounds each probe: a check that hangs must not hold up a dispatch.
const probeLimit = 30 * time.Second

// checker runs probes, remembering each result so a batch asks once per CLI
// and per repository rather than once per task.
type checker struct {
	seen map[string][]Check
}

func (c *checker) all(key string, probe func() []Check) []Check {
	if c.seen == nil {
		c.seen = map[string][]Check{}
	}
	if got, ok := c.seen[key]; ok {
		return got
	}
	got := probe()
	c.seen[key] = got
	return got
}

func (c *checker) once(key string, probe func() Check) Check {
	return c.all(key, func() []Check { return []Check{probe()} })[0]
}

// environment checks what t needs from this machine: its CLIs installed and
// logged in, a forge it can open a PR on, and a test command that can run.
// t.Repo must already be the repository's root.
func (c *checker) environment(t task.Task) []Check {
	checks := c.cli(t.CLI)
	// An automatic review spends money unattended, so the CLI that will run it,
	// named or chosen by default, is checked too.
	if reviewer := t.ReviewerCLI(); t.Review.Enabled && t.Review.Auto && reviewer != t.CLI {
		checks = append(checks, c.cli(reviewer)...)
	}
	return append(checks, c.repo(t)...)
}

// cli checks one CLI is installed and, if so, logged in.
func (c *checker) cli(cli task.CLI) []Check {
	bin := c.once("binary:"+string(cli), func() Check { return checkBinary(cli) })
	if bin.Level == LevelFail {
		return []Check{bin}
	}
	checks := []Check{bin, c.once("login:"+string(cli), func() Check { return checkLogin(cli) })}
	if cli == task.CLIClaude {
		if v := c.once("version:claude", checkClaudeVersion); v.Level != LevelOK {
			checks = append(checks, v)
		}
	}
	return checks
}

// claudeCumulative is the first Claude Code whose resumed runs report the whole
// session's cost, which is how a task's spend is added up.
var claudeCumulative = [3]int{2, 1, 277}

// checkClaudeVersion warns about a Claude Code whose resumed runs report only
// their own cost, so a task's spend would be under-counted. A version that
// cannot be read is not judged.
func checkClaudeVersion() Check {
	name := "claude version"
	out, err := probe("claude", "--version")
	var got [3]int
	if err != nil {
		return Check{name, LevelOK, "unknown"}
	}
	if _, err := fmt.Sscanf(firstLine(out), "%d.%d.%d", &got[0], &got[1], &got[2]); err != nil {
		return Check{name, LevelOK, "unknown"}
	}
	for i := range got {
		if got[i] != claudeCumulative[i] {
			if got[i] < claudeCumulative[i] {
				return Check{name, LevelWarn, fmt.Sprintf("%d.%d.%d reports each resumed run alone, so spend is under-counted; upgrade to 2.1.277 or later", got[0], got[1], got[2])}
			}
			break
		}
	}
	return Check{name, LevelOK, firstLine(out)}
}

// repo checks what t needs from its repository: a forge, an identity, tests.
func (c *checker) repo(t task.Task) []Check {
	var checks []Check
	if t.AutoPR {
		checks = append(checks, c.all("forge:"+t.Repo, func() []Check { return checkForge(t.Repo) })...)
	}
	if t.DCOSignoff {
		checks = append(checks, c.once("identity:"+t.Repo, func() Check { return checkIdentity(t.Repo) }))
	}
	if t.TestCommand != "" {
		checks = append(checks, checkTestCommand(t.Repo, t.TestCommand))
	}
	return checks
}

func checkBinary(cli task.CLI) Check {
	name := string(cli) + " installed"
	bin, err := agentBinary(task.Task{CLI: cli})
	if err != nil {
		return Check{name, LevelFail, err.Error()}
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Check{name, LevelFail, fmt.Sprintf("%q is not on PATH", bin)}
	}
	return Check{name, LevelOK, path}
}

// checkLogin asks each CLI whether it is logged in, in the way it documents.
func checkLogin(cli task.CLI) Check {
	name := string(cli) + " logged in"
	switch cli {
	case task.CLIClaude:
		out, err := probe("claude", "auth", "status", "--json")
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		switch {
		case err == nil && json.Unmarshal([]byte(out), &status) == nil && status.LoggedIn:
			return Check{name, LevelOK, "claude auth status"}
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			return Check{name, LevelOK, "ANTHROPIC_API_KEY is set"}
		}
		return Check{name, LevelFail, "not logged in; run 'claude auth login'"}
	case task.CLICodex:
		// `codex login status` documents neither its output nor its exit codes,
		// so a failed run or a plain "not logged in" is taken as no.
		out, err := probe("codex", "login", "status")
		if err != nil || strings.Contains(strings.ToLower(out), "not logged in") {
			return Check{name, LevelFail, "not logged in; run 'codex login'"}
		}
		return Check{name, LevelOK, firstLine(out)}
	case task.CLICopilot:
		// Copilot documents no way to check without starting a session, so its
		// credentials can only be looked for, never confirmed.
		for _, v := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
			if os.Getenv(v) != "" {
				return Check{name, LevelOK, v + " is set"}
			}
		}
		return Check{name, LevelWarn, "cannot be checked; it may use a stored login or gh's"}
	}
	return Check{name, LevelWarn, "no way to check this CLI's login"}
}

// checkForge checks the remote a PR would go to: reachable, and its CLI logged in.
func checkForge(repo string) []Check {
	opener := forge.New()
	has, err := opener.HasRemote(repo, defaultRemote)
	if err != nil {
		return []Check{{"remote", LevelFail, err.Error()}}
	}
	if !has {
		return []Check{{"remote", LevelOK, "no origin; work stays on its branch"}}
	}
	checks := []Check{{"origin reachable", LevelOK, ""}}
	if out, err := probeIn(repo, "git", "ls-remote", defaultRemote, "HEAD"); err != nil {
		checks[0] = Check{"origin reachable", LevelFail, firstLine(out + " " + err.Error())}
	}
	url, err := opener.RemoteURL(repo, defaultRemote)
	if err != nil {
		return append(checks, Check{"forge", LevelFail, err.Error()})
	}
	var bin string
	switch forge.Detect(url) {
	case forge.KindGitHub:
		bin = "gh"
	case forge.KindGitLab:
		bin = "glab"
	default:
		return append(checks, Check{"forge", LevelWarn, "origin is not GitHub or GitLab; the draft PR is opened by hand"})
	}
	if _, err := exec.LookPath(bin); err != nil {
		return append(checks, Check{bin + " installed", LevelFail, fmt.Sprintf("%q is not on PATH", bin)})
	}
	if out, err := probe(bin, "auth", "status"); err != nil {
		return append(checks, Check{bin + " logged in", LevelFail, firstLine(out + " " + err.Error())})
	}
	return append(checks, Check{bin + " logged in", LevelOK, bin + " auth status"})
}

// checkIdentity warns when no git identity is configured for sign-offs. It is
// not fatal: publishing falls back to the branch's newest author.
func checkIdentity(repo string) Check {
	if gitConfig(repo, "user.name") == "" || gitConfig(repo, "user.email") == "" {
		return Check{"git identity", LevelWarn, "user.name or user.email unset; sign-offs use the newest commit's author"}
	}
	return Check{"git identity", LevelOK, gitConfig(repo, "user.email")}
}

// checkTestCommand checks the test command's first program exists, without
// running it: a missing one would fail every fix round the agent is paid for.
// It asks the shell, in the repository the command runs in, so builtins such
// as cd and relative paths such as ./test.sh count.
func checkTestCommand(repo, command string) Check {
	name := "test command"
	fields := strings.Fields(command)
	// Leading VAR=value assignments are not the program, but one setting PATH
	// changes where it is found, which this lookup would not see.
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		if strings.HasPrefix(fields[0], "PATH=") {
			return Check{name, LevelOK, command + " (not checked: sets its own PATH)"}
		}
		fields = fields[1:]
	}
	if len(fields) == 0 || strings.ContainsAny(fields[0], "()$`{};&|<>'\"\\") {
		return Check{name, LevelOK, command + " (not checked: starts with shell syntax)"}
	}
	if _, err := probeIn(repo, "sh", "-c", `command -v -- "$1"`, "sh", fields[0]); err != nil {
		return Check{name, LevelFail, fmt.Sprintf("%q needs %q, which this shell cannot find", command, fields[0])}
	}
	return Check{name, LevelOK, command}
}

func probe(name string, args ...string) (string, error) { return probeIn("", name, args...) }

func probeIn(dir, name string, args ...string) (string, error) {
	cmd := bounded.Within(probeLimit, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// failures returns the checks that refuse a task, as one error, or nil.
func failures(id string, checks []Check) error {
	var lines []string
	for _, c := range checks {
		if c.Level == LevelFail {
			lines = append(lines, fmt.Sprintf("  %s: %s", c.Name, c.Detail))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("task %q cannot run here yet:\n%s\nrun 'agent-orc doctor' for the full picture", id, strings.Join(lines, "\n"))
}

// PrintChecks writes one line per check, for doctor.
func PrintChecks(out io.Writer, checks []Check) {
	marks := map[Level]string{LevelOK: "ok  ", LevelWarn: "warn", LevelFail: "FAIL"}
	for _, c := range checks {
		line := fmt.Sprintf("%s  %s", marks[c.Level], c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(out, line)
	}
}

// Doctor runs every check a dispatch from repo would get, with the settings it
// would inherit, prints them all, and fails if any would refuse a task. With no
// CLI named, it checks each one that is installed.
func Doctor(out io.Writer, repo string, s config.Settings, cli task.CLI) error {
	t := task.Task{
		ID:          "doctor",
		CLI:         cli,
		AutoPR:      s.AutoPR == nil || *s.AutoPR,
		DCOSignoff:  s.DCOSignoff != nil && *s.DCOSignoff,
		TestCommand: s.TestCommand,
	}
	if t.CLI == "" {
		t.CLI = s.CLI
	}
	if r := s.Review; r != nil {
		t.Review = task.Review{
			Enabled: r.Enabled != nil && *r.Enabled,
			Auto:    r.Auto != nil && *r.Auto,
			CLI:     r.CLI,
		}.Normalize()
	}
	if r, err := gitx.Open(repo); err == nil {
		t.Repo = r.Dir
		t, _ = resolveTestCommand(t)
	} else {
		fmt.Fprintf(out, "note  %s is not a git repository; skipping its checks\n", repo)
		t.AutoPR, t.DCOSignoff, t.TestCommand = false, false, ""
	}

	var c checker
	var checks []Check
	if t.CLI != "" {
		checks = c.environment(t)
	} else {
		// No CLI chosen yet: report on each installed one. A named automatic
		// reviewer is required, so it alone stays a failure when missing.
		required := t.Review.Enabled && t.Review.Auto && t.Review.CLI != ""
		installed := 0
		for _, known := range task.KnownCLIs {
			got := c.cli(known)
			if got[0].Level == LevelFail {
				if !required || known != t.Review.CLI {
					got[0] = Check{got[0].Name, LevelWarn, "not installed"}
				}
				checks = append(checks, got[0])
				continue
			}
			installed++
			checks = append(checks, got...)
		}
		if installed == 0 {
			checks = append(checks, Check{"agent CLI", LevelFail, fmt.Sprintf("none of %v is installed", task.KnownCLIs)})
		}
		checks = append(checks, c.repo(t)...)
	}
	PrintChecks(out, checks)
	failed := 0
	for _, ch := range checks {
		if ch.Level == LevelFail {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}
