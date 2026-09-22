package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/MaryannGitonga/agent-orc/internal/paths"

	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

func mkTask(id string, cli task.CLI, status state.Status, branch string) state.Task {
	return state.Task{
		Task:      task.Task{ID: id, CLI: cli, Branch: branch},
		Status:    status,
		StartedAt: time.Now().UTC().Add(-time.Minute),
	}
}

func TestFilterMatchesTheFieldsSomeoneWouldType(t *testing.T) {
	tasks := []state.Task{
		mkTask("PROJ-1234", task.CLIClaude, state.StatusRunning, "agent-orc/proj-1234"),
		mkTask("DEP-88", task.CLICopilot, state.StatusDone, "chore/dep-88"),
		mkTask("FIX-501", task.CLIClaude, state.StatusFailed, "agent-orc/fix-501"),
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"PROJ-1234", "DEP-88", "FIX-501"}},
		{"   ", []string{"PROJ-1234", "DEP-88", "FIX-501"}},
		{"proj", []string{"PROJ-1234"}}, // id, and case-insensitively
		{"copilot", []string{"DEP-88"}}, // cli
		{"chore/", []string{"DEP-88"}},  // branch
		{"failed", []string{"FIX-501"}}, // status
		{"claude", []string{"PROJ-1234", "FIX-501"}},
		{"nothing matches this", nil},
	} {
		got := filterTasks(tasks, tc.query)
		if len(got) != len(tc.want) {
			t.Errorf("filter(%q) returned %d task(s), want %d", tc.query, len(got), len(tc.want))
			continue
		}
		for i, id := range tc.want {
			if got[i].ID != id {
				t.Errorf("filter(%q)[%d] = %q, want %q", tc.query, i, got[i].ID, id)
			}
		}
	}
}

func TestPadKeepsColumnsInPlace(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"abc", 3, "abc"},
		{"ab", 4, "ab  "},
		{"", 2, "  "},
		{"abcdef", 4, "abc…"},
		{"abcdef", 1, "a"},
	} {
		if got := pad(tc.in, tc.width); got != tc.want {
			t.Errorf("pad(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
		// Whatever it does, the column must be exactly as wide as asked, or
		// every column to its right moves.
		if n := len([]rune(pad(tc.in, tc.width))); n != tc.width {
			t.Errorf("pad(%q, %d) is %d wide, want %d", tc.in, tc.width, n, tc.width)
		}
	}
}

func TestEveryStatusHasAMark(t *testing.T) {
	all := []state.Status{
		state.StatusPending, state.StatusRunning, state.StatusVerifying,
		state.StatusReviewing, state.StatusPublishing, state.StatusDone,
		state.StatusReviewed, state.StatusStopped, state.StatusFailed,
		state.StatusPublishFailed, state.StatusReviewFailed, state.StatusPolicyViolation,
	}
	for _, s := range all {
		if mark(s) == "" {
			t.Errorf("status %q has no mark", s)
		}
	}
	// An unrecognised status is a failure rather than a blank column: a record
	// written by a newer version must still render as something.
	if got := mark(state.Status("from-the-future")); got != "✗" {
		t.Errorf("mark(unknown) = %q, want the failure mark", got)
	}
}

func TestDetailNamesThePhaseNotJustTheStatus(t *testing.T) {
	verifying := mkTask("V-1", task.CLIClaude, state.StatusVerifying, "agent-orc/v-1")
	verifying.TestRuns = 2
	if got := detail(verifying); !strings.Contains(got, "test attempt 2") {
		t.Errorf("detail() = %q, want it to name the attempt", got)
	}

	reviewing := mkTask("R-1", task.CLIClaude, state.StatusReviewing, "agent-orc/r-1")
	reviewing.ReviewRound = 1
	// Rounds are counted as completed, so the one under way is the next.
	if got := detail(reviewing); !strings.Contains(got, "round 2") {
		t.Errorf("detail() = %q, want it to name the round under way", got)
	}

	done := mkTask("D-1", task.CLIClaude, state.StatusDone, "agent-orc/d-1")
	done.PRURL = "https://github.com/o/r/pull/7"
	if got := detail(done); !strings.Contains(got, done.PRURL) {
		t.Errorf("detail() = %q, want the PR url once there is one", got)
	}
}

func TestTaskInfoLeavesOutWhatIsNotSet(t *testing.T) {
	bare := mkTask("B-1", task.CLICodex, state.StatusRunning, "agent-orc/b-1")
	info := taskInfo(bare)
	if !strings.Contains(info, "B-1") {
		t.Errorf("taskInfo() = %q, want the id", info)
	}
	for _, absent := range []string{"session", "pr", "error", "test runs", "sanitized"} {
		if strings.Contains(info, absent) {
			t.Errorf("taskInfo() mentions %q for a task that has none:\n%s", absent, info)
		}
	}

	full := bare
	full.SessionID = "0a3f77c2"
	full.TestRuns = 3
	passed := false
	full.TestsPassed = &passed
	full.RewrittenCommits = 2
	info = taskInfo(full)
	for _, want := range []string{"0a3f77c2", "3, last failed", "2 commit message(s)"} {
		if !strings.Contains(info, want) {
			t.Errorf("taskInfo() = %q, want it to contain %q", info, want)
		}
	}
}

func TestCountSummaryCountsEveryStatus(t *testing.T) {
	tasks := []state.Task{
		mkTask("A", task.CLIClaude, state.StatusRunning, "a"),
		mkTask("B", task.CLIClaude, state.StatusRunning, "b"),
		mkTask("C", task.CLIClaude, state.StatusDone, "c"),
	}
	got := strings.Join(countSummary(tasks), " ")
	for _, want := range []string{"2 running", "1 done"} {
		if !strings.Contains(got, want) {
			t.Errorf("countSummary() = %q, want it to contain %q", got, want)
		}
	}
	if len(countSummary(nil)) != 0 {
		t.Error("countSummary(nil) returned something, want nothing to render")
	}
}

// TestViewRendersWithoutATerminal covers the whole render path on a model that
// has been handed a size and some tasks, which is all View needs. It is the
// cheapest guard against a panic in a layout that only a real terminal would
// otherwise exercise.
func TestViewRendersWithoutATerminal(t *testing.T) {
	m := newModel(paths.New(t.TempDir()))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	loaded, _ := sized.(model).Update(tasksMsg{tasks: []state.Task{
		mkTask("PROJ-1234", task.CLIClaude, state.StatusVerifying, "agent-orc/proj-1234"),
		mkTask("DEP-88", task.CLICopilot, state.StatusDone, "chore/dep-88"),
	}})

	out := loaded.(model).View()
	for _, want := range []string{"agent-orc", "PROJ-1234", "DEP-88", "verifying", "supervisor", "q quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() is missing %q:\n%s", want, out)
		}
	}

	// An empty store still renders, and says what to do about it.
	empty := newModel(paths.New(t.TempDir()))
	sized, _ = empty.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if out := sized.(model).View(); !strings.Contains(out, "no tasks") {
		t.Errorf("View() with no tasks = %q, want it to say so", out)
	}
}

func TestModelSitsBesideTheCLIUntilTheTerminalIsTooNarrow(t *testing.T) {
	task1 := mkTask("PROJ-1234", task.CLIClaude, state.StatusRunning, "agent-orc/proj-1234")
	task1.Model = "claude-opus-5"

	wide := columnsFor(120)
	if !wide.model {
		t.Error("columnsFor(120) dropped the model column on a wide terminal")
	}
	if got := wide.row(task1, lipgloss.NewStyle()); !strings.Contains(got, "claude-opus-5") {
		t.Errorf("row() = %q, want the model beside the CLI", got)
	}
	if got := wide.header(); !strings.Contains(got, "MODEL") {
		t.Errorf("header() = %q, want a MODEL column", got)
	}

	// The model is the first column to go, because the detail line repeats it.
	narrow := columnsFor(80)
	if narrow.model {
		t.Error("columnsFor(80) kept the model column on a narrow terminal")
	}
	if got := narrow.row(task1, lipgloss.NewStyle()); strings.Contains(got, "claude-opus-5") {
		t.Errorf("row() = %q, want the model dropped when it does not fit", got)
	}

	// A task that never named a model ran on the CLI's own default.
	task1.Model = ""
	if got := wide.row(task1, lipgloss.NewStyle()); !strings.Contains(got, "-") {
		t.Errorf("row() = %q, want a dash for an unnamed model", got)
	}

	// Whatever the width, a row must not be wider than the terminal, or it
	// wraps and every row after it is pushed out of place.
	for _, width := range []int{60, 80, 100, 120, 200} {
		c := columnsFor(width)
		if n := len([]rune(stripANSI(c.row(task1, lipgloss.NewStyle())))) + 2; n > width && width >= colsWithoutModel+minBranch {
			t.Errorf("at width %d a row is %d wide", width, n)
		}
		if len([]rune(stripANSI(c.row(task1, lipgloss.NewStyle())))) != len([]rune(c.header())) {
			t.Errorf("at width %d the header and the rows are different widths", width)
		}
	}
}

// stripANSI removes the styling so a rendered width can be measured.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestShiftedArrowsScrollTheLogAndPlainOnesChooseATask covers the split between
// the two things the arrow keys could mean. Getting it wrong either way is
// easy to miss: a selection that scrolls instead, or a log that cannot be read
// one line at a time.
func TestShiftedArrowsScrollTheLogAndPlainOnesChooseATask(t *testing.T) {
	var cur tea.Model = newModel(paths.New(t.TempDir()))
	cur, _ = cur.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	cur, _ = cur.Update(tasksMsg{tasks: []state.Task{
		mkTask("FIRST-1", task.CLIClaude, state.StatusRunning, "agent-orc/first-1"),
		mkTask("SECOND-1", task.CLIClaude, state.StatusRunning, "agent-orc/second-1"),
	}})
	long := make([]string, 200)
	for i := range long {
		long[i] = "log line"
	}
	cur, _ = cur.Update(logMsg{id: "FIRST-1", pane: paneSupervisor, text: strings.Join(long, "\n")})

	key := func(k tea.KeyType) { cur, _ = cur.Update(tea.KeyMsg{Type: k}) }
	offset := func() int { return cur.(model).vp.YOffset }

	bottom := offset()
	if bottom == 0 {
		t.Fatal("a new log should open at its end, not its start")
	}

	key(tea.KeyShiftUp)
	if got := offset(); got != bottom-1 {
		t.Errorf("shift+up moved the log to %d, want one line up from %d", got, bottom)
	}
	key(tea.KeyShiftDown)
	if got := offset(); got != bottom {
		t.Errorf("shift+down moved the log to %d, want it back at %d", got, bottom)
	}
	if got := cur.(model).cursor; got != 0 {
		t.Errorf("scrolling moved the selection to row %d; it should stay put", got)
	}

	key(tea.KeyPgUp)
	if got := offset(); got >= bottom {
		t.Errorf("pgup left the log at %d, want it above %d", got, bottom)
	}

	scrolled := offset()
	key(tea.KeyDown)
	if got := cur.(model).cursor; got != 1 {
		t.Errorf("down left the selection on row %d, want the next task", got)
	}
	if got := offset(); got != scrolled {
		t.Errorf("down scrolled the log from %d to %d; it should only choose a task", scrolled, got)
	}

	if out := cur.(model).View(); !strings.Contains(out, "pgup/pgdn") || !strings.Contains(out, "shift+↑↓") {
		t.Errorf("the footer does not say how to scroll:\n%s", out)
	}
}

func TestFooterStaysOnOneLine(t *testing.T) {
	notes := []string{"following", `filter "claude" (3)`}
	for _, width := range []int{40, 60, 80, 100, 120, 200} {
		got := footer(width, notes)
		// One line, however narrow: the log is sized on that assumption, and a
		// footer that wraps pushes the whole screen down by a row.
		if w := lipgloss.Width(got); w > width && width >= 40 {
			t.Errorf("at width %d the footer is %d wide:\n%s", width, w, got)
		}
		if !strings.Contains(got, "q quit") {
			t.Errorf("at width %d the footer lost quit:\n%s", width, got)
		}
	}
	// With room to spare, nothing is dropped.
	wide := footer(200, nil)
	for _, k := range footerKeys {
		if !strings.Contains(wide, k) {
			t.Errorf("a 200-column footer is missing %q:\n%s", k, wide)
		}
	}
	// The most useful keys are the last to go.
	if narrow := footer(60, nil); !strings.Contains(narrow, "↑↓ select") {
		t.Errorf("a 60-column footer dropped the selection keys:\n%s", narrow)
	}
}
