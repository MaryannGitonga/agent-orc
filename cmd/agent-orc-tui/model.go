package main

import (
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// refreshEvery is how often the records and the open log are re-read. State is
// a handful of small JSON files, so polling costs less than a file watcher
// would and brings no dependency with it.
const refreshEvery = time.Second

// The size assumed when the terminal will not say, which is what a pseudo
// terminal with no window size attached reports.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// pane is which of the selected task's views is showing.
type pane int

const (
	paneSupervisor pane = iota
	paneAgent
	paneInfo
)

var paneNames = []string{"supervisor", "agent log", "info"}

type (
	tickMsg  time.Time
	tasksMsg struct {
		tasks []state.Task
		gone  map[string]bool // ids whose recorded process is no longer there
		err   error
	}
	logMsg struct {
		id   string
		pane pane
		text string
		// force replaces what is on screen even while it is being read, and
		// toEnd then jumps to the end of it: what r and G ask for.
		force, toEnd bool
	}
)

type model struct {
	layout paths.Layout
	store  *state.Store

	tasks []state.Task    // every task, newest first
	gone  map[string]bool // of those, the ones whose process has gone

	// The selection is the task, not the row. Rows are sorted newest first, so
	// a row number points at a different task the moment another is
	// dispatched or the filter hides one above it. cursor is only ever the
	// position selectedID currently occupies.
	selectedID string
	cursor     int
	tableTop   int // the first row the table shows

	// How the top of the screen is laid out, chosen by relayout to fit the
	// terminal: how many table rows, whether to drop the spacer lines, and
	// whether even that does not fit.
	capacity int
	compact  bool
	tooSmall bool

	pane   pane
	follow bool

	filter    textinput.Model
	filtering bool

	vp        viewport.Model
	ready     bool
	err       error
	width     int
	height    int
	shownID   string // what the viewport is showing, so a switch to anything
	shownPane pane   // else opens at the end rather than at an old offset
	shownText string // and what it said, to tell when there is newer output
	wrapWidth int    // the width it was wrapped for, so a resize can re-wrap
	newer     bool   // newer output was held back while you read

	// reading counts log reads in flight, and pending remembers a request that
	// arrived while one was running. One read at a time, whoever asked: the
	// timer skips its turn, and a key press is coalesced into a single read
	// once the running one returns. Holding a movement key would otherwise
	// start a read per repeat, each of them megabytes, all but the last thrown
	// away as stale.
	reading      int
	pending      bool
	pendingForce bool
	pendingToEnd bool
}

func newModel(layout paths.Layout) model {
	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "id, cli, branch or status"
	return model{
		layout:   layout,
		store:    state.NewStore(layout.State),
		follow:   true,
		filter:   filter,
		capacity: maxTableRows,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refresh re-reads every record.
func (m model) refresh() tea.Cmd {
	return func() tea.Msg {
		tasks, gone, err := loadTasks(m.store)
		return tasksMsg{tasks: tasks, gone: gone, err: err}
	}
}

// requestLog reads whichever view the selected task is showing. The pane and
// id travel with the result, because a slow read must not land in a viewport
// that has since moved to another task.
func (m *model) requestLog(force, toEnd bool) tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	if m.reading > 0 {
		m.pending = true
		m.pendingForce = m.pendingForce || force
		m.pendingToEnd = m.pendingToEnd || toEnd
		return nil
	}
	m.reading++
	p, layout := m.pane, m.layout
	return func() tea.Msg {
		var text string
		switch p {
		case paneAgent:
			text = agentLog(t)
		case paneInfo:
			text = taskInfo(t)
		default:
			text = supervisorLog(layout, t.ID)
		}
		return logMsg{id: t.ID, pane: p, text: text, force: force, toEnd: toEnd}
	}
}

// loadLog is the ordinary read a key or a new selection asks for.
func (m *model) loadLog() tea.Cmd { return m.requestLog(false, false) }

// pollLog is the timer's read, which is skipped rather than queued: a log slow
// enough to outlast a tick should be read as often as it can be, not
// continuously.
func (m *model) pollLog() tea.Cmd {
	if m.reading > 0 {
		return nil
	}
	return m.loadLog()
}

// drainPending makes the read that was asked for while another was running.
func (m *model) drainPending() tea.Cmd {
	if !m.pending {
		return nil
	}
	force, toEnd := m.pendingForce, m.pendingToEnd
	m.pending, m.pendingForce, m.pendingToEnd = false, false, false
	return m.requestLog(force, toEnd)
}

// visible is the tasks matching the current filter.
func (m model) visible() []state.Task {
	return filterTasks(m.tasks, m.filter.Value())
}

func (m model) selected() (state.Task, bool) {
	rows := m.visible()
	if len(rows) == 0 || m.cursor >= len(rows) {
		return state.Task{}, false
	}
	return rows[m.cursor], true
}

// Update handles a message, then lays the screen out again from what it now
// holds. Doing it after every message, rather than only on a resize, is what
// keeps the log sized to the table that is actually there: the first resize
// arrives before any task has been read, and the table, the error line and
// the detail block all change size later without the window changing at all.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.relayout()
	return next, cmd
}

func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if !m.ready {
			m.vp = viewport.New(m.contentWidth(), 1)
			m.ready = true
		}
		return m, nil

	case tickMsg:
		refresh, poll := m.refresh(), m.pollLog()
		return m, tea.Batch(refresh, poll, tick())

	case tasksMsg:
		m.err = msg.err
		if msg.err == nil {
			m.tasks, m.gone = msg.tasks, msg.gone
		}
		m.syncSelection()
		return m, nil

	case logMsg:
		if m.reading > 0 {
			m.reading--
		}
		next := m.drainPending()
		// Stale by the time it arrived: the selection or the pane moved on.
		if cur, ok := m.selected(); !ok || cur.ID != msg.id || m.pane != msg.pane {
			return m, next
		}
		atBottom := m.vp.AtBottom()
		switched := m.shownID != msg.id || m.shownPane != msg.pane
		live := m.follow && atBottom
		// Hold still while you read. The pane keeps a window of the last lines,
		// and swapping in a newer window under a fixed scroll position slides
		// the text up by however much arrived. So when you have scrolled up, or
		// paused, newer output waits until you ask for it.
		if !switched && !live && !msg.force {
			if msg.text != m.shownText {
				m.newer = true
			}
			return m, next
		}
		m.shownID, m.shownPane, m.shownText, m.newer = msg.id, msg.pane, msg.text, false
		m.wrapWidth = m.contentWidth()
		m.vp.SetContent(wrapTo(msg.text, m.wrapWidth))
		if switched || live || msg.toEnd {
			m.vp.GotoBottom()
		}
		return m, next

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	// While the filter has focus, anything that is not a key belongs to it:
	// the cursor's own blink messages arrive this way, and sending them to the
	// viewport instead is what leaves the prompt looking dead.
	if m.filtering {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m model) onKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.filtering {
		switch msg.String() {
		case "ctrl+c":
			// Quit means quit, whatever has focus. Handing it to the text box
			// instead would make you close the filter first to leave.
			return m, tea.Quit
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			m.syncSelection()
			cmd := m.loadLog()
			return m, cmd
		case "enter":
			m.filtering = false
			m.filter.Blur()
			cmd := m.loadLog()
			return m, cmd
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.syncSelection()
		read := m.loadLog()
		return m, tea.Batch(cmd, read)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
		cmd := m.loadLog()
		return m, cmd
	case "down", "j":
		m.move(1)
		cmd := m.loadLog()
		return m, cmd
	case "tab":
		m.pane = (m.pane + 1) % pane(len(paneNames))
		cmd := m.loadLog()
		return m, cmd
	case "shift+tab":
		m.pane = (m.pane + pane(len(paneNames)) - 1) % pane(len(paneNames))
		cmd := m.loadLog()
		return m, cmd
	case "f":
		m.follow = !m.follow
		if !m.follow {
			return m, nil
		}
		// Following again means showing what there is to follow: the held
		// window is older than the log, so the label would otherwise be a
		// claim the screen does not back up until the next tick.
		m.vp.GotoBottom()
		cmd := m.requestLog(true, true)
		return m, cmd
	// One line at a time. The plain arrows choose a task, and a log is read far
	// more often than the selection changes, so the shifted arrows scroll it
	// rather than the other way round.
	case "shift+up":
		m.vp.LineUp(1)
		return m, nil
	case "shift+down":
		m.vp.LineDown(1)
		return m, nil
	case "g":
		m.vp.GotoTop()
		return m, nil
	case "G":
		// The end of the log as it is now, not of what was on screen.
		m.vp.GotoBottom()
		cmd := m.requestLog(true, true)
		return m, cmd
	case "r":
		refresh, read := m.refresh(), m.requestLog(true, false)
		return m, tea.Batch(refresh, read)
	case "/":
		m.filtering = true
		// Focus returns the command that starts the cursor blinking. Dropping
		// it leaves a prompt with no cursor, which reads as an unresponsive box.
		return m, m.filter.Focus()
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// move walks the selection, stopping at both ends rather than wrapping: a list
// that jumps from the last row to the first is easy to lose your place in.
func (m *model) move(delta int) {
	rows := m.visible()
	if len(rows) == 0 {
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(rows)-1)
	m.selectedID = rows[m.cursor].ID
	m.scrollTable(len(rows))
}

// syncSelection finds the selected task again after the rows changed under
// it. It stays on the same task wherever that task has moved to, and only
// falls back to a neighbouring row when the task is gone or filtered out.
func (m *model) syncSelection() {
	rows := m.visible()
	if len(rows) == 0 {
		m.cursor, m.selectedID, m.tableTop = 0, "", 0
		return
	}
	m.cursor = clamp(m.cursor, 0, len(rows)-1)
	for i, t := range rows {
		if t.ID == m.selectedID {
			m.cursor = i
			break
		}
	}
	m.selectedID = rows[m.cursor].ID
	m.scrollTable(len(rows))
}

// scrollTable moves the table's window just far enough to keep the selected
// row on screen.
func (m *model) scrollTable(rows int) {
	limit := max(1, m.capacity)
	if m.cursor < m.tableTop {
		m.tableTop = m.cursor
	}
	if m.cursor >= m.tableTop+limit {
		m.tableTop = m.cursor - limit + 1
	}
	m.tableTop = clamp(m.tableTop, 0, max(0, rows-limit))
}

// fitTable chooses the most table rows that still leave the header, the footer
// and a line of log on screen. A screen taller than the terminal loses its top
// lines, the header first, so the table gives way instead: row by row down to
// one, then without the spacer lines, and only then does it give up and say the
// terminal is too small.
func (m *model) fitTable() {
	height := m.contentHeight()
	rows := len(m.visible())
	preferred := clamp(height/3, 1, maxTableRows)
	m.tooSmall = false
	for _, compact := range []bool{false, true} {
		m.compact = compact
		for c := preferred; c >= 1; c-- {
			m.capacity = c
			m.scrollTable(rows)
			if lipgloss.Height(m.renderTop())+2 <= height { // a log line and the footer
				return
			}
		}
	}
	m.tooSmall = true
}

// relayout sizes the log to whatever the rest of the screen leaves, measured
// from what is actually rendered rather than predicted from a row count.
func (m *model) relayout() {
	if !m.ready {
		return
	}
	m.fitTable()
	used := lipgloss.Height(m.renderTop()) + 1 // plus the footer's line
	wasAtBottom := m.vp.AtBottom()
	// The prompt has to know its own width or it never scrolls, and what you
	// are typing, cursor included, runs off the right edge and is cut.
	m.filter.Width = max(10, m.contentWidth()-len(m.filter.Prompt)-2)
	m.vp.Width = m.contentWidth()
	m.vp.Height = max(1, m.contentHeight()-used)
	// Held content was wrapped for the old width. The viewport would re-wrap it
	// itself and then cut the overflow, so a narrowed terminal loses lines from
	// a log that is being read rather than followed.
	if m.shownText != "" && m.wrapWidth != m.vp.Width {
		m.wrapWidth = m.vp.Width
		m.vp.SetContent(wrapTo(m.shownText, m.wrapWidth))
	}
	if m.follow && wasAtBottom {
		m.vp.GotoBottom()
	} else {
		// A taller log can leave the offset past the new end.
		m.vp.SetYOffset(m.vp.YOffset)
	}
}

// A terminal that reports no size is not a terminal one column wide: some
// pseudo-terminals answer 0x0, and laying out for that makes the screen
// unreadable. Fall back to the conventional default instead.
func (m model) contentWidth() int {
	if m.width <= 0 {
		return defaultWidth
	}
	return m.width
}

func (m model) contentHeight() int {
	if m.height <= 0 {
		return defaultHeight
	}
	return m.height
}

// wrapTo breaks long lines before they reach the log. The viewport wraps lines
// itself and then cuts whatever no longer fits from the bottom, which in a log
// being followed is exactly the newest line. Wrapping first makes every line it
// counts a line on screen, so the end of the log is the end of the screen.
func wrapTo(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

func clamp(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}
