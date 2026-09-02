// Package forge pushes a finished branch and opens a draft pull request.
//
// Opening a *draft* is what does the safety work: it makes "agent-produced,
// not yet human-checked" visible in the PR list the moment the branch exists.
// A human still reviews the diff and clicks "Ready for review" — agent-orc has
// no command for that, because it is the human-in-the-loop gate.
package forge

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Kind is the hosting platform a repository's remote points at.
type Kind string

// The platforms agent-orc can open a draft change on.
const (
	// KindGitHub uses the gh CLI.
	KindGitHub Kind = "github"
	// KindGitLab uses the glab CLI.
	KindGitLab Kind = "gitlab"
	// KindUnknown is a remote neither CLI handles.
	KindUnknown Kind = "unknown"
)

// Detect works out which platform a remote URL belongs to.
func Detect(remoteURL string) Kind {
	host := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(host, "github.com") || strings.Contains(host, "github."):
		return KindGitHub
	case strings.Contains(host, "gitlab.com") || strings.Contains(host, "gitlab."):
		return KindGitLab
	default:
		return KindUnknown
	}
}

// Request is everything needed to open a draft change.
type Request struct {
	// Worktree is the checkout the commands run in.
	Worktree string
	// Branch is the branch to push and open the change from.
	Branch string
	// BaseBranch is what the change targets.
	BaseBranch string
	// Title and Body describe the change.
	Title, Body string
	// Remote is the remote to push to, normally "origin".
	Remote string
}

// Runner shells out to git and the platform CLI. It is an interface so the
// push-and-open sequence can be tested without a real remote.
type Runner interface {
	Run(dir string, argv ...string) (string, error)
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

// Run executes argv in dir and returns its combined output.
func (ExecRunner) Run(dir string, argv ...string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is built here, not from user input
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

// Opener pushes a branch and opens a draft change for it.
type Opener struct {
	Runner Runner
}

// New returns an opener that shells out for real.
func New() *Opener { return &Opener{Runner: ExecRunner{}} }

// RemoteBranchExists reports whether the branch is already published.
//
// It is checked before pushing because a branch that is already on the remote
// means the agent pushed it itself, against the instructions it was given —
// which is a trust problem with that CLI, not something to paper over.
func (o *Opener) RemoteBranchExists(worktree, remote, branch string) (bool, error) {
	out, err := o.Runner.Run(worktree, "git", "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return false, fmt.Errorf("checking whether %s/%s already exists: %w: %s", remote, branch, err, out)
	}
	return strings.TrimSpace(out) != "", nil
}

// RemoteURL returns the push URL of a remote.
func (o *Opener) RemoteURL(worktree, remote string) (string, error) {
	out, err := o.Runner.Run(worktree, "git", "remote", "get-url", remote)
	if err != nil {
		return "", fmt.Errorf("reading the URL of remote %q: %w: %s", remote, err, out)
	}
	return strings.TrimSpace(out), nil
}

// Push publishes the branch.
func (o *Opener) Push(r Request) error {
	if out, err := o.Runner.Run(r.Worktree, "git", "push", "-u", r.Remote, r.Branch); err != nil {
		return fmt.Errorf("pushing %s: %w: %s", r.Branch, err, out)
	}
	return nil
}

// OpenDraft opens a draft pull or merge request and returns its URL.
func (o *Opener) OpenDraft(kind Kind, r Request) (string, error) {
	var argv []string
	switch kind {
	case KindGitHub:
		argv = []string{"gh", "pr", "create", "--draft",
			"--base", r.BaseBranch, "--head", r.Branch,
			"--title", r.Title, "--body", r.Body}
	case KindGitLab:
		argv = []string{"glab", "mr", "create", "--draft",
			"--target-branch", r.BaseBranch, "--source-branch", r.Branch,
			"--title", r.Title, "--description", r.Body}
	default:
		return "", fmt.Errorf("no draft-PR support for this remote; push succeeded, open the change by hand")
	}

	out, err := o.Runner.Run(r.Worktree, argv...)
	if err != nil {
		return "", fmt.Errorf("opening the draft change: %w: %s", err, out)
	}
	return lastURL(out), nil
}

// lastURL picks the change's URL out of the CLI's output.
func lastURL(out string) string {
	fields := strings.Fields(out)
	for i := len(fields) - 1; i >= 0; i-- {
		if strings.HasPrefix(fields[i], "http://") || strings.HasPrefix(fields[i], "https://") {
			return fields[i]
		}
	}
	return strings.TrimSpace(out)
}
