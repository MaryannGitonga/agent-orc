package sanitize

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Environment variables carrying the policy to the per-commit pass. The rebase
// runs one child process per commit, and this is how it inherits the rules.
const (
	// EnvPatterns holds the strip patterns, one per line.
	EnvPatterns = "AGENT_ORC_STRIP_PATTERNS"
	// EnvSignOff holds the DCO identity, or is empty for no sign-off.
	EnvSignOff = "AGENT_ORC_SIGNOFF"
)

// Rewriter applies a policy to every commit a branch has ahead of its base.
type Rewriter struct {
	// Worktree is where the branch is checked out.
	Worktree string
	// Base is the branch the task was cut from; commits up to it are left
	// alone. Nothing outside Base..HEAD is ever touched.
	Base string
	// Policy is what to strip and what to add.
	Policy Policy
	// Patterns is the policy's pattern list, passed to the per-commit pass.
	Patterns []string
	// Self is the agent-orc binary the rebase execs once per commit.
	Self string
}

// Run rewrites the branch and reports how many commits changed.
//
// It first checks whether anything needs changing at all: a branch whose
// commits are already clean is left with its SHAs intact rather than rewritten
// for nothing. If the rewrite cannot complete, the rebase is aborted so the
// branch is left exactly as the agent produced it, because a half-rewritten
// branch must never be pushed.
func (r Rewriter) Run() (int, error) {
	messages, err := r.messages()
	if err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		return 0, nil
	}

	needed := 0
	for _, m := range messages {
		if _, changed := r.Policy.Clean(m); changed {
			needed++
		}
	}
	if needed == 0 {
		return 0, nil
	}

	// --rebase-merges because this pass rewrites commit messages, not history
	// shape: a plain rebase would flatten any merge the branch contains and
	// silently change the topology it was asked only to clean up.
	cmd := exec.Command("git", "rebase", "--force-rebase", "--rebase-merges",
		"--exec", quoteForExec(r.Self)+" sanitize-commit", r.Base)
	cmd.Dir = r.Worktree
	cmd.Env = append(os.Environ(),
		EnvPatterns+"="+strings.Join(r.Patterns, "\n"),
		EnvSignOff+"="+r.signOff(),
		// A rebase must not stop to ask anything; there is no terminal here.
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	if err := cmd.Run(); err != nil {
		abort := exec.Command("git", "rebase", "--abort")
		abort.Dir = r.Worktree
		_ = abort.Run()
		return 0, fmt.Errorf("rewriting commit messages on the task branch: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}
	return needed, nil
}

// signOff renders the DCO identity for the child processes.
func (r Rewriter) signOff() string {
	if r.Policy.SignOff == nil {
		return ""
	}
	return r.Policy.SignOff.String()
}

// messages returns the message of every commit the branch has ahead of base.
func (r Rewriter) messages() ([]string, error) {
	// Merges included deliberately. The rewrite is --rebase-merges, so a merge
	// commit's message goes through the per-commit pass like any other; if the
	// pre-check skipped merges, a trailer living only on a merge message would
	// leave needed at zero, skip the rewrite entirely, and reach the remote
	// unsanitized.
	cmd := exec.Command("git", "log", "--reverse", "--format=%B%x00", r.Base+"..HEAD")
	cmd.Dir = r.Worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing commits on the task branch: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	var out []string
	for _, part := range strings.Split(stdout.String(), "\x00") {
		if strings.TrimSpace(part) != "" {
			out = append(out, strings.TrimLeft(part, "\n"))
		}
	}
	return out, nil
}

// quoteForExec makes a path safe inside the shell command git runs for --exec.
func quoteForExec(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// AmendHead applies the policy to the commit currently at HEAD. It is what the
// rebase execs once per commit, and it is a no-op when the message is already
// compliant.
func AmendHead(worktree string) error {
	policy, err := policyFromEnv()
	if err != nil {
		return err
	}

	current, err := headMessage(worktree)
	if err != nil {
		return err
	}
	cleaned, changed := policy.Clean(current)
	if !changed {
		return nil
	}

	cmd := exec.Command("git", "commit", "--amend", "--file=-", "--no-edit", "--allow-empty")
	cmd.Dir = worktree
	cmd.Stdin = strings.NewReader(cleaned)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amending the commit message: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// policyFromEnv rebuilds the policy the parent rebase configured.
func policyFromEnv() (Policy, error) {
	var patterns []string
	for _, line := range strings.Split(os.Getenv(EnvPatterns), "\n") {
		if strings.TrimSpace(line) != "" {
			patterns = append(patterns, line)
		}
	}
	var signOff *Signature
	if raw := strings.TrimSpace(os.Getenv(EnvSignOff)); raw != "" {
		sig, err := ParseSignature(raw)
		if err != nil {
			return Policy{}, err
		}
		signOff = &sig
	}
	return NewPolicy(patterns, signOff)
}

// ParseSignature reads a "Name <email>" identity.
func ParseSignature(raw string) (Signature, error) {
	open := strings.LastIndex(raw, "<")
	closing := strings.LastIndex(raw, ">")
	if open == -1 || closing < open {
		return Signature{}, fmt.Errorf("identity %q is not in 'Name <email>' form", raw)
	}
	name := strings.TrimSpace(raw[:open])
	email := strings.TrimSpace(raw[open+1 : closing])
	if name == "" || email == "" {
		return Signature{}, fmt.Errorf("identity %q is missing a name or an email", raw)
	}
	return Signature{Name: name, Email: email}, nil
}

// headMessage returns the full message of the commit at HEAD.
func headMessage(worktree string) (string, error) {
	cmd := exec.Command("git", "log", "-1", "--format=%B")
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("reading the commit message: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
