package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beyto1974/auto-tunnel/internal/logbuf"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
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

// mixedSnapshot holds one row per source, including a host port the user has
// not asked for yet.
func mixedSnapshot() state.Snapshot {
	return state.Snapshot{
		SSH: sshconn.Status{State: sshconn.StateConnected},
		Tunnels: []state.Tunnel{
			{
				Key: "docker:abc:80/tcp", Source: "docker", Enabled: true,
				Name: "web", Image: "nginx:alpine", State: state.TunnelListening,
				ContainerPort: 80, LocalPort: 8080, RemoteTarget: "127.0.0.1:8080",
			},
			{
				Key: "host:host:5432/tcp", Source: "host",
				Name: "postgres", Image: "127.0.0.1", State: state.TunnelOffered,
				ContainerPort: 5432, RemoteTarget: "127.0.0.1:5432",
			},
			{
				Key: "static:db:3306/tcp", Source: "static", Enabled: true,
				Name: "db", Image: "db=3306", State: state.TunnelListening,
				ContainerPort: 3306, LocalPort: 3306, RemoteTarget: "127.0.0.1:3306",
			},
		},
	}
}

func mixedModel(t *testing.T, actions Actions) Model {
	t.Helper()
	m := New(actions, logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(mixedSnapshot()))
	return loaded.(Model)
}

func TestEnterStartsForwardingAnOfferedPort(t *testing.T) {
	actions := newFakeActions()
	m := mixedModel(t, actions)

	// Rows sort by name: db, postgres, web.
	onPostgres, _ := press(t, m, "down")
	enabled, _ := press(t, onPostgres, "enter")

	if !actions.enabled["host:host:5432/tcp"] {
		t.Errorf("the engine was not asked to forward the offered port, last key %q", actions.lastKey)
	}
	if want := "forwarding postgres"; enabled.status != want {
		t.Errorf("status = %q, want %q", enabled.status, want)
	}
}

func TestEnterStopsAForwardedPort(t *testing.T) {
	actions := newFakeActions()
	actions.enabled["static:db:3306/tcp"] = true
	m := mixedModel(t, actions)

	stopped, _ := press(t, m, "enter") // db is the first row

	if actions.enabled["static:db:3306/tcp"] {
		t.Error("enter on a forwarded row did not switch it off")
	}
	if want := "stopped db"; stopped.status != want {
		t.Errorf("status = %q, want %q", stopped.status, want)
	}
}

func TestSpaceIsTheSameAsEnter(t *testing.T) {
	actions := newFakeActions()
	m := mixedModel(t, actions)

	pressed, _ := press(t, m, " ")

	if actions.lastKey != "static:db:3306/tcp" {
		t.Errorf("space toggled %q, want the selected row", actions.lastKey)
	}
	if pressed.status == "" {
		t.Error("space gave no feedback")
	}
}

func TestEnterReportsWhenTheEngineDoesNotKnowTheRow(t *testing.T) {
	// A row can vanish between the snapshot the user is looking at and the
	// engine's own view; saying so beats a keypress that silently does nothing.
	actions := newFakeActions()
	actions.unknown = true
	m := mixedModel(t, actions)

	refused, _ := press(t, m, "enter")

	if want := "db cannot be forwarded"; refused.status != want {
		t.Errorf("status = %q, want %q", refused.status, want)
	}
}

func TestEnterOnAnEmptyListSaysNothing(t *testing.T) {
	actions := newFakeActions()
	m := New(actions, logbuf.New(1))
	loaded, _ := m.Update(SnapshotMsg(state.Snapshot{}))

	pressed, _ := press(t, loaded.(Model), "enter")

	if pressed.status != "" {
		t.Errorf("status = %q, want nothing with no row selected", pressed.status)
	}
	if actions.lastKey != "" {
		t.Errorf("the engine was asked about %q with no row selected", actions.lastKey)
	}
}

func TestSourceKeyCyclesThroughTheSources(t *testing.T) {
	m := mixedModel(t, newFakeActions())
	if len(m.rows) != 3 {
		t.Fatalf("all sources show %d rows, want 3", len(m.rows))
	}

	for _, want := range []struct {
		source string
		name   string
	}{
		{source: "docker", name: "web"},
		{source: "host", name: "postgres"},
		{source: "static", name: "db"},
	} {
		next, _ := press(t, m, "t")
		m = next
		if len(m.rows) != 1 || m.rows[0].Name != want.name {
			t.Fatalf("source %q shows %+v, want only %s", want.source, m.rows, want.name)
		}
		if !strings.Contains(m.View(), "showing "+want.source) {
			t.Errorf("view does not say which source is shown\n---\n%s", m.View())
		}
	}

	back, _ := press(t, m, "t")
	if len(back.rows) != 3 {
		t.Errorf("cycling did not return to every source, got %d rows", len(back.rows))
	}
}

func TestBulkKeyActsOnExactlyTheVisibleRows(t *testing.T) {
	// "t" then "a" is the two-press answer to "forward all the host ports":
	// anything the filter hides must be left alone.
	actions := newFakeActions()
	m := mixedModel(t, actions)

	onHost, _ := press(t, m, "t")
	onHost, _ = press(t, onHost, "t")
	bulk, _ := press(t, onHost, "a")

	if len(actions.bulk) != 1 || actions.bulk[0] != "host:host:5432/tcp" {
		t.Errorf("bulk keys = %v, want only the visible host row", actions.bulk)
	}
	if !actions.bulkOn {
		t.Error("a row that was not forwarded should have been switched on")
	}
	if !strings.Contains(bulk.status, "forwarding 1 of 1") {
		t.Errorf("status = %q, want the count of rows changed", bulk.status)
	}
}

func TestBulkKeyOnlySwitchesOffWhenEverythingIsAlreadyOn(t *testing.T) {
	actions := newFakeActions()
	snap := mixedSnapshot()
	for i := range snap.Tunnels {
		snap.Tunnels[i].Enabled = true
	}
	m := New(actions, logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	bulk, _ := press(t, loaded.(Model), "a")

	if actions.bulkOn {
		t.Error("every row was already forwarded, so the key should have switched them off")
	}
	if !strings.Contains(bulk.status, "stopped 3 of 3") {
		t.Errorf("status = %q, want the count of rows changed", bulk.status)
	}
}

func TestBulkKeyOnAnEmptyListSaysNothing(t *testing.T) {
	actions := newFakeActions()
	m := New(actions, logbuf.New(1))
	loaded, _ := m.Update(SnapshotMsg(state.Snapshot{}))

	pressed, _ := press(t, loaded.(Model), "a")

	if pressed.status != "" || actions.bulk != nil {
		t.Errorf("status = %q, bulk = %v, want nothing to happen", pressed.status, actions.bulk)
	}
}

func TestEmptySourceFilterResultExplainsItself(t *testing.T) {
	snap := mixedSnapshot()
	snap.Tunnels = snap.Tunnels[:1] // docker only
	m := New(newFakeActions(), logbuf.New(1))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	onHost, _ := press(t, loaded.(Model), "t")
	onHost, _ = press(t, onHost, "t")

	if got := onHost.table(); !strings.Contains(got, `no tunnels match "host"`) {
		t.Errorf("table = %q, want the source named in the no-match message", got)
	}
}

func TestOpenKeyHandsTheURLToTheBrowser(t *testing.T) {
	actions := newFakeActions()
	snap := mixedSnapshot()
	snap.Tunnels[0].URL = "http://127.0.0.1:8080" // the web row
	m := New(actions, logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	// Rows sort by name: db, postgres, web.
	onWeb, _ := press(t, loaded.(Model), "G")
	opened, _ := press(t, onWeb, "o")

	if len(actions.opened) != 1 || actions.opened[0] != "http://127.0.0.1:8080" {
		t.Errorf("opened %v, want the selected row's url", actions.opened)
	}
	if want := "opening http://127.0.0.1:8080"; opened.status != want {
		t.Errorf("status = %q, want %q", opened.status, want)
	}
}

func TestOpenKeySaysWhenARowIsNotAWebService(t *testing.T) {
	actions := newFakeActions()
	m := mixedModel(t, actions) // no row carries a URL

	pressed, _ := press(t, m, "o")

	if len(actions.opened) != 0 {
		t.Errorf("opened %v, want nothing for a row with no url", actions.opened)
	}
	if want := "db is not a web service"; pressed.status != want {
		t.Errorf("status = %q, want %q", pressed.status, want)
	}
}

func TestOpenKeyReportsAFailureToOpen(t *testing.T) {
	actions := newFakeActions()
	actions.openErr = errors.New("exec: \"xdg-open\": executable file not found in $PATH")
	snap := mixedSnapshot()
	snap.Tunnels[0].URL = "http://127.0.0.1:8080"
	m := New(actions, logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	onWeb, _ := press(t, loaded.(Model), "G")
	failed, _ := press(t, onWeb, "o")

	if !strings.Contains(failed.status, "could not open") || !strings.Contains(failed.status, "xdg-open") {
		t.Errorf("status = %q, want the reason it could not open", failed.status)
	}
}

func TestOpenKeyOnAnEmptyListSaysNothing(t *testing.T) {
	actions := newFakeActions()
	m := New(actions, logbuf.New(1))
	loaded, _ := m.Update(SnapshotMsg(state.Snapshot{}))

	pressed, _ := press(t, loaded.(Model), "o")

	if pressed.status != "" || len(actions.opened) != 0 {
		t.Errorf("status = %q, opened = %v, want nothing to happen", pressed.status, actions.opened)
	}
}

func TestForwardedOnlyKeyHidesTheRowsNobodyPicked(t *testing.T) {
	// With a hundred AVAILABLE candidates listed, the handful actually being
	// forwarded is what the user came to look at.
	m := mixedModel(t, newFakeActions())
	if len(m.rows) != 3 {
		t.Fatalf("all rows = %d, want 3", len(m.rows))
	}

	only, _ := press(t, m, "f")

	if len(only.rows) != 2 {
		t.Fatalf("forwarded only shows %+v, want the two enabled rows", only.rows)
	}
	for _, row := range only.rows {
		if !row.Enabled {
			t.Errorf("row %q is not forwarded but is still shown", row.Name)
		}
	}
	if want := "showing forwarded only"; only.status != want {
		t.Errorf("status = %q, want %q", only.status, want)
	}
	view := only.View()
	if !strings.Contains(view, "forwarded only") || !strings.Contains(view, "f forwarded (on)") {
		t.Errorf("view does not say the filter is on\n---\n%s", view)
	}

	back, _ := press(t, only, "f")
	if len(back.rows) != 3 {
		t.Errorf("toggling back left %d rows, want all 3", len(back.rows))
	}
	if want := "showing everything discovered"; back.status != want {
		t.Errorf("status = %q, want %q", back.status, want)
	}
}

func TestForwardedOnlyCombinesWithTheOtherFilters(t *testing.T) {
	m := mixedModel(t, newFakeActions())

	onlyOn, _ := press(t, m, "f")
	hostOnly, _ := press(t, onlyOn, "t")
	hostOnly, _ = press(t, hostOnly, "t")

	// The only host row in the fixture is the offered one, so both filters
	// together leave nothing — and the message has to name both.
	if len(hostOnly.rows) != 0 {
		t.Fatalf("rows = %+v, want none: the host row is not forwarded", hostOnly.rows)
	}
	if got := hostOnly.table(); !strings.Contains(got, `no tunnels match "forwarded host"`) {
		t.Errorf("table = %q, want both filters named", got)
	}
}

func TestForwardedOnlyKeepsTheCursorOnARowThatSurvives(t *testing.T) {
	m := mixedModel(t, newFakeActions())
	onPostgres, _ := press(t, m, "down") // the offered row

	only, _ := press(t, onPostgres, "f")

	row, ok := only.selected()
	if !ok {
		t.Fatal("nothing is selected after hiding the selected row")
	}
	if !row.Enabled {
		t.Errorf("selection landed on %q, which is not forwarded", row.Name)
	}
}

func TestTableShowsTheURLInPlaceOfTheRemoteTarget(t *testing.T) {
	// A web row is there to be opened, so the column carries the address to
	// open; the far side moves to the selected-row line beneath the table.
	snap := mixedSnapshot()
	snap.Tunnels[0].URL = "https://127.0.0.1:15432"
	m := New(newFakeActions(), logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	table := loaded.(Model).table()
	if !strings.Contains(table, "https://127.0.0.1:15432") {
		t.Errorf("table does not show the url\n---\n%s", table)
	}
	// The longest URL this can produce must not be truncated.
	if strings.Contains(table, "https://127.0.0.1:1543…") {
		t.Errorf("the url was cut short\n---\n%s", table)
	}
	// A row with no URL keeps pointing at the remote side.
	if !strings.Contains(table, "127.0.0.1:5432") {
		t.Errorf("a non-web row lost its remote target\n---\n%s", table)
	}
	if !strings.Contains(table, "REMOTE / URL") {
		t.Errorf("the column header does not say it holds both\n---\n%s", table)
	}

	onWeb, _ := press(t, loaded.(Model), "G")
	if got := onWeb.table(); !strings.Contains(got, "via 127.0.0.1:8080") {
		t.Errorf("the selected row does not say what it points at\n---\n%s", got)
	}
}

func TestURLsAreRenderedAsPlainText(t *testing.T) {
	// OSC 8 carries no style of its own: every terminal that honours it
	// underlines the link, which puts a dotted rule through the table. The URL
	// is printed plainly instead, and "o" is what opens it.
	snap := mixedSnapshot()
	snap.Tunnels[0].URL = "http://127.0.0.1:8080"
	m := New(newFakeActions(), logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	view := loaded.(Model).View()
	if strings.Contains(view, "\x1b]8;;") {
		t.Errorf("a hyperlink escape is still emitted\n---\n%q", view)
	}
	if !strings.Contains(view, "http://127.0.0.1:8080") {
		t.Errorf("the url is not shown at all\n---\n%s", view)
	}
}

func TestWebRowsStayAlignedWithTheRest(t *testing.T) {
	// The URL cell is padded like every other column; a web row must not shear
	// the counters that follow it.
	snap := mixedSnapshot()
	snap.Tunnels[0].URL = "http://127.0.0.1:8080"
	m := New(newFakeActions(), logbuf.New(10))
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	var lengths []int
	for _, line := range strings.Split(loaded.(Model).table(), "\n") {
		if !strings.Contains(line, "0B") {
			continue // not a data row
		}
		lengths = append(lengths, len(stripSGR(line)))
	}
	if len(lengths) < 2 {
		t.Fatalf("found %d data rows, want at least two to compare", len(lengths))
	}
	for i, got := range lengths {
		if got != lengths[0] {
			t.Errorf("row %d renders %d columns, row 0 renders %d", i, got, lengths[0])
		}
	}
}

// stripSGR removes colour codes so only printable width is compared.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestSortByRemoteOrdersOnWhatTheColumnShows(t *testing.T) {
	// The column shows a URL where there is one, so sorting has to use the same
	// text: ordering by RemoteTarget would scatter the web services.
	rows := []state.Tunnel{
		{Name: "web", RemoteTarget: "127.0.0.1:8080", URL: "http://127.0.0.1:8080"},
		{Name: "db", RemoteTarget: "127.0.0.1:5432"},
		{Name: "vault", RemoteTarget: "127.0.0.1:8200", URL: "https://127.0.0.1:18200"},
		{Name: "cache", RemoteTarget: "10.0.0.9:6379"},
	}
	sortRows(rows, sortByRemote)

	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.Name)
	}
	// Bare host:port first (digits sort before letters), then the URLs together.
	want := []string{"cache", "db", "web", "vault"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortByRemote produced %v, want %v", got, want)
		}
	}
}

func TestSortKeyIsRememberedForTheNextRun(t *testing.T) {
	actions := newFakeActions()
	m := newTestModel(t, actions)

	m, _ = press(t, m, "s")
	if len(actions.saved) != 1 || actions.saved[0].Sort != "local port" {
		t.Fatalf("saved %+v, want the new sort order", actions.saved)
	}
	m, _ = press(t, m, "s")
	if actions.saved[len(actions.saved)-1].Sort != "traffic" {
		t.Errorf("saved %+v, want each change remembered", actions.saved)
	}
}

func TestSortStartsFromTheRememberedPreference(t *testing.T) {
	actions := newFakeActions()
	actions.prefs = Prefs{Sort: "remote/url"}

	m := newTestModel(t, actions)

	if m.sort != sortByRemote {
		t.Errorf("sort = %q, want the remembered remote/url", m.sort)
	}
	if !strings.Contains(m.View(), "s sort (remote/url)") {
		t.Errorf("the footer does not show the remembered order\n---\n%s", m.View())
	}
	// One press moves on from there rather than restarting the cycle.
	next, _ := press(t, m, "s")
	if next.sort != sortByName {
		t.Errorf("sort after one press = %q, want it to continue the cycle", next.sort)
	}
}

func TestUnknownPreferenceFallsBackToTheDefault(t *testing.T) {
	// A hand-edited or stale file must not be a startup failure.
	actions := newFakeActions()
	actions.prefs = Prefs{Sort: "by vibes"}

	if m := newTestModel(t, actions); m.sort != sortByName {
		t.Errorf("sort = %q, want the default for an unknown preference", m.sort)
	}
}
