package orc

import (
	"strings"
	"testing"
)

// TestLastLinesSkipsBlankLines covers the tail of a failed command's output
// that goes into its error. Blank lines carry nothing, so they must not use up
// the few lines the message has room for.
func TestLastLinesSkipsBlankLines(t *testing.T) {
	tests := map[string]struct {
		in   string
		n    int
		want string
	}{
		"fewer lines than asked": {"one\ntwo\n", 5, "one\ntwo"},
		"only the last n":        {"one\ntwo\nthree\n", 2, "two\nthree"},
		"blank lines between":    {"one\n\ntwo\n\n\nthree\n\n", 2, "two\nthree"},
		"nothing at all":         {"\n\n", 3, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := lastLines(tc.in, tc.n); got != tc.want {
				t.Errorf("lastLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// TestTheFailurePromptQuotesTheOutputAsWritten covers the other tail: what
// the agent is told the tests printed. Blank lines stay, because dropping them
// would hand the agent a different output from the one that failed.
func TestTheFailurePromptQuotesTheOutputAsWritten(t *testing.T) {
	output := "=== RUN TestGreeting\n    want:\n\thello\n\n\tworld\n    got:\n\thello\n\tworld\n--- FAIL: TestGreeting\n"
	got := testFailurePrompt("go test ./...", output)
	if want := "---\n" + strings.TrimSpace(output) + "\n---"; !strings.Contains(got, want) {
		t.Errorf("prompt does not quote the output verbatim:\n%s", got)
	}
	if !strings.Contains(got, "    go test ./...") {
		t.Errorf("prompt does not name the command:\n%s", got)
	}

	long := strings.Repeat("noise\n", testOutputLines) + "the summary\n"
	if got := testFailurePrompt("make test", long); strings.Count(got, "noise") != testOutputLines-1 ||
		!strings.Contains(got, "the summary") {
		t.Errorf("prompt should carry the last %d lines, summary included:\n%s", testOutputLines, got)
	}
}
