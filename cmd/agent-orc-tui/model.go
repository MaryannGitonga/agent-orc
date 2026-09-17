package main

import (
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

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

	tasks  []state.Task // every task, newest first
	cursor int          // index into the filtered view
	pane   pane
	follow bool

	filter    textinput.Model
	filtering bool

	vp      viewport.Model
	ready   bool
	err     error
	width   int
	height  int
	shownID string // the task the viewport's content belongs to
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutPanes()
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refresh(), m.loadLog(), tick())

	case tasksMsg:
		m.err = msg.err
		if msg.err == nil {
			m.tasks = msg.tasks
		}
		m.clampCursor()
		return m, nil

	case logMsg:
		// Stale by the time it arrived: the selection or the pane moved on.
		if cur, ok := m.selected(); !ok || cur.ID != msg.id || m.pane != msg.pane {
			return m, nil
		}
		atBottom := m.vp.AtBottom()
		changed := m.shownID != msg.id
		m.shownID = msg.id
		m.vp.SetContent(msg.text)
		if changed || (m.follow && atBottom) {
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

func (m model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			m.clampCursor()
			return m, m.loadLog()
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, m.loadLog()
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.clampCursor()
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
	n := len(m.visible())
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, n-1)
}

func (m *model) clampCursor() {
	n := len(m.visible())
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor = clamp(m.cursor, 0, n-1)
}

// layoutPanes gives the log whatever is left once the table and the chrome
// around it have taken what they need.
func (m *model) layoutPanes() {
	rows := len(m.visible())
	if rows > maxTableRows {
		rows = maxTableRows
	}
	// A terminal that reports no size is not a terminal one column wide: some
	// pseudo-terminals answer 0x0, and wrapping the log at a couple of columns
	// makes it unreadable. Fall back to the conventional default instead.
	width, height := m.width, m.height
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	h := height - (chromeHeight + rows)
	if h < 3 {
		h = 3
	}
	w := width
	if !m.ready {
		m.vp = viewport.New(w, h)
		m.ready = true
		return
	}
	m.vp.Width, m.vp.Height = w, h
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
