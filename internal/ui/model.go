// Package ui renders the live dashboard. It only ever reads snapshots, never
// the manager's internals, which is what keeps rendering race-free.
package ui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mustiko/auto-tunnel/internal/logbuf"
	"github.com/mustiko/auto-tunnel/internal/state"
)

// Actions is what the dashboard can ask the engine to do.
type Actions interface {
	// TogglePause flips whether a tunnel accepts new connections.
	TogglePause(key string) (paused, ok bool)
	// Rescan asks for an immediate discovery poll.
	Rescan()
}

// SnapshotMsg delivers a new view of the world to the dashboard.
type SnapshotMsg state.Snapshot

type sortMode int

const (
	sortByName sortMode = iota
	sortByLocalPort
	sortByTraffic
)

func (s sortMode) String() string {
	switch s {
	case sortByLocalPort:
		return "local port"
	case sortByTraffic:
		return "traffic"
	default:
		return "name"
	}
}

// Model is the bubbletea model backing the dashboard.
type Model struct {
	actions Actions
	logs    *logbuf.Buffer

	snap    state.Snapshot
	rows    []state.Tunnel // filtered and sorted view of snap.Tunnels
	width   int
	height  int
	cursor  int
	sort    sortMode
	filter  string
	editing bool // the filter prompt has focus
	showLog bool
	status  string // transient feedback for the last key pressed
}

// New creates a dashboard model.
func New(actions Actions, logs *logbuf.Buffer) Model {
	return Model{actions: actions, logs: logs, width: 100, height: 30}
}

// Init satisfies tea.Model; snapshots arrive from outside the program.
func (m Model) Init() tea.Cmd { return nil }

// Update handles snapshots, resizes, and key presses.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case SnapshotMsg:
		m.snap = state.Snapshot(msg)
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		if m.editing {
			return m.handleFilterKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.editing = false
		m.filter = ""
	case tea.KeyEnter:
		m.editing = false
	case tea.KeyBackspace:
		if m.filter != "" {
			m.filter = m.filter[:len(m.filter)-1]
		}
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(msg.Runes)
	}
	m.refresh()
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(0, len(m.rows)-1)
	case "r":
		m.actions.Rescan()
		m.status = "rescanning"
	case "p":
		m.status = m.togglePause()
	case "s":
		m.sort = (m.sort + 1) % 3
		m.status = "sorted by " + m.sort.String()
		m.refresh()
	case "l":
		m.showLog = !m.showLog
	case "/":
		m.editing = true
		m.filter = ""
	}
	return m, nil
}

func (m *Model) togglePause() string {
	row, ok := m.selected()
	if !ok {
		return ""
	}
	paused, ok := m.actions.TogglePause(row.Key)
	if !ok {
		return row.Name + " cannot be paused"
	}
	if paused {
		return "paused " + row.Name
	}
	return "resumed " + row.Name
}

// selected returns the highlighted row.
func (m Model) selected() (state.Tunnel, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return state.Tunnel{}, false
	}
	return m.rows[m.cursor], true
}

// refresh recomputes the visible rows, keeping the cursor on the same tunnel
// where possible so a background snapshot never moves the selection.
func (m *Model) refresh() {
	var selectedKey string
	if row, ok := m.selected(); ok {
		selectedKey = row.Key
	}

	rows := make([]state.Tunnel, 0, len(m.snap.Tunnels))
	for _, t := range m.snap.Tunnels {
		if matches(t, m.filter) {
			rows = append(rows, t)
		}
	}
	sortRows(rows, m.sort)
	m.rows = rows

	m.cursor = 0
	for i, row := range rows {
		if row.Key == selectedKey {
			m.cursor = i
			break
		}
	}
	if m.cursor >= len(rows) {
		m.cursor = max(0, len(rows)-1)
	}
}

func matches(t state.Tunnel, filter string) bool {
	if filter == "" {
		return true
	}
	needle := strings.ToLower(filter)
	for _, hay := range []string{t.Name, t.Image, t.RemoteTarget, string(t.State)} {
		if strings.Contains(strings.ToLower(hay), needle) {
			return true
		}
	}
	return false
}

func sortRows(rows []state.Tunnel, mode sortMode) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch mode {
		case sortByLocalPort:
			if a.LocalPort != b.LocalPort {
				return a.LocalPort < b.LocalPort
			}
		case sortByTraffic:
			at, bt := a.BytesIn+a.BytesOut, b.BytesIn+b.BytesOut
			if at != bt {
				return at > bt // busiest first
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ContainerPort < b.ContainerPort
	})
}
