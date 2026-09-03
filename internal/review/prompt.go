// Package review runs an independent agentic review of a finished task branch
// and feeds actionable comments back to the worker that produced it.
//
// The reviewer is a *fresh* session in its own worktree, never a resume of the
// worker's. That is the whole point: a reviewer that inherited the worker's
// conversation would also inherit its framing of the problem. It sees the diff
// and the original task, the way a human reviewer sees the PR and not the
// author's scratch work.
//
// The loop is sequential and hard-capped. The worker and the reviewer never
// run at the same time and never message each other. This is a bounded
// pre-check, not a replacement for human review.
package review

import (
	"fmt"
	"strings"
)

// LGTM is the verdict a reviewer returns when it has nothing to raise.
const LGTM = "LGTM"

// ReviewerPrompt builds the prompt handed to the reviewing session.
//
// It must be sent on a task with Raw set. Otherwise Task.Render appends the
// worker operating rules, which tell the agent to commit its work, and a
// reviewer that has just been told not to edit anything would be handed the
// opposite instruction while sitting in a checkout of the branch under review.
// The caller in internal/orc sets Raw for exactly this reason; that coupling is
// covered by TestReviewerIsNotToldToCommit.
func ReviewerPrompt(taskPrompt, baseBranch string) string {
	var b strings.Builder
	b.WriteString("You are reviewing a change on the branch checked out in this worktree.\n")
	fmt.Fprintf(&b, "Review the diff against `%s`. Run `git diff %s...HEAD` to see it.\n\n", baseBranch, baseBranch)
	b.WriteString("The change was made to satisfy this task:\n\n---\n")
	b.WriteString(strings.TrimSpace(taskPrompt))
	b.WriteString("\n---\n\n")
	b.WriteString(`Judge it on three things, in this order:
1. Correctness: does it do what it claims, without introducing bugs?
2. Completeness: does it actually satisfy the task above?
3. Style: does it match the surrounding code?

Do not edit anything. Do not commit, push, or create branches. Review only.

Respond in exactly one of two forms:
- The single word ` + LGTM + ` on its own line, if you would approve it as is.
- Otherwise, a short list of concrete, actionable comments, one per line, each
  starting with "- ". Name the file and what to change. Do not pad the list
  with observations you would not ask a colleague to act on.`)
	return b.String()
}

// FeedbackPrompt builds the prompt that hands the reviewer's comments back to
// the worker's own session.
func FeedbackPrompt(comments []string) string {
	var b strings.Builder
	b.WriteString("An independent reviewer looked at your branch and raised the following:\n\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	b.WriteString("\nAddress these on the same branch, in this worktree. Commit your changes\n")
	b.WriteString("locally with a one-line conventional commit subject. Do not push, do not\n")
	b.WriteString("open a pull request, and do not add any Co-authored-by trailer.\n")
	b.WriteString("If you disagree with a comment, say why in your reply rather than\n")
	b.WriteString("changing the code to match it.\n")
	return b.String()
}

// Verdict is what a reviewer decided.
type Verdict struct {
	// Approved is true when the reviewer returned LGTM.
	Approved bool
	// Comments are the actionable items, when it did not.
	Comments []string
}

// ParseVerdict reads a reviewer's response.
//
// Anything that is neither an approval nor a usable comment list is an error
// rather than a guess: §15's rule is that an unparseable review stops and
// waits for a human, because acting on a misread verdict is worse than not
// acting at all.
func ParseVerdict(output string) (Verdict, error) {
	var comments []string
	approved := false

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if isApproval(line) {
			approved = true
			continue
		}
		if item, ok := bulletText(line); ok {
			comments = append(comments, item)
		}
	}

	switch {
	case len(comments) > 0:
		// Comments win over a stray "LGTM" elsewhere in the output: a
		// reviewer that raised something concrete is not approving.
		return Verdict{Comments: comments}, nil
	case approved:
		return Verdict{Approved: true}, nil
	default:
		return Verdict{}, fmt.Errorf("the reviewer's response was neither %s nor a list of comments", LGTM)
	}
}

// isApproval reports whether a line is the reviewer's approval.
func isApproval(line string) bool {
	trimmed := strings.Trim(line, "*_`.# ")
	return strings.EqualFold(trimmed, LGTM)
}

// bulletText returns the text of a "- " list item.
func bulletText(line string) (string, bool) {
	for _, prefix := range []string{"- ", "* ", "• "} {
		if item, ok := strings.CutPrefix(line, prefix); ok {
			item = strings.TrimSpace(item)
			if item != "" {
				return item, true
			}
		}
	}
	return "", false
}
