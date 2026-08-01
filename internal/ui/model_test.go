package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mustiko/auto-tunnel/internal/logbuf"
	"github.com/mustiko/auto-tunnel/internal/sshconn"
	"github.com/mustiko/auto-tunnel/internal/state"
)

type fakeActions struct {
	paused   map[string]bool
	rescans  int
	lastKey  string
	unknown  bool // report every key as not pausable
	toggling bool
}

func newFakeActions() *fakeActions {
	return &fakeActions{paused: map[string]bool{}}
}

func (f *fakeActions) TogglePause(key string) (bool, bool) {
	f.lastKey = key
	f.toggling = true
	if f.unknown {
		return false, false
	}
	f.paused[key] = !f.paused[key]
	return f.paused[key], true
}

func (f *fakeActions) Rescan() { f.rescans++ }

func testSnapshot() state.Snapshot {
	now := time.Now()
	return state.Snapshot{
		Target:     "deploy@10.0.0.5:22",
		Started:    now.Add(-12 * time.Minute),
		LastScan:   now,
		NextScan:   now.Add(4 * time.Second),
		Containers: 3,
		SSH: sshconn.Status{
			State: sshconn.StateConnected,
			RTT:   4 * time.Millisecond,
			Since: now.Add(-12 * time.Minute),
		},
		Tunnels: []state.Tunnel{
			{
				Key: "web:80/tcp", Name: "web", Image: "nginx:alpine", Proto: "tcp",
				ContainerPort: 80, RemotePort: 8080, RemoteTarget: "127.0.0.1:8080",
				LocalPort: 8080, Published: true, State: state.TunnelActive,
				ActiveConns: 2, TotalConns: 9, BytesIn: 2048, BytesOut: 512,
			},
			{
				Key: "db:5432/tcp", Name: "db", Image: "postgres:16", Proto: "tcp",
				ContainerPort: 5432, RemotePort: 5432, RemoteTarget: "127.0.0.1:5432",
				LocalPort: 25432, Published: true, State: state.TunnelListening,
			},
			{
				Key: "dns:53/udp", Name: "dns", Image: "coredns:latest", Proto: "udp",
				ContainerPort: 53, RemotePort: 53, RemoteTarget: "127.0.0.1:53",
				Published: true, State: state.TunnelUnsupported,
				LastError: "ssh forwards tcp only",
			},
		},
	}
}

// newTestModel returns a sized model already holding a snapshot.
func newTestModel(t *testing.T, actions Actions) Model {
	t.Helper()
	m := New(actions, logbuf.New(50))
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	updated, _ = updated.(Model).Update(SnapshotMsg(testSnapshot()))
	return updated.(Model)
}

func press(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "up", "down", "enter", "esc":
		msg = tea.KeyMsg{Type: map[string]tea.KeyType{
			"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter, "esc": tea.KeyEsc,
		}[key]}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	updated, cmd := m.Update(msg)
	return updated.(Model), cmd
}

func TestViewShowsHeaderAndEveryTunnel(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	view := m.View()

	for _, want := range []string{
		"auto-tunnel", "deploy@10.0.0.5:22", "connected", "3 container(s)",
		"web", "nginx:alpine", "8080", "127.0.0.1:8080", "ACTIVE",
		"db", "25432", "LISTENING",
		"dns", "UNSUPPORTED",
		"2.0KB", "512B",
		"q quit",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q\n---\n%s", want, view)
		}
	}
}

func TestViewBannerSurfacesDiscoveryFailure(t *testing.T) {
	m := New(newFakeActions(), logbuf.New(10))
	snap := testSnapshot()
	snap.DiscoveryError = "permission denied while trying to connect to the Docker daemon"
	updated, _ := m.Update(SnapshotMsg(snap))
	view := updated.(Model).View()

	if !strings.Contains(view, "docker discovery failing") ||
		!strings.Contains(view, "permission denied") {
		t.Errorf("discovery failure is not surfaced\n---\n%s", view)
	}
}

func TestViewBannerSurfacesSSHOutage(t *testing.T) {
	m := New(newFakeActions(), logbuf.New(10))
	snap := testSnapshot()
	snap.SSH = sshconn.Status{
		State:     sshconn.StateReconnecting,
		Attempts:  3,
		LastError: "dial tcp: connection refused",
	}
	updated, _ := m.Update(SnapshotMsg(snap))
	view := updated.(Model).View()

	if !strings.Contains(view, "reconnecting") || !strings.Contains(view, "attempt 3") {
		t.Errorf("reconnect state is not shown\n---\n%s", view)
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("ssh error is not shown\n---\n%s", view)
	}
}

func TestPauseTogglesTheSelectedTunnel(t *testing.T) {
	actions := newFakeActions()
	m := newTestModel(t, actions)

	// Rows sort by name, so the first row is "db".
	m, _ = press(t, m, "p")
	if actions.lastKey != "db:5432/tcp" {
		t.Errorf("paused %q, want the selected db tunnel", actions.lastKey)
	}
	if !strings.Contains(m.View(), "paused db") {
		t.Errorf("no feedback after pausing\n---\n%s", m.View())
	}

	m, _ = press(t, m, "down")
	m, _ = press(t, m, "p")
	if actions.lastKey != "dns:53/udp" {
		t.Errorf("paused %q after moving down, want the dns row", actions.lastKey)
	}
}

func TestRescanKeyAsksTheEngineForAPoll(t *testing.T) {
	actions := newFakeActions()
	m := newTestModel(t, actions)

	m, _ = press(t, m, "r")
	if actions.rescans != 1 {
		t.Errorf("Rescan called %d times, want 1", actions.rescans)
	}
	if !strings.Contains(m.View(), "rescanning") {
		t.Error("no feedback after requesting a rescan")
	}
}

func TestFilterNarrowsRowsAndEscClearsIt(t *testing.T) {
	m := newTestModel(t, newFakeActions())

	m, _ = press(t, m, "/")
	m, _ = press(t, m, "w")
	m, _ = press(t, m, "e")
	m, _ = press(t, m, "b")
	m, _ = press(t, m, "enter")

	if len(m.rows) != 1 || m.rows[0].Name != "web" {
		t.Fatalf("filter kept %+v, want only web", m.rows)
	}
	view := m.View()
	if strings.Contains(view, "postgres:16") {
		t.Errorf("filtered-out row is still rendered\n---\n%s", view)
	}

	m, _ = press(t, m, "/")
	m, _ = press(t, m, "esc")
	if len(m.rows) != 3 {
		t.Errorf("esc left %d rows, want the filter cleared back to 3", len(m.rows))
	}
}

func TestSortCyclesBetweenModes(t *testing.T) {
	m := newTestModel(t, newFakeActions())

	if got := m.rows[0].Name; got != "db" {
		t.Fatalf("default sort put %q first, want db (by name)", got)
	}

	m, _ = press(t, m, "s") // by local port: the UDP row has none
	if got := m.rows[0].Name; got != "dns" {
		t.Errorf("by local port put %q first, want dns (port 0)", got)
	}

	m, _ = press(t, m, "s") // by traffic: web is the only row with bytes
	if got := m.rows[0].Name; got != "web" {
		t.Errorf("by traffic put %q first, want web", got)
	}

	m, _ = press(t, m, "s") // back to name
	if got := m.rows[0].Name; got != "db" {
		t.Errorf("cycling did not return to sort by name, got %q first", got)
	}
}

func TestSelectionSticksToTheTunnelAcrossSnapshots(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	m, _ = press(t, m, "down") // select dns

	selected, _ := m.selected()
	if selected.Name != "dns" {
		t.Fatalf("selected %q, want dns", selected.Name)
	}

	// A new snapshot arrives without the db row; the cursor must stay on dns
	// rather than sliding onto whatever now occupies its index.
	snap := testSnapshot()
	snap.Tunnels = []state.Tunnel{snap.Tunnels[0], snap.Tunnels[2]}
	updated, _ := m.Update(SnapshotMsg(snap))
	m = updated.(Model)

	selected, _ = m.selected()
	if selected.Name != "dns" {
		t.Errorf("selection moved to %q after a snapshot, want dns", selected.Name)
	}
}

func TestLogPaneTogglesAndShowsRecentLines(t *testing.T) {
	logs := logbuf.New(10)
	logs.Add("00:00:01 INFO  tunnel started tunnel=web:80/tcp")
	m := New(newFakeActions(), logs)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	updated, _ = updated.(Model).Update(SnapshotMsg(testSnapshot()))
	m = updated.(Model)

	if strings.Contains(m.View(), "tunnel started") {
		t.Error("log pane is visible before being toggled on")
	}
	m, _ = press(t, m, "l")
	if !strings.Contains(m.View(), "tunnel started") {
		t.Errorf("log pane does not show buffered lines\n---\n%s", m.View())
	}
	m, _ = press(t, m, "l")
	if strings.Contains(m.View(), "tunnel started") {
		t.Error("log pane did not toggle back off")
	}
}

func TestZeroSizedTerminalKeepsTheTableUsable(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	m = updated.(Model)

	if m.height <= 0 || m.width <= 0 {
		t.Fatalf("size collapsed to %dx%d", m.width, m.height)
	}
	view := m.View()
	for _, name := range []string{"db", "dns", "web"} {
		if !strings.Contains(view, name) {
			t.Errorf("row %q vanished when the terminal reported no size\n---\n%s", name, view)
		}
	}
}

func TestQuitReturnsQuitCommand(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	if _, cmd := press(t, m, "q"); cmd == nil {
		t.Error("q returned no command, want tea.Quit")
	}
}

func TestEmptyStateExplainsItself(t *testing.T) {
	m := New(newFakeActions(), logbuf.New(10))
	updated, _ := m.Update(SnapshotMsg(state.Snapshot{Target: "host", SSH: sshconn.Status{State: sshconn.StateConnected}}))
	if view := updated.(Model).View(); !strings.Contains(view, "no forwardable ports discovered yet") {
		t.Errorf("empty dashboard gives no explanation\n---\n%s", view)
	}
}
