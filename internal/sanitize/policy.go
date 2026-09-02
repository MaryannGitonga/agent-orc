// Package sanitize rewrites the commit messages on a task's branch before
// anything is pushed.
//
// It does two jobs in one pass: it strips trailers that should not be there
// (AI attribution), and adds the one that must be (DCO sign-off). Doing it
// here rather than trusting each CLI's own attribution setting is deliberate —
// those settings are inconsistently honoured, and an agent crafting a raw
// `git commit` can bypass them entirely. This pass cannot be bypassed because
// it runs after the agent has finished and before the branch is pushed.
//
// It only ever touches commits unique to the task's own branch, in the task's
// own worktree. It never rewrites the base branch or anyone else's work.
package sanitize

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// signOffPrefix is the trailer this package adds and must never strip.
const signOffPrefix = "Signed-off-by:"

// DefaultPatterns are the trailers stripped from every commit. New CLIs bring
// new trailer formats, and this list is the one place that tracks them.
var DefaultPatterns = []string{
	`(?i)^co-authored-by:\s*claude\b`,
	`(?i)^co-authored-by:\s*copilot\b`,
	`(?i)^co-authored-by:\s*codex\b`,
	`(?i)^co-authored-by:\s*.*\[bot\]`,
	`(?i)^assisted-by:\s*(claude|copilot|codex)\b`,
	`(?i)^claude-session:`,
	`(?i)^generated-with:`,
	`(?i)^🤖 generated with`,
	`(?i)^generated with \[claude code\]`,
}

// Signature is the identity a DCO sign-off is written with.
type Signature struct {
	Name, Email string
}

// String renders the signature as it appears in a trailer.
func (s Signature) String() string { return fmt.Sprintf("%s <%s>", s.Name, s.Email) }

// Policy is what to strip and what to add.
type Policy struct {
	// Strip removes any message line matching one of these.
	Strip []*regexp.Regexp
	// SignOff, when set, adds a DCO trailer to commits that lack one.
	SignOff *Signature
}

// NewPolicy compiles a pattern list into a policy.
func NewPolicy(patterns []string, signOff *Signature) (Policy, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return Policy{}, fmt.Errorf("invalid trailer pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return Policy{Strip: compiled, SignOff: signOff}, nil
}

// LoadPatterns returns the default patterns plus any extra ones from path, one
// regular expression per line, blank lines and # comments ignored. A missing
// file is not an error — it just means the defaults are the whole list.
func LoadPatterns(path string) ([]string, error) {
	patterns := append([]string(nil), DefaultPatterns...)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return patterns, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return patterns, nil
}

// Clean rewrites one commit message. It reports whether anything changed, so
// callers can skip rewriting a branch that is already compliant.
func (p Policy) Clean(message string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")

	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		// The subject is never a trailer, and stripping it would leave a
		// commit with no message at all.
		if i > 0 && p.stripped(line) {
			continue
		}
		kept = append(kept, line)
	}

	body := strings.TrimRight(strings.Join(kept, "\n"), "\n \t")
	if p.SignOff != nil {
		body = addSignOff(body, *p.SignOff)
	}
	body += "\n"

	return body, body != normalize(message)
}

// stripped reports whether a line should be removed. A sign-off is never
// stripped, whatever the patterns say — this pass exists partly to add them.
func (p Policy) stripped(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, signOffPrefix) {
		return false
	}
	for _, re := range p.Strip {
		if re.MatchString(trimmed) {
			return true
		}
	}
	return false
}

// addSignOff appends a DCO trailer unless the exact one is already present.
func addSignOff(body string, sig Signature) string {
	want := signOffPrefix + " " + sig.String()
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			return body
		}
	}
	// A trailer belongs in the trailer block: separated from the subject or
	// body by a blank line, and never glued onto prose.
	if body == "" {
		return want
	}
	if lastLineIsTrailer(body) {
		return body + "\n" + want
	}
	return body + "\n\n" + want
}

// lastLineIsTrailer reports whether the message already ends in a trailer
// block, so a sign-off can join it rather than starting a new one.
var trailerLine = regexp.MustCompile(`^[A-Za-z][A-Za-z-]*:\s`)

func lastLineIsTrailer(body string) bool {
	lines := strings.Split(body, "\n")
	// A subject on its own is never a trailer block, however much "feat: x"
	// looks like one — a trailer needs a body above it to belong to.
	if len(lines) < 2 {
		return false
	}
	return trailerLine.MatchString(lines[len(lines)-1])
}

// normalize renders a message the way Clean would with nothing to change, so
// the two can be compared to decide whether a rewrite is needed at all.
func normalize(message string) string {
	return strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), "\n \t") + "\n"
}
