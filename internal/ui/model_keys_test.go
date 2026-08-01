package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beyto1974/auto-tunnel/internal/logbuf"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

func TestCursorMovementKeys(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	if len(m.rows) != 3 {
		t.Fatalf("test snapshot has %d rows, want 3", len(m.rows))
	}

	// up at the top must not run off the start of the list
	top, _ := press(t, m, "up")
	if top.cursor != 0 {
		t.Errorf("cursor after up at the top = %d, want 0", top.cursor)
	}

	down, _ := press(t, m, "down")
	if down.cursor != 1 {
		t.Errorf("cursor after down = %d, want 1", down.cursor)
	}
	back, _ := press(t, down, "k")
	if back.cursor != 0 {
		t.Errorf("cursor after k = %d, want 0", back.cursor)
	}

	end, _ := press(t, m, "G")
	if end.cursor != 2 {
		t.Errorf("cursor after G = %d, want 2", end.cursor)
	}
	// down at the bottom must not run off the end either
	stuck, _ := press(t, end, "j")
	if stuck.cursor != 2 {
		t.Errorf("cursor after j at the bottom = %d, want 2", stuck.cursor)
	}
	home, _ := press(t, end, "g")
	if home.cursor != 0 {
		t.Errorf("cursor after g = %d, want 0", home.cursor)
	}
}

func TestEndKeyOnAnEmptyListStaysAtZero(t *testing.T) {
	m := New(newFakeActions(), logbuf.New(1))
	loaded, _ := m.Update(SnapshotMsg(state.Snapshot{}))

	end, _ := press(t, loaded.(Model), "G")

	if end.cursor != 0 {
		t.Errorf("cursor after G on an empty list = %d, want 0", end.cursor)
	}
}

func TestFilterBackspace(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	filtering, _ := press(t, m, "/")
	typed, _ := press(t, filtering, "web")

	trimmed, _ := typed.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := trimmed.(Model).filter; got != "we" {
		t.Errorf("filter after backspace = %q, want %q", got, "we")
	}

	// Backspacing an empty filter is a no-op, not an out-of-range slice.
	empty, _ := press(t, m, "/")
	for i := 0; i < 3; i++ {
		next, _ := empty.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		empty = next.(Model)
	}
	if got := empty.filter; got != "" {
		t.Errorf("filter after backspacing an empty prompt = %q, want empty", got)
	}
}

func TestFilterAcceptsSpaceAndEnterApplies(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	filtering, _ := press(t, m, "/")

	spaced, _ := filtering.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if got := spaced.(Model).filter; got != " " {
		t.Errorf("filter after space = %q, want a single space", got)
	}

	applied, _ := press(t, filtering, "enter")
	if applied.editing {
		t.Error("enter left the filter prompt focused")
	}
}

func TestCtrlCQuitsFromTheFilterPrompt(t *testing.T) {
	// The prompt swallows every rune, so without this the only way out of a
	// filter would be esc — Ctrl-C has to keep working as an exit.
	m := newTestModel(t, newFakeActions())
	filtering, _ := press(t, m, "/")

	_, cmd := filtering.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if cmd == nil {
		t.Fatal("ctrl+c in the filter prompt returned no command, want tea.Quit")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Errorf("ctrl+c returned %v, want tea.Quit", msg)
	}
}

func TestPauseOnAnEmptyListSaysNothing(t *testing.T) {
	actions := newFakeActions()
	m := New(actions, logbuf.New(1))
	loaded, _ := m.Update(SnapshotMsg(state.Snapshot{}))

	paused, _ := press(t, loaded.(Model), "p")

	if paused.status != "" {
		t.Errorf("status = %q, want nothing with no row selected", paused.status)
	}
	if actions.toggling {
		t.Error("the engine was asked to pause a tunnel that does not exist")
	}
}

func TestPauseReportsWhenTheEngineRefuses(t *testing.T) {
	// An UNSUPPORTED row has no forwarder behind it, so the manager reports the
	// key as unknown and the user needs to be told rather than left guessing.
	actions := newFakeActions()
	actions.unknown = true
	m := newTestModel(t, actions)

	paused, _ := press(t, m, "p")

	if want := "db cannot be paused"; paused.status != want {
		t.Errorf("status = %q, want %q", paused.status, want)
	}
}

func TestPauseThenResumeReportsBothWays(t *testing.T) {
	m := newTestModel(t, newFakeActions())

	paused, _ := press(t, m, "p")
	if want := "paused db"; paused.status != want {
		t.Errorf("status = %q, want %q", paused.status, want)
	}

	resumed, _ := press(t, paused, "p")
	if want := "resumed db"; resumed.status != want {
		t.Errorf("status = %q, want %q", resumed.status, want)
	}
}

func TestSortRowsBreaksTiesByContainerPort(t *testing.T) {
	// Several ports of one container share a name, so without the port tiebreak
	// their order would flip between snapshots and the rows would jitter.
	rows := []state.Tunnel{
		{Name: "web", ContainerPort: 443, LocalPort: 8443},
		{Name: "web", ContainerPort: 80, LocalPort: 8080},
		{Name: "api", ContainerPort: 3000, LocalPort: 3000},
	}
	sortRows(rows, sortByName)

	got := []string{}
	for _, r := range rows {
		got = append(got, r.Name+":"+string(rune('0'+r.ContainerPort/1000)))
	}
	if rows[0].Name != "api" || rows[1].ContainerPort != 80 || rows[2].ContainerPort != 443 {
		t.Errorf("sortRows produced %v, want api, web:80, web:443", got)
	}
}

func TestSortRowsByLocalPortAndTraffic(t *testing.T) {
	rows := []state.Tunnel{
		{Name: "b", LocalPort: 9000, BytesIn: 10},
		{Name: "a", LocalPort: 8000, BytesIn: 500, BytesOut: 500},
		{Name: "c", LocalPort: 8500, BytesIn: 20},
	}

	sortRows(rows, sortByLocalPort)
	if rows[0].LocalPort != 8000 || rows[2].LocalPort != 9000 {
		t.Errorf("sortByLocalPort produced %d, %d, %d", rows[0].LocalPort, rows[1].LocalPort, rows[2].LocalPort)
	}

	sortRows(rows, sortByTraffic)
	if rows[0].Name != "a" || rows[2].Name != "b" {
		t.Errorf("sortByTraffic produced %s, %s, %s, want the busiest first", rows[0].Name, rows[1].Name, rows[2].Name)
	}
}

func TestMatchesSearchesEveryColumn(t *testing.T) {
	row := state.Tunnel{Name: "web", Image: "nginx:alpine", RemoteTarget: "127.0.0.1:8080", State: state.TunnelActive}

	for _, needle := range []string{"", "WEB", "alpine", "8080", "active"} {
		if !matches(row, needle) {
			t.Errorf("matches(%q) = false, want true", needle)
		}
	}
	if matches(row, "postgres") {
		t.Error(`matches("postgres") = true, want false`)
	}
}

func TestZeroWindowSizeIsIgnored(t *testing.T) {
	m := newTestModel(t, newFakeActions())

	resized, _ := m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})

	got := resized.(Model)
	if got.width != m.width || got.height != m.height {
		t.Errorf("size became %dx%d, want the previous %dx%d", got.width, got.height, m.width, m.height)
	}
	if strings.TrimSpace(got.View()) == "" {
		t.Error("view is empty after a zero-sized resize")
	}
}
