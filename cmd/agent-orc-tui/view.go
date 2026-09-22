package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/MaryannGitonga/agent-orc/internal/orc"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/version"
)

// maxTableRows is the most rows the table shows before it scrolls, however
// tall the terminal.
const maxTableRows = 12

var (
	dim       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	bold      = lipgloss.NewStyle().Bold(true)
	selected  = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	tabOn     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	statusHue = map[state.Status]lipgloss.Style{
		state.StatusPending:         lipgloss.NewStyle().Foreground(lipgloss.Color("117")),
		state.StatusRunning:         lipgloss.NewStyle().Foreground(lipgloss.Color("78")),
		state.StatusVerifying:       lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		state.StatusReviewing:       lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		state.StatusPublishing:      lipgloss.NewStyle().Foreground(lipgloss.Color("111")),
		state.StatusDone:            lipgloss.NewStyle().Foreground(lipgloss.Color("78")),
		state.StatusReviewed:        lipgloss.NewStyle().Foreground(lipgloss.Color("78")),
		state.StatusStopped:         lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		state.StatusFailed:          errStyle,
		state.StatusPublishFailed:   errStyle,
		state.StatusReviewFailed:    errStyle,
		state.StatusPolicyViolation: errStyle,
	}
)

// mark is the symbol shown beside a status. Shape carries the state as well as
// colour does, so the table still reads on a terminal without colour.
func mark(s state.Status) string {
	switch s {
	case state.StatusPending:
		return "○"
	case state.StatusRunning:
		return "●"
	case state.StatusVerifying, state.StatusReviewing:
		return "◐"
	case state.StatusPublishing:
		return "◑"
	case state.StatusDone, state.StatusReviewed:
		return "✓"
	case state.StatusStopped:
		return "■"
	default:
		return "✗"
	}
}

func hue(s state.Status) lipgloss.Style {
	if style, ok := statusHue[s]; ok {
		return style
	}
	return lipgloss.NewStyle()
}

// filterTasks keeps the tasks matching q, which is matched against the fields
// someone would actually type: the id, the CLI, the branch and the status.
func filterTasks(tasks []state.Task, q string) []state.Task {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return tasks
	}
	out := make([]state.Task, 0, len(tasks))
	for _, t := range tasks {
		haystack := strings.ToLower(strings.Join([]string{
			t.ID, string(t.CLI), t.Branch, string(t.Status),
		}, " "))
		if strings.Contains(haystack, q) {
			out = append(out, t)
		}
	}
	return out
}

// countSummary is the header's tally, in the order tasks move through.
func countSummary(tasks []state.Task) []string {
	counts := map[state.Status]int{}
	for _, t := range tasks {
		counts[t.Status]++
	}
	order := make([]state.Status, 0, len(counts))
	for s := range counts {
		order = append(order, s)
	}
	sort.Slice(order, func(i, j int) bool { return string(order[i]) < string(order[j]) })
	out := make([]string, 0, len(order))
	for _, s := range order {
		out = append(out, hue(s).Render(fmt.Sprintf("%d %s", counts[s], s)))
	}
	return out
}

// pad renders s in a column n wide, cutting it with an ellipsis when it does
// not fit so the columns after it stay where they are.
func pad(s string, n int) string {
	r := []rune(s)
	switch {
	case len(r) == n:
		return s
	case len(r) < n:
		return s + strings.Repeat(" ", n-len(r))
	case n <= 1:
		return string(r[:n])
	default:
		return string(r[:n-1]) + "…"
	}
}

// columns is which of the table's columns fit, and how much is left for the
// branch. The model belongs beside the CLI, as it is in `agent-orc status`:
// "claude" says who ran it, but "claude-opus-5" is what it cost. It is the
// first column to go on a narrow terminal, because it is also the one the
// detail line below repeats.
type columns struct {
	model  bool
	branch int
}

// The fixed part of a row: each column plus the space after it, and the two
// columns for the cursor.
const (
	colsWithoutModel = 2 + 15 + 9 + 13 + 16 + 4 + 9
	modelColumn      = 17
	minBranch        = 6
)

func columnsFor(width int) columns {
	if width <= 0 {
		width = defaultWidth
	}
	if rest := width - (colsWithoutModel + modelColumn); rest >= 10 {
		return columns{model: true, branch: rest}
	}
	rest := width - colsWithoutModel
	if rest < minBranch {
		rest = minBranch
	}
	return columns{branch: rest}
}

func (c columns) header() string {
	out := pad("ID", 14) + " " + pad("CLI", 8) + " "
	if c.model {
		out += pad("MODEL", 16) + " "
	}
	return out + pad("STATUS", 12) + " " + pad("SPEND", 15) + " " +
		pad("RDS", 3) + " " + pad("ELAPSED", 8) + " " + pad("BRANCH", c.branch)
}

// row renders one task in style bg, which is the highlight for the selected row
// and nothing for the rest.
//
// The status cell has a colour of its own, and a styled string ends by
// resetting everything, background included. Wrapping the finished row in
// the highlight would therefore lose it from the status onwards, so each cell
// is styled on its own, and the status inherits the background.
func (c columns) row(t state.Task, bg lipgloss.Style) string {
	lead := pad(t.ID, 14) + " " + pad(string(t.CLI), 8) + " "
	if c.model {
		model := t.Model
		if model == "" {
			// The CLI's own default, which agent-orc never had to name.
			model = "-"
		}
		lead += pad(model, 16) + " "
	}
	status := pad(mark(t.Status)+" "+string(t.Status), 12)
	rest := " " + pad(orc.Spend(t), 15) + " " + pad(orc.Rounds(t), 3) + " " +
		pad(orc.Elapsed(t), 8) + " " + pad(t.Branch, c.branch)
	return bg.Render(lead) + hue(t.Status).Inherit(bg).Render(status) + bg.Render(rest)
}

// taskInfo is the info pane: the facts that do not fit on the detail line.
func taskInfo(t state.Task) string {
	var b strings.Builder
	add := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%-10s %s\n", label, value)
		}
	}
	add("id", t.ID)
	add("status", string(t.Status))
	add("cli", string(t.CLI))
	add("model", t.Model)
	add("repo", t.Repo)
	add("branch", t.Branch)
	add("base", t.BaseBranch)
	add("worktree", t.Worktree)
	add("tests", t.TestCommand)
	add("session", t.SessionID)
	add("pr", t.PRURL)
	if t.TestRuns > 0 {
		outcome := "not finished"
		if t.TestsPassed != nil {
			outcome = map[bool]string{true: "passed", false: "failed"}[*t.TestsPassed]
		}
		add("test runs", fmt.Sprintf("%d, last %s", t.TestRuns, outcome))
	}
	if t.RewrittenCommits > 0 {
		add("sanitized", fmt.Sprintf("%d commit message(s) rewritten", t.RewrittenCommits))
	}
	add("budget note", t.BudgetNote)
	add("error", t.Error)
	return strings.TrimRight(b.String(), "\n")
}

// detail is the two lines under the table describing the selected task.
func detail(t state.Task) string {
	phase := string(t.Status)
	switch t.Status {
	case state.StatusVerifying:
		if t.TestRuns > 0 {
			phase = fmt.Sprintf("verifying, test attempt %d", t.TestRuns)
		}
	case state.StatusReviewing:
		phase = fmt.Sprintf("reviewing, round %d", t.ReviewRound+1)
	}
	pid := "-"
	if t.PID != 0 {
		pid = fmt.Sprintf("%d", t.PID)
	}
	first := fmt.Sprintf("%s  %s", bold.Render(t.ID), hue(t.Status).Render(phase))
	second := fmt.Sprintf("%s %s   %s %s   %s %s",
		dim.Render("branch"), t.Branch,
		dim.Render("spend"), orc.Spend(t),
		dim.Render("pid"), pid)
	if t.PRURL != "" {
		second += "   " + dim.Render("pr") + " " + t.PRURL
	}
	return first + "\n" + second
}

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}
	return m.renderTop() + "\n" + m.vp.View() + "\n" + m.renderFooter()
}

// renderTop is everything above the log. The layout measures this very string
// to size the log, so the two cannot disagree about how tall it is.
func (m model) renderTop() string {
	width := m.contentWidth()
	rows := m.visible()
	var lines []string
	add := func(s string) { lines = append(lines, fit(s, width)) }

	head := bold.Render("agent-orc")
	if summary := countSummary(m.tasks); len(summary) > 0 {
		head += "   " + strings.Join(summary, "  ")
	}
	add(head + "   " + dim.Render(version.String()))
	add("")
	if m.err != nil {
		add(errStyle.Render("could not read state: " + m.err.Error()))
	}

	cols := columnsFor(width)
	add(dim.Render("  " + cols.header()))
	if len(rows) == 0 {
		add(dim.Render("  no tasks; dispatch one with 'agent-orc run'"))
	}
	last := min(len(rows), m.tableTop+m.tableCapacity())
	for i := m.tableTop; i < last; i++ {
		cursor, bg := "  ", lipgloss.NewStyle()
		if i == m.cursor {
			cursor, bg = "▸ ", selected
		}
		add(cursor + cols.row(rows[i], bg))
	}
	// Only when the table is scrolling, so a short list has no dead line.
	if len(rows) > m.tableCapacity() {
		add(dim.Render(fmt.Sprintf("  rows %d to %d of %d", m.tableTop+1, last, len(rows))))
	}
	add("")

	t, ok := m.selected()
	if !ok {
		add("")
		add("")
		add("")
		return strings.Join(lines, "\n")
	}
	for _, l := range strings.Split(detail(t), "\n") {
		add(l)
	}
	tabs := make([]string, len(paneNames))
	for i, name := range paneNames {
		if pane(i) == m.pane {
			tabs[i] = tabOn.Render("[ " + name + " ]")
			continue
		}
		tabs[i] = dim.Render("  " + name + "  ")
	}
	add(strings.Join(tabs, " "))
	return strings.Join(lines, "\n")
}

func (m model) renderFooter() string {
	if m.filtering {
		return fit(m.filter.View(), m.contentWidth())
	}
	var notes []string
	if m.follow {
		notes = append(notes, tabOn.Render("following"))
	}
	if q := m.filter.Value(); q != "" {
		notes = append(notes, dim.Render(fmt.Sprintf("filter %q (%d)", q, len(m.visible()))))
	}
	return footer(m.width, notes)
}

// fit cuts a line to the terminal's width. A line that wraps takes a second
// row the layout never counted, and pushes the top of the screen out of view.
func fit(s string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// footerKeys is every key worth advertising, most useful first. On a narrow
// terminal the list is cut from the end, and quit is always kept.
var footerKeys = []string{
	"↑↓ select", "shift+↑↓ scroll", "pgup/pgdn page", "tab pane",
	"/ filter", "f follow", "g/G top/end", "r refresh",
}

const footerGap = "   "

// footer renders the key hints and any status notes on a single line. It has
// to stay one line: the log below is sized on that assumption, and a footer
// that wraps pushes the whole screen down by a row.
func footer(width int, notes []string) string {
	if width <= 0 {
		width = defaultWidth
	}
	used := len([]rune("q quit"))
	for _, n := range notes {
		used += len(footerGap) + lipgloss.Width(n)
	}
	var keys []string
	for _, k := range footerKeys {
		if used+len([]rune(k))+len(footerGap) > width {
			break
		}
		keys = append(keys, k)
		used += len([]rune(k)) + len(footerGap)
	}
	out := dim.Render(strings.Join(append(keys, "q quit"), footerGap))
	for _, n := range notes {
		out += footerGap + n
	}
	return out
}
