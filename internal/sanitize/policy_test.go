package sanitize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policy(t *testing.T, signOff *Signature) Policy {
	t.Helper()
	p, err := NewPolicy(DefaultPatterns, signOff)
	if err != nil {
		t.Fatalf("NewPolicy() = %v", err)
	}
	return p
}

func TestCleanStripsAttributionTrailers(t *testing.T) {
	tests := map[string]string{
		"claude co-author":  "Co-Authored-By: Claude <noreply@anthropic.com>",
		"copilot co-author": "Co-authored-by: Copilot <copilot@github.com>",
		"codex co-author":   "Co-authored-by: Codex <codex@openai.com>",
		"any bot":           "Co-authored-by: some-thing[bot] <b@example.com>",
		"session link":      "Claude-Session: https://claude.ai/code/session_123",
		"generated with":    "🤖 Generated with [Claude Code](https://claude.com/claude-code)",
	}
	for name, trailer := range tests {
		t.Run(name, func(t *testing.T) {
			got, changed := policy(t, nil).Clean("feat: add the thing\n\n" + trailer + "\n")
			if !changed {
				t.Error("Clean() reported no change, want the trailer stripped")
			}
			if strings.Contains(got, trailer) {
				t.Errorf("Clean() = %q, want %q removed", got, trailer)
			}
			if !strings.HasPrefix(got, "feat: add the thing") {
				t.Errorf("Clean() = %q, want the subject kept", got)
			}
		})
	}
}

func TestCleanNeverStripsASignOff(t *testing.T) {
	// The same pass adds sign-offs; stripping one would be the worst possible
	// bug in this package, so it is guarded explicitly.
	p, err := NewPolicy(append(DefaultPatterns, `(?i)^signed-off-by:`), nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := "fix: thing\n\nSigned-off-by: A Dev <dev@example.com>\n"
	got, _ := p.Clean(msg)
	if !strings.Contains(got, "Signed-off-by: A Dev <dev@example.com>") {
		t.Errorf("Clean() = %q, want the sign-off kept even when a pattern matches it", got)
	}
}

func TestCleanLeavesACompliantMessageAlone(t *testing.T) {
	msg := "feat: add the thing\n\nSigned-off-by: A Dev <dev@example.com>\n"
	got, changed := policy(t, nil).Clean(msg)
	if changed {
		t.Errorf("Clean() reported a change to an already-clean message: %q", got)
	}
	if got != msg {
		t.Errorf("Clean() = %q, want %q", got, msg)
	}
}

func TestCleanNeverStripsTheSubject(t *testing.T) {
	// A subject that happens to look like a trailer must survive: stripping it
	// would leave a commit with no message.
	msg := "Co-authored-by: Claude <x@y>\n"
	got, _ := policy(t, nil).Clean(msg)
	if !strings.HasPrefix(got, "Co-authored-by: Claude") {
		t.Errorf("Clean() = %q, want the subject line kept", got)
	}
}

func TestCleanAddsASignOffWhenMissing(t *testing.T) {
	sig := Signature{Name: "A Dev", Email: "dev@example.com"}
	got, changed := policy(t, &sig).Clean("feat: add the thing\n")
	if !changed {
		t.Error("Clean() reported no change, want a sign-off added")
	}
	want := "feat: add the thing\n\nSigned-off-by: A Dev <dev@example.com>\n"
	if got != want {
		t.Errorf("Clean() = %q, want %q", got, want)
	}
}

func TestCleanDoesNotDuplicateAnExistingSignOff(t *testing.T) {
	sig := Signature{Name: "A Dev", Email: "dev@example.com"}
	msg := "feat: thing\n\nSigned-off-by: A Dev <dev@example.com>\n"
	got, changed := policy(t, &sig).Clean(msg)
	if changed {
		t.Errorf("Clean() = %q, want no change", got)
	}
	if n := strings.Count(got, "Signed-off-by:"); n != 1 {
		t.Errorf("Clean() produced %d sign-offs, want 1:\n%s", n, got)
	}
}

func TestCleanJoinsAnExistingTrailerBlock(t *testing.T) {
	sig := Signature{Name: "A Dev", Email: "dev@example.com"}
	got, _ := policy(t, &sig).Clean("feat: thing\n\nRefs: PROJ-1\n")
	want := "feat: thing\n\nRefs: PROJ-1\nSigned-off-by: A Dev <dev@example.com>\n"
	if got != want {
		t.Errorf("Clean() = %q, want the sign-off to join the trailer block:\n%q", got, want)
	}
}

func TestCleanSeparatesASignOffFromProse(t *testing.T) {
	sig := Signature{Name: "A Dev", Email: "dev@example.com"}
	got, _ := policy(t, &sig).Clean("feat: thing\n\nSome explanation of the change.\n")
	if !strings.Contains(got, "change.\n\nSigned-off-by:") {
		t.Errorf("Clean() = %q, want a blank line between the prose and the trailer", got)
	}
}

func TestCleanStripsAndSignsInOnePass(t *testing.T) {
	sig := Signature{Name: "A Dev", Email: "dev@example.com"}
	msg := "feat: thing\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"
	got, changed := policy(t, &sig).Clean(msg)
	if !changed {
		t.Fatal("Clean() reported no change")
	}
	if strings.Contains(got, "Claude") {
		t.Errorf("Clean() = %q, want the attribution stripped", got)
	}
	if !strings.Contains(got, "Signed-off-by: A Dev") {
		t.Errorf("Clean() = %q, want the sign-off added", got)
	}
}

func TestNewPolicyRejectsABadPattern(t *testing.T) {
	if _, err := NewPolicy([]string{"("}, nil); err == nil {
		t.Error("NewPolicy() with an invalid regexp = nil, want an error")
	}
}

func TestLoadPatternsExtendsTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trailers.txt")
	body := "# a comment\n\n(?i)^sponsored-by:\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPatterns(path)
	if err != nil {
		t.Fatalf("LoadPatterns() = %v", err)
	}
	if len(got) != len(DefaultPatterns)+1 {
		t.Errorf("LoadPatterns() returned %d patterns, want the defaults plus one", len(got))
	}
	if got[len(got)-1] != `(?i)^sponsored-by:` {
		t.Errorf("last pattern = %q, want the one from the file", got[len(got)-1])
	}
}

func TestLoadPatternsWithNoFile(t *testing.T) {
	got, err := LoadPatterns(filepath.Join(t.TempDir(), "absent.txt"))
	if err != nil {
		t.Fatalf("LoadPatterns() = %v, want a missing file to be fine", err)
	}
	if len(got) != len(DefaultPatterns) {
		t.Errorf("LoadPatterns() returned %d patterns, want just the defaults", len(got))
	}
}

func TestParseSignature(t *testing.T) {
	got, err := ParseSignature("A Dev <dev@example.com>")
	if err != nil {
		t.Fatalf("ParseSignature() = %v", err)
	}
	if got.Name != "A Dev" || got.Email != "dev@example.com" {
		t.Errorf("ParseSignature() = %+v, want A Dev / dev@example.com", got)
	}
	for _, bad := range []string{"A Dev", "<dev@example.com>", "A Dev <>", ""} {
		if _, err := ParseSignature(bad); err == nil {
			t.Errorf("ParseSignature(%q) = nil, want an error", bad)
		}
	}
}
