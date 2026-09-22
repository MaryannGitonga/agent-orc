package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// screen drives a model the way the program does, one message at a time, and
// keeps the result so a test reads as a sequence of things someone did.
type screen struct{ m model }

// newScreen sizes the model before any task is read, which is the order a real
// terminal delivers them in.
func newScreen(t *testing.T, width, height int) *screen {
	s := &screen{m: newModel(paths.New(t.TempDir()))}
	s.send(tea.WindowSizeMsg{Width: width, Height: height})
	return s
}

func (s *screen) send(msg tea.Msg) tea.Cmd {
	next, cmd := s.m.Update(msg)
	s.m = next.(model)
	return cmd
}

func (s *screen) load(tasks ...state.Task) { s.send(tasksMsg{tasks: tasks}) }

func (s *screen) press(k tea.KeyType)       { s.send(tea.KeyMsg{Type: k}) }
func (s *screen) typeRunes(r string)        { s.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)}) }
func (s *screen) lines() []string           { return strings.Split(s.m.View(), "\n") }
func (s *screen) selectedID() string        { return s.m.selectedID }
func (s *screen) showLog(text string)       { s.send(logMsg{id: s.m.selectedID, pane: s.m.pane, text: text}) }
func (s *screen) contains(want string) bool { return strings.Contains(s.m.View(), want) }

func tasks(ids ...string) []state.Task {
	out := make([]state.Task, len(ids))
	for i, id := range ids {
		out[i] = mkTask(id, task.CLIClaude, state.StatusRunning, "agent-orc/"+strings.ToLower(id))
	}
	return out
}

func numbered(n int) []state.Task {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("T-%02d", i+1)
	}
	return tasks(ids...)
}

// TestScreenFitsOnceTasksLoad covers the order a real terminal works in: the
// size arrives first, while the table is still empty, and the tasks a moment
// later. A log sized for the empty table made the screen taller than the
// terminal, and the top of it, header and table included, scrolled away.
func TestScreenFitsOnceTasksLoad(t *testing.T) {
	for _, n := range []int{0, 1, 5, 15} {
		s := newScreen(t, 100, 30)
		s.load(numbered(n)...)
		lines := s.lines()
		if len(lines) > 30 {
			t.Errorf("with %d task(s) the screen is %d lines, want at most 30", n, len(lines))
		}
		if !strings.Contains(lines[0], "agent-orc") {
			t.Errorf("with %d task(s) the header is not on the first line:\n%s", n, lines[0])
		}
	}

	// An error line appearing later must be counted too.
	s := newScreen(t, 100, 30)
	s.load(numbered(5)...)
	s.send(tasksMsg{err: errors.New("permission denied")})
	if n := len(s.lines()); n > 30 {
		t.Errorf("with an error showing the screen is %d lines, want at most 30", n)
	}
	if !s.contains("could not read state") {
		t.Error("the read error is not shown")
	}
}

// TestSelectionFollowsTheTaskNotTheRow covers what someone watching a task
// expects: to keep watching it. Rows are newest first, so a new dispatch, or a
// filter hiding a row above, used to move the selection to a different task.
func TestSelectionFollowsTheTaskNotTheRow(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("B", "A")...)
	s.press(tea.KeyDown)
	if got := s.selectedID(); got != "A" {
		t.Fatalf("selected %q after moving down, want A", got)
	}

	// Another task is dispatched and appears at the top.
	s.load(tasks("C", "B", "A")...)
	if got := s.selectedID(); got != "A" {
		t.Errorf("a new dispatch moved the selection to %q, want it to stay on A", got)
	}

	// A filter hides a row above the selection.
	mixed := tasks("C", "B", "A")
	mixed[1].CLI = task.CLICopilot
	s.load(mixed...)
	s.typeRunes("/")
	s.typeRunes("claude")
	if got := s.selectedID(); got != "A" {
		t.Errorf("filtering moved the selection to %q, want it to stay on A", got)
	}

	// Only when the task itself goes does the selection move, to a neighbour.
	s.press(tea.KeyEsc)
	s.load(tasks("C", "B")...)
	if got := s.selectedID(); got == "" || got == "A" {
		t.Errorf("after A was cleaned up the selection is %q, want a remaining task", got)
	}
}

// TestTableScrollsToKeepTheSelectionVisible covers a list longer than the
// table. Moving past the last visible row used to select rows that were never
// drawn, with a detail block and a log for a task you could not see.
func TestTableScrollsToKeepTheSelectionVisible(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(numbered(15)...)
	for i := 0; i < 13; i++ {
		s.press(tea.KeyDown)
	}
	if got := s.selectedID(); got != "T-14" {
		t.Fatalf("selected %q after 13 moves, want T-14", got)
	}
	if !s.contains("▸ T-14") {
		t.Errorf("the selected row T-14 is not on screen:\n%s", s.m.View())
	}
	if !s.contains("of 15") {
		t.Error("a scrolling table does not say which rows it is showing")
	}

	// And back up to the top.
	for i := 0; i < 13; i++ {
		s.press(tea.KeyUp)
	}
	if !s.contains("▸ T-01") {
		t.Errorf("after moving back up, T-01 is not on screen:\n%s", s.m.View())
	}
}

// TestCtrlCQuitsFromTheFilter covers the one key that must work wherever the
// focus is.
func TestCtrlCQuitsFromTheFilter(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	s.typeRunes("/")
	if !s.m.filtering {
		t.Fatal("'/' did not open the filter")
	}
	cmd := s.send(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c in the filter did nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c in the filter did not quit")
	}
}

// TestSwitchingPanesOpensAtTheEnd covers moving from one log to another. The
// new log used to inherit the old one's scroll position, landing somewhere in
// the middle of a different file.
func TestSwitchingPanesOpensAtTheEnd(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	long := strings.Repeat("supervisor line\n", 200)
	s.showLog(long)
	for i := 0; i < 20; i++ {
		s.press(tea.KeyShiftUp)
	}
	if s.m.vp.AtBottom() {
		t.Fatal("scrolling up left the log at its end")
	}

	s.press(tea.KeyTab)
	s.showLog(strings.Repeat("agent line\n", 200))
	if !s.m.vp.AtBottom() {
		t.Error("switching to the agent log kept the supervisor log's scroll position")
	}
}

// TestHighlightCoversTheWholeRow covers the selected row's background. The
// status cell ends by resetting its colour, and the reset used to take the
// highlight with it, leaving spend, rounds, elapsed and branch unmarked.
func TestHighlightCoversTheWholeRow(t *testing.T) {
	before := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(before)

	one := mkTask("PROJ-1234", task.CLIClaude, state.StatusVerifying, "agent-orc/proj-1234")
	row := columnsFor(120).row(one, selected, false)

	after := row[strings.Index(row, "verifying")+len("verifying"):]
	if !strings.Contains(after, "48;5;236") {
		t.Errorf("the columns after the status have no highlight:\n%q", after)
	}
}

// TestNoLineIsWiderThanTheTerminal covers the lines that used to wrap: the
// detail line once a PR url is set, and every table row below 74 columns. A
// wrapped line takes a row the layout never counted.
func TestNoLineIsWiderThanTheTerminal(t *testing.T) {
	withPR := mkTask("PROJ-1234", task.CLIClaude, state.StatusDone, "agent-orc/proj-1234")
	withPR.PRURL = "https://github.com/MaryannGitonga/agent-orc/pull/1234"
	for _, width := range []int{50, 60, 74, 80, 100} {
		s := newScreen(t, width, 30)
		s.load(withPR, mkTask("DEP-88", task.CLICopilot, state.StatusRunning, "chore/dep-88"))
		for i, line := range s.lines() {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("at width %d, line %d is %d wide:\n%s", width, i, w, line)
			}
		}
		if n := len(s.lines()); n > 30 {
			t.Errorf("at width %d the screen is %d lines, want at most 30", width, n)
		}
	}
}

// TestFollowingShowsTheNewestLineEvenWhenLinesWrap covers a log with long
// lines. The viewport wraps them and then drops whatever overflows from the
// bottom, which while following is exactly the newest line.
func TestFollowingShowsTheNewestLineEvenWhenLinesWrap(t *testing.T) {
	s := newScreen(t, 60, 24)
	s.load(tasks("A")...)
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(strings.Repeat("a long supervisor line ", 8) + "\n")
	}
	b.WriteString("THE NEWEST LINE")
	s.showLog(b.String())
	if !s.contains("THE NEWEST LINE") {
		t.Errorf("following a log with wrapped lines hides its newest line:\n%s", s.m.View())
	}
}

// TestNoLogWithoutItsTask covers the log pane outliving what it belongs to. A
// filter matching nothing, or the selected task being cleaned up, cleared the
// detail lines but left the previous task's log on screen beneath them.
func TestNoLogWithoutItsTask(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("DEMO-2", "DEMO-1")...)
	s.showLog("DEMO-2 SUPERVISOR LINE")
	if !s.contains("DEMO-2 SUPERVISOR LINE") {
		t.Fatal("the selected task's log is not shown to begin with")
	}

	s.typeRunes("/")
	s.typeRunes("zzz")
	if !s.contains("no tasks") {
		t.Fatal("a filter matching nothing does not say so")
	}
	if s.contains("DEMO-2 SUPERVISOR LINE") {
		t.Error("a filter matching nothing still shows the last task's log")
	}

	// Cleared, and the selected task is then cleaned up out from under it.
	s.press(tea.KeyEsc)
	s.showLog("DEMO-2 SUPERVISOR LINE")
	s.load(tasks("DEMO-1")...)
	if s.contains("DEMO-2 SUPERVISOR LINE") {
		t.Error("a removed task's log is still shown under the task that replaced it")
	}

	// And the pane is still the height the layout gave it.
	if n := len(s.lines()); n > 30 {
		t.Errorf("with the log blanked the screen is %d lines, want at most 30", n)
	}
}

// TestFooterNeverWraps covers the notes the footer carries alongside its key
// hints. Dropping hints made room for them only up to a point, and a long
// filter on an 80 column terminal still wrapped onto a second line.
func TestFooterNeverWraps(t *testing.T) {
	s := newScreen(t, 80, 30)
	s.load(tasks("A")...)
	s.typeRunes("/")
	s.typeRunes(strings.Repeat("a-very-long-filter-", 5))
	s.press(tea.KeyEnter)

	lines := s.lines()
	if n := len(lines); n > 30 {
		t.Errorf("with a long filter the screen is %d lines, want at most 30", n)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("line %d is %d wide:\n%s", i, w, line)
		}
	}
	if !strings.Contains(lines[len(lines)-1], "filter") {
		t.Errorf("the footer lost the filter note entirely:\n%s", lines[len(lines)-1])
	}
}

// TestSummaryIsInLifecycleOrder covers the header's tally. It used to be
// alphabetical, which put done and failed ahead of the tasks still working.
func TestSummaryIsInLifecycleOrder(t *testing.T) {
	var all []state.Task
	for _, st := range []state.Status{state.StatusFailed, state.StatusDone, state.StatusRunning, state.StatusPending} {
		all = append(all, mkTask(string(st), task.CLIClaude, st, "b"))
	}
	all = append(all, mkTask("new", task.CLIClaude, state.Status("from-the-future"), "b"))

	got := strings.Join(countSummary(all), " ")
	want := []string{"1 pending", "1 running", "1 done", "1 failed", "1 from-the-future"}
	last := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Errorf("summary %q is missing %q", got, w)
			continue
		}
		if i < last {
			t.Errorf("summary %q has %q out of order", got, w)
		}
		last = i
	}
}

func numberedLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "log line %d\n", i)
	}
	return strings.TrimRight(b.String(), "\n")
}

// TestTickDoesNotStackReads covers a read slower than the refresh. Each tick
// used to start another read regardless, so on a large log they overlapped and
// piled up; a tick now leaves the log alone while a read is still out.
func TestTickDoesNotStackReads(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	s.send(tickMsg{})
	if s.m.reading != 1 {
		t.Fatalf("after one tick %d read(s) are out, want 1", s.m.reading)
	}
	s.send(tickMsg{})
	s.send(tickMsg{})
	if s.m.reading != 1 {
		t.Errorf("ticks while a read was out started more: %d out, want 1", s.m.reading)
	}
	s.showLog("done reading")
	if s.m.reading != 0 {
		t.Errorf("after the read came back %d are still counted as out", s.m.reading)
	}
	s.send(tickMsg{})
	if s.m.reading != 1 {
		t.Errorf("once the read finished, the next tick did not start another: %d out", s.m.reading)
	}
}

// TestScrolledUpTextStaysStill covers reading back through a log that keeps
// growing. The pane holds a window of the last lines, and a newer window
// swapped in under the same scroll position slid the text up by however much
// had arrived.
func TestScrolledUpTextStaysStill(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	s.showLog(numberedLines(1, 300))
	for i := 0; i < 30; i++ {
		s.press(tea.KeyShiftUp)
	}
	before := s.m.renderLog()

	// Forty new lines arrive; the window now starts forty lines later.
	s.showLog(numberedLines(41, 340))
	if after := s.m.renderLog(); after != before {
		t.Errorf("the text moved while scrolled up:\nbefore\n%s\nafter\n%s", before, after)
	}
	if !s.m.newer || !strings.Contains(s.m.renderFooter(), "new output") {
		t.Error("newer output was held back without saying so")
	}

	// G asks for the latest, and gets it.
	s.typeRunes("G")
	s.send(logMsg{id: "A", pane: s.m.pane, text: numberedLines(41, 340), force: true, toEnd: true})
	if !strings.Contains(s.m.renderLog(), "log line 340") || s.m.newer {
		t.Errorf("G did not bring the newest output into view:\n%s", s.m.renderLog())
	}
}

// TestShortTerminalsKeepTheHeader covers the smallest screens. The table used
// to keep at least three rows, which with the fixed lines outgrew a short
// terminal, and the renderer then dropped the top lines, the header first.
func TestShortTerminalsKeepTheHeader(t *testing.T) {
	for _, tc := range []struct{ height, tasks int }{
		{10, 3}, {13, 5}, {13, 15}, {16, 15}, {24, 15},
		// Short enough that even without spacers the table must drop below
		// three rows, with the scroll line showing as well.
		{9, 15}, {10, 15},
	} {
		s := newScreen(t, 100, tc.height)
		s.load(numbered(tc.tasks)...)
		lines := s.lines()
		if len(lines) > tc.height {
			t.Errorf("%d rows, %d tasks: the screen is %d lines", tc.height, tc.tasks, len(lines))
		}
		if !strings.Contains(lines[0], "agent-orc") {
			t.Errorf("%d rows, %d tasks: the header is not on the first line:\n%s", tc.height, tc.tasks, s.m.View())
		}
		if !s.contains("▸ ") {
			t.Errorf("%d rows, %d tasks: the selected row is not on screen", tc.height, tc.tasks)
		}
	}

	// Below what can be laid out at all, it says so rather than show a
	// screen with its top cut off.
	s := newScreen(t, 100, 5)
	s.load(numbered(5)...)
	if out := s.m.View(); !strings.Contains(out, "taller terminal") || len(strings.Split(out, "\n")) > 5 {
		t.Errorf("a 5 row terminal got:\n%s", out)
	}
}

// TestHeldLogIsRewrappedOnResize covers narrowing the terminal while reading
// back through a log. The held text stayed wrapped for the old width, so the
// viewport wrapped it again and cut the overflow, and one press of a scroll key
// then moved by more than one row.
func TestHeldLogIsRewrappedOnResize(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	var b strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "line %02d %s\n", i, strings.Repeat("x", 90))
	}
	s.showLog(strings.TrimRight(b.String(), "\n"))
	for i := 0; i < 10; i++ {
		s.press(tea.KeyShiftUp)
	}

	s.send(tea.WindowSizeMsg{Width: 50, Height: 30})
	before := strings.Split(s.m.renderLog(), "\n")
	for i, line := range before {
		if w := lipgloss.Width(line); w > 50 {
			t.Fatalf("after narrowing, row %d is %d wide", i, w)
		}
	}
	s.press(tea.KeyShiftDown)
	after := strings.Split(s.m.renderLog(), "\n")
	// One press, one row. Text still wrapped for the old width moves by as many
	// rows as each held line now takes.
	if before[1] != after[0] {
		t.Errorf("one scroll moved more than one row:\nwas second row %q\nnow first row %q", before[1], after[0])
	}
}

// TestFilterPromptHasACursor covers the prompt looking alive. Focus returns the
// command that starts the cursor blinking, and everything that is not a key
// press has to reach the text input for it to keep blinking.
func TestFilterPromptHasACursor(t *testing.T) {
	s := newScreen(t, 100, 30)
	s.load(tasks("A")...)
	s.showLog(numberedLines(1, 300))

	if cmd := s.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")}); cmd == nil {
		t.Error("'/' returned no command, so the cursor never starts blinking")
	}

	// While the filter has focus, other messages belong to it, not to the log.
	offset := s.m.vp.YOffset
	s.send(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if s.m.vp.YOffset != offset {
		t.Error("a message sent while filtering was handled by the log instead of the prompt")
	}
}

// TestTaskWhoseProcessIsGoneIsMarked covers a supervisor that was killed. The
// record still says running, and the dashboard drew it as working, with a
// growing elapsed time, while agent-orc status called the same task failed.
func TestTaskWhoseProcessIsGoneIsMarked(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(dir)
	live := mkTask("ALIVE-1", task.CLIClaude, state.StatusRunning, "agent-orc/alive-1")
	// agent-orc records a process group leader, and liveness is judged on the
	// group, so this test's own group is what stands in for a live task.
	live.PID = syscall.Getpgrp()
	dead := mkTask("DEAD-1", task.CLIClaude, state.StatusRunning, "agent-orc/dead-1")
	dead.PID = 0x7FFFFFFF // a pid nothing can have
	done := mkTask("DONE-1", task.CLIClaude, state.StatusDone, "agent-orc/done-1")
	for _, rec := range []state.Task{live, dead, done} {
		if err := store.Save(rec); err != nil {
			t.Fatal(err)
		}
	}

	_, gone, err := loadTasks(store)
	if err != nil {
		t.Fatal(err)
	}
	if !gone["DEAD-1"] {
		t.Error("a running record whose process is gone was not noticed")
	}
	for _, id := range []string{"ALIVE-1", "DONE-1"} {
		if gone[id] {
			t.Errorf("%s was marked as gone", id)
		}
	}

	// And it shows, in the row and in the detail line.
	s := newScreen(t, 120, 30)
	s.send(tasksMsg{tasks: []state.Task{dead}, gone: map[string]bool{"DEAD-1": true}})
	out := s.m.View()
	if !strings.Contains(out, "✗ running") {
		t.Errorf("the row does not mark the task as having no process:\n%s", out)
	}
	if !strings.Contains(out, "process is gone") {
		t.Errorf("the detail line does not say the process is gone:\n%s", out)
	}
}
