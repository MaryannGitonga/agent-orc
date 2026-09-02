// Package source turns a task's `source` field into the prompt an agent sees.
//
// A JIRA or GitHub reference is fetched once, at launch, and combined with the
// task's own prompt. It is deliberately a one-time read: nothing here polls,
// and nothing writes back to the ticket.
package source

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind is the sort of reference a source string holds.
type Kind string

// The kinds of source reference agent-orc understands.
const (
	// KindText is free text used verbatim; it is not fetched.
	KindText Kind = "text"
	// KindGitHub is a GitHub issue, `github://owner/repo#number`.
	KindGitHub Kind = "github"
	// KindJIRA is a JIRA issue, `jira://TICKET-ID`.
	KindJIRA Kind = "jira"
)

// Ref is a parsed source reference.
type Ref struct {
	Kind Kind
	// Raw is the original source string.
	Raw string
	// Owner and Repo are set for [KindGitHub].
	Owner, Repo string
	// Number is the issue number, set for [KindGitHub].
	Number int
	// Key is the ticket key, set for [KindJIRA].
	Key string
}

var (
	githubRef = regexp.MustCompile(`^github://([^/\s]+)/([^/\s#]+)#(\d+)$`)
	jiraRef   = regexp.MustCompile(`^jira://([A-Za-z][A-Za-z0-9_]*-\d+)$`)
)

// Parse classifies a source string. Anything without a recognised scheme is
// free text, which is used as-is rather than treated as an error. A task can
// legitimately describe its own work without a ticket behind it.
func Parse(raw string) (Ref, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Ref{}, fmt.Errorf("source is empty")
	}

	if m := githubRef.FindStringSubmatch(trimmed); m != nil {
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			return Ref{}, fmt.Errorf("invalid github issue number in %q", raw)
		}
		return Ref{Kind: KindGitHub, Raw: trimmed, Owner: m[1], Repo: m[2], Number: n}, nil
	}
	if m := jiraRef.FindStringSubmatch(trimmed); m != nil {
		return Ref{Kind: KindJIRA, Raw: trimmed, Key: strings.ToUpper(m[1])}, nil
	}

	// A malformed scheme is a mistake, not free text: silently treating
	// "github://org/repo" as a prompt would launch an agent on nonsense.
	if i := strings.Index(trimmed, "://"); i > 0 && !strings.ContainsAny(trimmed[:i], " \t\n") {
		switch scheme := trimmed[:i]; scheme {
		case "github":
			return Ref{}, fmt.Errorf("malformed github source %q, want github://owner/repo#number", raw)
		case "jira":
			return Ref{}, fmt.Errorf("malformed jira source %q, want jira://TICKET-123", raw)
		default:
			return Ref{}, fmt.Errorf("unknown source scheme %q in %q, want github:// or jira://", scheme, raw)
		}
	}
	return Ref{Kind: KindText, Raw: trimmed}, nil
}

// Issue is the part of a ticket that becomes a prompt.
type Issue struct {
	Title string
	Body  string
}

// Text renders an issue as the base prompt.
func (i Issue) Text() string {
	title := strings.TrimSpace(i.Title)
	body := strings.TrimSpace(i.Body)
	switch {
	case title == "":
		return body
	case body == "":
		return title
	default:
		return title + "\n\n" + body
	}
}

// Compose layers a task's own prompt on top of the fetched ticket text. Both
// are kept: the ticket says what the work is, the prompt narrows or adds to
// it, and neither has to repeat the other.
func Compose(fetched, prompt string) string {
	fetched = strings.TrimSpace(fetched)
	prompt = strings.TrimSpace(prompt)
	switch {
	case fetched == "":
		return prompt
	case prompt == "":
		return fetched
	default:
		return fetched + "\n\nAdditional instructions:\n" + prompt
	}
}
