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
		err   error
	}
	logMsg struct {
		id   string
		pane pane
		text string
	}
)

type model struct {
	layout paths.Layout
	store  *state.Store

	tasks []state.Task // every task, newest first

	// The selection is the task, not the row. Rows are sorted newest first, so
	// a row number points at a different task the moment another is
	// dispatched or the filter hides one above it. cursor is only ever the
	// position selectedID currently occupies.
	selectedID string
	cursor     int
	tableTop   int // the first row the table shows

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
}

func newModel(layout paths.Layout) model {
	filter := textinput.New()
	filter.Prompt = "/"
	filter.Placeholder = "id, cli, branch or status"
	return model{
		layout: layout,
		store:  state.NewStore(layout.State),
		follow: true,
		filter: filter,
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
		tasks, err := loadTasks(m.store)
		return tasksMsg{tasks: tasks, err: err}
	}
}

// loadLog reads whichever view the selected task is showing. The pane and id
// travel with the result, because a slow read must not land in a viewport that
// has since moved to another task.
func (m model) loadLog() tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	id, p, layout := t.ID, m.pane, m.layout
	return func() tea.Msg {
		var text string
		switch p {
		case paneAgent:
			text = agentLog(layout, id)
		case paneInfo:
			text = taskInfo(t)
		default:
			text = supervisorLog(layout, id)
		}
		return logMsg{id: id, pane: p, text: text}
	}
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
		return m, tea.Batch(m.refresh(), m.loadLog(), tick())

	case tasksMsg:
		m.err = msg.err
		if msg.err == nil {
			m.tasks = msg.tasks
		}
		m.syncSelection()
		return m, nil

	case logMsg:
		// Stale by the time it arrived: the selection or the pane moved on.
		if cur, ok := m.selected(); !ok || cur.ID != msg.id || m.pane != msg.pane {
			return m, nil
		}
		atBottom := m.vp.AtBottom()
		switched := m.shownID != msg.id || m.shownPane != msg.pane
		m.shownID, m.shownPane = msg.id, msg.pane
		m.vp.SetContent(wrapTo(msg.text, m.contentWidth()))
		if switched || (m.follow && atBottom) {
			m.vp.GotoBottom()
		}
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
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
			return m, m.loadLog()
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, m.loadLog()
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.syncSelection()
		return m, tea.Batch(cmd, m.loadLog())
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
		return m, m.loadLog()
	case "down", "j":
		m.move(1)
		return m, m.loadLog()
	case "tab":
		m.pane = (m.pane + 1) % pane(len(paneNames))
		return m, m.loadLog()
	case "shift+tab":
		m.pane = (m.pane + pane(len(paneNames)) - 1) % pane(len(paneNames))
		return m, m.loadLog()
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.vp.GotoBottom()
		}
		return m, nil
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
		m.vp.GotoBottom()
		return m, nil
	case "r":
		return m, tea.Batch(m.refresh(), m.loadLog())
	case "/":
		m.filtering = true
		m.filter.Focus()
		return m, nil
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
	limit := m.tableCapacity()
	if m.cursor < m.tableTop {
		m.tableTop = m.cursor
	}
	if m.cursor >= m.tableTop+limit {
		m.tableTop = m.cursor - limit + 1
	}
	m.tableTop = clamp(m.tableTop, 0, max(0, rows-limit))
}

// tableCapacity is how many rows the table may show before it scrolls. It
// shrinks on a short terminal, so the table cannot squeeze the log out.
func (m model) tableCapacity() int {
	return clamp(m.contentHeight()/3, 3, maxTableRows)
}

// relayout sizes the log to whatever the rest of the screen leaves, measured
// from what is actually rendered rather than predicted from a row count.
func (m *model) relayout() {
	if !m.ready {
		return
	}
	m.scrollTable(len(m.visible()))
	used := lipgloss.Height(m.renderTop()) + 1 // plus the footer's line
	wasAtBottom := m.vp.AtBottom()
	m.vp.Width = m.contentWidth()
	m.vp.Height = max(1, m.contentHeight()-used)
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
