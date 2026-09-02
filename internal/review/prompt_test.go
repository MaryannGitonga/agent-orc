package review

import (
	"strings"
	"testing"
)

func TestReviewerPromptShowsTheTaskAndTheDiff(t *testing.T) {
	got := ReviewerPrompt("Fix the retry handler", "main")
	for _, want := range []string{"Fix the retry handler", "git diff main...HEAD", "Correctness", "Completeness", "Style", LGTM} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// The reviewer must not touch the branch it is judging.
	for _, want := range []string{"Do not edit", "Do not commit"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing the instruction %q", want)
		}
	}
}

func TestFeedbackPromptCarriesTheCommentsAndTheRules(t *testing.T) {
	got := FeedbackPrompt([]string{"handler.go: the retry never backs off", "add a test"})
	for _, want := range []string{"the retry never backs off", "add a test", "Do not push", "Co-authored-by"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestParseVerdictApproval(t *testing.T) {
	for _, output := range []string{"LGTM", "  lgtm  ", "**LGTM**", "LGTM.\n"} {
		got, err := ParseVerdict(output)
		if err != nil {
			t.Errorf("ParseVerdict(%q) = %v", output, err)
			continue
		}
		if !got.Approved {
			t.Errorf("ParseVerdict(%q).Approved = false, want true", output)
		}
	}
}

func TestParseVerdictComments(t *testing.T) {
	output := "Here is what I found:\n\n- handler.go: the retry never backs off\n- add a test for the timeout path\n"
	got, err := ParseVerdict(output)
	if err != nil {
		t.Fatalf("ParseVerdict() = %v", err)
	}
	if got.Approved {
		t.Error("Approved = true, want false when there are comments")
	}
	if len(got.Comments) != 2 {
		t.Fatalf("got %d comments, want 2: %v", len(got.Comments), got.Comments)
	}
	if got.Comments[0] != "handler.go: the retry never backs off" {
		t.Errorf("Comments[0] = %q, want the bullet text without its marker", got.Comments[0])
	}
}

func TestParseVerdictPrefersCommentsOverAStrayApproval(t *testing.T) {
	// A reviewer that raised something concrete is not approving, whatever
	// else its prose says.
	got, err := ParseVerdict("LGTM overall, but:\n- handler.go: fix the backoff\n")
	if err != nil {
		t.Fatalf("ParseVerdict() = %v", err)
	}
	if got.Approved {
		t.Error("Approved = true, want false when concrete comments were raised")
	}
	if len(got.Comments) != 1 {
		t.Errorf("got %d comments, want 1", len(got.Comments))
	}
}

func TestParseVerdictRejectsAnUnreadableResponse(t *testing.T) {
	for _, output := range []string{"", "I had a look and it seems fine to me.", "   \n\n  "} {
		if _, err := ParseVerdict(output); err == nil {
			t.Errorf("ParseVerdict(%q) = nil, want an error rather than a guess", output)
		}
	}
}
