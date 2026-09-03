package source

import (
	"strings"
	"testing"
)

func TestParseGitHubReferences(t *testing.T) {
	got, err := Parse("github://acme/data-mesh#87")
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got.Kind != KindGitHub {
		t.Errorf("Kind = %q, want %q", got.Kind, KindGitHub)
	}
	if got.Owner != "acme" || got.Repo != "data-mesh" || got.Number != 87 {
		t.Errorf("Parse() = %+v, want acme/data-mesh#87", got)
	}
}

func TestParseJIRAReferencesAndUppercasesTheKey(t *testing.T) {
	got, err := Parse("jira://proj-1234")
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got.Kind != KindJIRA {
		t.Errorf("Kind = %q, want %q", got.Kind, KindJIRA)
	}
	if got.Key != "PROJ-1234" {
		t.Errorf("Key = %q, want %q", got.Key, "PROJ-1234")
	}
}

func TestParseTreatsUnschemedTextAsFreeText(t *testing.T) {
	got, err := Parse("  fix the retry handler  ")
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got.Kind != KindText {
		t.Errorf("Kind = %q, want %q", got.Kind, KindText)
	}
	if got.Raw != "fix the retry handler" {
		t.Errorf("Raw = %q, want it trimmed", got.Raw)
	}
}

func TestParseRejectsMalformedReferences(t *testing.T) {
	tests := map[string]string{
		"github with no issue number": "github://org/repo",
		"github with a bad number":    "github://org/repo#abc",
		"jira with no number":         "jira://PROJ",
		"unknown scheme":              "gitlab://org/repo#1",
		"empty":                       "   ",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Errorf("Parse(%q) = nil, want an error", raw)
			}
		})
	}
}

func TestParseKeepsProseContainingAColonSlashSlash(t *testing.T) {
	// A prompt is allowed to mention a URL; only a bare scheme prefix is a
	// reference, and prose has spaces before it.
	got, err := Parse("see https://example.com for background")
	if err != nil {
		t.Fatalf("Parse() = %v, want prose to be accepted", err)
	}
	if got.Kind != KindText {
		t.Errorf("Kind = %q, want %q", got.Kind, KindText)
	}
}

func TestIssueText(t *testing.T) {
	tests := map[string]struct {
		issue Issue
		want  string
	}{
		"title and body": {Issue{"Retry loops", "It spins forever."}, "Retry loops\n\nIt spins forever."},
		"title only":     {Issue{Title: "Retry loops"}, "Retry loops"},
		"body only":      {Issue{Body: "It spins forever."}, "It spins forever."},
		"neither":        {Issue{}, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.issue.Text(); got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComposeKeepsBothTheTicketAndThePrompt(t *testing.T) {
	got := Compose("Ticket text", "Focus on the retry handler")
	if !strings.HasPrefix(got, "Ticket text") {
		t.Errorf("Compose() = %q, want the ticket first", got)
	}
	if !strings.Contains(got, "Additional instructions:\nFocus on the retry handler") {
		t.Errorf("Compose() = %q, want the prompt appended as extra instructions", got)
	}
}

func TestComposeWithOnlyOneSide(t *testing.T) {
	if got := Compose("", "just a prompt"); got != "just a prompt" {
		t.Errorf("Compose() = %q, want the prompt alone", got)
	}
	if got := Compose("just a ticket", ""); got != "just a ticket" {
		t.Errorf("Compose() = %q, want the ticket alone", got)
	}
}
