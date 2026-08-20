// Package ui renders the live dashboard. It only ever reads snapshots, never
// the manager's internals, which is what keeps rendering race-free.
package ui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beyto1974/auto-tunnel/internal/logbuf"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

// Actions is what the dashboard can ask the engine to do.
type Actions interface {
	// TogglePause flips whether a tunnel accepts new connections.
	TogglePause(key string) (paused, ok bool)
	// SetEnabled starts or stops forwarding one discovered port, reporting
	// whether the key was known.
	SetEnabled(key string, on bool) bool
	// SetEnabledAll applies one choice to several ports, returning how many it
	// changed.
	SetEnabledAll(keys []string, on bool) int
	// OpenURL hands a forwarded service's URL to the desktop's browser.
	OpenURL(url string) error
	// Prefs returns the dashboard settings remembered from earlier runs.
	Prefs() Prefs
	// SavePrefs remembers them for the next one.
	SavePrefs(Prefs)
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
	sortByRemote
	sortModes // how many there are; keep last
)

func (s sortMode) String() string {
	switch s {
	case sortByLocalPort:
		return "local port"
	case sortByTraffic:
		return "traffic"
	case sortByRemote:
		return "remote/url"
	default:
		return "name"
	}
}

// parseSortMode reads back what String wrote, falling back to the default for
// anything it does not recognise — a stale or hand-edited preference file must
// not be a startup failure.
func parseSortMode(s string) sortMode {
	for mode := sortMode(0); mode < sortModes; mode++ {
		if mode.String() == s {
			return mode
		}
	}
	return sortByName
}

// Prefs are the dashboard settings that outlive a single run.
type Prefs struct {
	// Sort is a sortMode as String renders it.
	Sort string `json:"sort"`
}

// sourceFilters are cycled by the "t" key. The empty string shows everything;
// the rest match state.Tunnel.Source exactly.
var sourceFilters = []string{"", "docker", "host", "static"}

func sourceFilterName(s string) string {
	if s == "" {
		return "all"
	}
	return s
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
	source  int  // index into sourceFilters
	onlyOn  bool // hide the rows that are listed but not forwarded
	editing bool // the filter prompt has focus
	showLog bool
	status  string // transient feedback for the last key pressed
}

// New creates a dashboard model, picking up where the last run left off.
func New(actions Actions, logs *logbuf.Buffer) Model {
	return Model{
		actions: actions,
		logs:    logs,
		width:   100,
		height:  30,
		sort:    parseSortMode(actions.Prefs().Sort),
	}
}

// Init satisfies tea.Model; snapshots arrive from outside the program.
func (m Model) Init() tea.Cmd { return nil }

// Update handles snapshots, resizes, and key presses.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that reports no size (a pty with none set, for instance)
		// would otherwise collapse the table to a single row.
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
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
	case "enter", " ":
		m.status = m.toggleEnabled()
	case "a":
		m.status = m.toggleAllVisible()
	case "o":
		m.status = m.openSelected()
	case "f":
		m.onlyOn = !m.onlyOn
		m.status = "showing " + m.scopeName()
		m.refresh()
	case "t":
		m.source = (m.source + 1) % len(sourceFilters)
		m.status = "showing " + sourceFilterName(sourceFilters[m.source])
		m.refresh()
	case "s":
		m.sort = (m.sort + 1) % sortModes
		m.status = "sorted by " + m.sort.String()
		m.actions.SavePrefs(Prefs{Sort: m.sort.String()})
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

// toggleEnabled starts or stops forwarding the selected row.
func (m *Model) toggleEnabled() string {
	row, ok := m.selected()
	if !ok {
		return ""
	}
	if !m.actions.SetEnabled(row.Key, !row.Enabled) {
		return row.Name + " cannot be forwarded"
	}
	if row.Enabled {
		return "stopped " + row.Name
	}
	return "forwarding " + row.Name
}

// toggleAllVisible switches every row the user can currently see. Anything not
// yet forwarded turns on; only once they are all on does the key turn them off,
// so the common case — "give me all of these" — is a single press.
func (m *Model) toggleAllVisible() string {
	if len(m.rows) == 0 {
		return ""
	}
	on := false
	keys := make([]string, 0, len(m.rows))
	for _, row := range m.rows {
		keys = append(keys, row.Key)
		if !row.Enabled {
			on = true
		}
	}
	changed := m.actions.SetEnabledAll(keys, on)
	verb := "stopped"
	if on {
		verb = "forwarding"
	}
	return fmt.Sprintf("%s %d of %d shown", verb, changed, len(keys))
}

// openSelected opens the selected row in a browser. Only a row a probe has
// identified as a web service has somewhere to open.
func (m *Model) openSelected() string {
	row, ok := m.selected()
	if !ok {
		return ""
	}
	if row.URL == "" {
		return row.Name + " is not a web service"
	}
	if err := m.actions.OpenURL(row.URL); err != nil {
		return "could not open " + row.URL + ": " + err.Error()
	}
	return "opening " + row.URL
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

	source := sourceFilters[m.source]
	rows := make([]state.Tunnel, 0, len(m.snap.Tunnels))
	for _, t := range m.snap.Tunnels {
		if m.onlyOn && !t.Enabled {
			continue
		}
		if fromSource(t, source) && matches(t, m.filter) {
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

// scopeName names what the "f" key is currently showing.
func (m Model) scopeName() string {
	if m.onlyOn {
		return "forwarded only"
	}
	return "everything discovered"
}

// remoteKey is the text the REMOTE / URL column renders for a row.
func remoteKey(t state.Tunnel) string {
	if t.URL != "" {
		return t.URL
	}
	return t.RemoteTarget
}

// fromSource applies the source filter. A row with no source at all is treated
// as docker, matching how discovery reads its own zero value.
func fromSource(t state.Tunnel, source string) bool {
	if source == "" {
		return true
	}
	if t.Source == "" {
		return source == "docker"
	}
	return t.Source == source
}

func matches(t state.Tunnel, filter string) bool {
	if filter == "" {
		return true
	}
	needle := strings.ToLower(filter)
	for _, hay := range []string{t.Name, t.Image, t.RemoteTarget, string(t.State), t.Source} {
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
		case sortByRemote:
			// Sort on what the column actually shows, so the order matches what
			// the eye reads. URLs sort together, after the bare host:port rows.
			if ar, br := remoteKey(a), remoteKey(b); ar != br {
				return ar < br
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ContainerPort < b.ContainerPort
	})
}
