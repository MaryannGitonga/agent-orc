package orc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/MaryannGitonga/agent-orc/internal/forge"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/sanitize"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// defaultRemote is the remote a task's branch is published to.
const defaultRemote = "origin"

// ErrNoRemote means the repository has no remote to publish to. That is a
// legitimate way to work (a local-only repository), so it is reported as its
// own condition rather than as a publish failure.
var ErrNoRemote = errors.New("the repository has no " + defaultRemote + " remote to push to")

// Publisher runs the sanitize → push → draft-PR chain for a finished task.
//
// The order matters and is not negotiable: nothing is pushed until the commit
// messages have been rewritten, which is why the agent is told at launch to
// commit locally and stop there.
type Publisher struct {
	layout paths.Layout
	store  *state.Store
	opener *forge.Opener
	self   string
	out    io.Writer
}

// NewPublisher returns a publisher writing progress to out.
func NewPublisher(layout paths.Layout, out io.Writer) (*Publisher, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the agent-orc binary: %w", err)
	}
	return &Publisher{
		layout: layout,
		store:  state.NewStore(layout.State),
		opener: forge.New(),
		self:   self,
		out:    out,
	}, nil
}

// Publish sanitizes the task's commits, pushes the branch and opens a draft
// change for it.
func (p *Publisher) Publish(id string) error {
	record, err := p.store.Load(id)
	if err != nil {
		return err
	}
	if record.Status.HasProcess() {
		return fmt.Errorf("task %q is still %s; wait for it to finish or run 'agent-orc stop %s'",
			id, record.Status, id)
	}
	if _, err := os.Stat(record.Worktree); err != nil {
		return fmt.Errorf("worktree %s is gone; the branch may already have been cleaned up: %w",
			record.Worktree, err)
	}

	remoteURL, err := p.opener.RemoteURL(record.Worktree, defaultRemote)
	if err != nil {
		return ErrNoRemote
	}
	if err := p.checkAgentDidNotPublish(&record); err != nil {
		return err
	}

	commits, err := p.countCommits(record)
	if err != nil {
		return err
	}
	if commits == 0 {
		return fmt.Errorf("task %q has no commits on %s; there is nothing to open a PR for",
			id, record.Branch)
	}

	rewritten, err := p.sanitize(record)
	if err != nil {
		return err
	}
	if rewritten > 0 {
		// Recorded before the push, so the state reflects what happened to
		// the branch even if opening the draft later fails.
		if err := p.store.Update(id, func(k *state.Task) { k.RewrittenCommits = rewritten }); err != nil {
			return err
		}
		fmt.Fprintf(p.out, "%s  rewrote %d commit message(s)\n", id, rewritten)
	}

	req := forge.Request{
		Worktree:   record.Worktree,
		Branch:     record.Branch,
		BaseBranch: record.BaseBranch,
		Remote:     defaultRemote,
		Title:      p.title(record),
		Body:       p.body(record, commits),
	}
	if err := p.opener.Push(req); err != nil {
		return err
	}
	fmt.Fprintf(p.out, "%s  pushed %s\n", id, record.Branch)

	url, err := p.opener.OpenDraft(forge.Detect(remoteURL), req)
	if err != nil {
		return err
	}

	if err := p.store.Update(id, func(k *state.Task) { k.PRURL = url }); err != nil {
		return err
	}
	fmt.Fprintf(p.out, "%s  draft opened %s\n", id, url)
	return nil
}

// checkAgentDidNotPublish flags a task whose agent pushed the branch itself,
// against the instructions it was given.
//
// Some CLIs honour prompt-level instructions no better than they honour their
// own attribution settings. When that happens the branch on the remote has not
// been through sanitization, so it is surfaced as a policy violation rather
// than quietly treated as if agent-orc had published it.
func (p *Publisher) checkAgentDidNotPublish(record *state.Task) error {
	published, err := p.opener.RemoteBranchExists(record.Worktree, defaultRemote, record.Branch)
	if err != nil {
		return err
	}
	if !published {
		return nil
	}
	if uerr := p.store.Update(record.ID, func(k *state.Task) {
		k.Status = state.StatusPolicyViolation
		k.Error = fmt.Sprintf("the agent pushed %s itself; the remote branch has not been sanitized", record.Branch)
	}); uerr != nil {
		return uerr
	}
	return fmt.Errorf("task %q: %s/%s already exists; the agent pushed it despite being told not to, so it never went through sanitization; inspect the branch before doing anything with it",
		record.ID, defaultRemote, record.Branch)
}

// sanitize rewrites the task branch's commit messages.
func (p *Publisher) sanitize(record state.Task) (int, error) {
	patterns, err := sanitize.LoadPatterns(p.layout.TrailerPatternsFile())
	if err != nil {
		return 0, err
	}

	var signOff *sanitize.Signature
	if record.DCOSignoff {
		sig, err := p.identity(record.Worktree)
		if err != nil {
			return 0, err
		}
		signOff = &sig
	}

	policy, err := sanitize.NewPolicy(patterns, signOff)
	if err != nil {
		return 0, err
	}
	return sanitize.Rewriter{
		Worktree: record.Worktree,
		Base:     record.BaseBranch,
		Policy:   policy,
		Patterns: patterns,
		Self:     p.self,
	}.Run()
}

// identity is who a DCO sign-off is written as.
//
// git's configured identity comes first, since that is who is certifying the
// contribution. Where it is unset, because the identity came from the
// environment instead as it does in CI, the author of the branch's newest
// commit is used rather than failing: that is the same person by construction.
func (p *Publisher) identity(worktree string) (sanitize.Signature, error) {
	name := gitConfig(worktree, "user.name")
	email := gitConfig(worktree, "user.email")
	if name != "" && email != "" {
		return sanitize.Signature{Name: name, Email: email}, nil
	}

	author, err := p.opener.Runner.Run(worktree, "git", "log", "-1", "--format=%an <%ae>")
	if err == nil {
		if sig, perr := sanitize.ParseSignature(strings.TrimSpace(author)); perr == nil {
			return sig, nil
		}
	}
	return sanitize.Signature{}, fmt.Errorf("dco_signoff needs git user.name and user.email to be set")
}

// gitConfig reads one config value, returning "" when it is not set.
func gitConfig(worktree, key string) string {
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// countCommits returns how many commits the branch has ahead of its base.
func (p *Publisher) countCommits(record state.Task) (int, error) {
	out, err := p.opener.Runner.Run(record.Worktree, "git", "rev-list", "--count",
		record.BaseBranch+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("counting commits on %s: %w: %s", record.Branch, err, out)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("counting commits on %s: unexpected output %q", record.Branch, out)
	}
	return n, nil
}

// title names the change after the task and its newest commit.
func (p *Publisher) title(record state.Task) string {
	subject, err := p.opener.Runner.Run(record.Worktree, "git", "log", "-1", "--format=%s")
	if err != nil || strings.TrimSpace(subject) == "" {
		return record.ID
	}
	subject = strings.TrimSpace(subject)
	if strings.Contains(subject, record.ID) {
		return subject
	}
	return record.ID + ": " + subject
}

// body describes the change briefly and says plainly where it came from.
func (p *Publisher) body(record state.Task, commits int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produced by `agent-orc` for task `%s` (%s", record.ID, record.CLI)
	if record.Model != "" {
		fmt.Fprintf(&b, ", %s", record.Model)
	}
	fmt.Fprintf(&b, "), %d commit(s) on `%s`.\n\n", commits, record.BaseBranch)
	if task := strings.TrimSpace(record.Source); task != "" {
		fmt.Fprintf(&b, "Source: %s\n\n", task)
	}
	b.WriteString("Opened as a draft: not yet human-checked. Review the diff before marking it ready.\n")
	return b.String()
}
