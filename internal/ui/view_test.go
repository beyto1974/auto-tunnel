package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

func TestNextScanText(t *testing.T) {
	tests := []struct {
		name string
		next time.Time
		want string
	}{
		{name: "no scan scheduled", next: time.Time{}, want: "-"},
		{name: "already due", next: time.Now().Add(-time.Second), want: "now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextScanText(tt.next); got != tt.want {
				t.Errorf("nextScanText = %q, want %q", got, tt.want)
			}
		})
	}

	// A future deadline is rendered as a duration; the exact number depends on
	// how long the test itself took, so only the unit is asserted.
	if got := nextScanText(time.Now().Add(90 * time.Second)); !strings.HasSuffix(got, "s") && !strings.HasSuffix(got, "m") {
		t.Errorf("nextScanText for a future scan = %q, want a duration", got)
	}
}

func TestRoundRTT(t *testing.T) {
	// A sub-millisecond link would otherwise render as a flat "0s", which reads
	// like a broken measurement rather than a fast one.
	if got := roundRTT(432 * time.Microsecond); got != 400*time.Microsecond {
		t.Errorf("roundRTT(432µs) = %s, want 400µs", got)
	}
	if got := roundRTT(4321 * time.Microsecond); got != 4*time.Millisecond {
		t.Errorf("roundRTT(4.321ms) = %s, want 4ms", got)
	}
}

func TestShortDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: -5 * time.Second, want: "0s"}, // a clock skew must not print "-5s"
		{d: 0, want: "0s"},
		{d: 45 * time.Second, want: "45s"},
		{d: 90 * time.Second, want: "1m"},
		{d: 59 * time.Minute, want: "59m"},
		{d: 90 * time.Minute, want: "1h30m"},
		{d: 25 * time.Hour, want: "25h0m"},
	}
	for _, tt := range tests {
		if got := shortDuration(tt.d); got != tt.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{n: 0, want: "0B"},
		{n: 1023, want: "1023B"},
		{n: 1024, want: "1.0KB"},
		{n: 1536, want: "1.5KB"},
		{n: 1 << 20, want: "1.0MB"},
		{n: 1 << 30, want: "1.0GB"},
		{n: 1 << 40, want: "1.0TB"},
		{n: 1 << 50, want: "1.0PB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestViewTruncate(t *testing.T) {
	tests := []struct {
		s     string
		width int
		want  string
	}{
		{s: "nginx", width: 0, want: ""},
		{s: "nginx", width: -1, want: ""},
		{s: "nginx", width: 1, want: "…"},
		{s: "nginx", width: 5, want: "nginx"},
		{s: "nginx", width: 9, want: "nginx"},
		{s: "nginx:alpine", width: 6, want: "nginx…"},
	}
	for _, tt := range tests {
		if got := truncate(tt.s, tt.width); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.width, got, tt.want)
		}
	}
}

func TestHeaderFallsBackForUnknownStates(t *testing.T) {
	// A state string the style map has never seen must still render, not panic:
	// sshconn could grow a state without ui knowing about it.
	m := newTestModel(t, newFakeActions())
	snap := testSnapshot()
	snap.SSH.State = sshconn.State("quiescing")
	updated, _ := m.Update(SnapshotMsg(snap))

	if got := updated.(Model).header(); !strings.Contains(got, "quiescing") {
		t.Errorf("header = %q, want it to contain the unknown state", got)
	}
}

func TestHeaderShowsTheReconnectAttemptCount(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	snap := testSnapshot()
	snap.SSH = sshconn.Status{State: sshconn.StateReconnecting, Attempts: 3}
	updated, _ := m.Update(SnapshotMsg(snap))

	if got := updated.(Model).header(); !strings.Contains(got, "attempt 3") {
		t.Errorf("header = %q, want the reconnect attempt count", got)
	}
}

func TestTableFallsBackForUnknownTunnelStates(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	snap := testSnapshot()
	// Not the first row: the cursor row is styled as selected, which skips the
	// per-state style lookup this test is after.
	snap.Tunnels[1].State = state.TunnelState("DRAINING")
	updated, _ := m.Update(SnapshotMsg(snap))

	if got := updated.(Model).table(); !strings.Contains(got, "DRAINING") {
		t.Errorf("table = %q, want it to render the unknown state", got)
	}
}

func TestTableScrollsAndReportsTheWindow(t *testing.T) {
	m := New(newFakeActions(), nil)
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 12})

	snap := state.Snapshot{}
	for i := 0; i < 12; i++ {
		snap.Tunnels = append(snap.Tunnels, state.Tunnel{
			Key:           string(rune('a'+i)) + ":80/tcp",
			Name:          string(rune('a' + i)),
			State:         state.TunnelListening,
			LocalPort:     8000 + i,
			ContainerPort: 80,
			RemoteTarget:  "127.0.0.1:80",
		})
	}
	loaded, _ := sized.(Model).Update(SnapshotMsg(snap))

	// "G" jumps to the last row, which has to scroll the window down.
	bottom, _ := press(t, loaded.(Model), "G")
	table := bottom.table()

	if !strings.Contains(table, "showing ") || !strings.Contains(table, " of 12") {
		t.Errorf("table is missing the scroll indicator:\n%s", table)
	}
	if strings.Contains(table, "\na ") {
		t.Errorf("table still shows the first row after scrolling to the end:\n%s", table)
	}
}

func TestTableExplainsAnEmptyFilterResult(t *testing.T) {
	m := newTestModel(t, newFakeActions())

	filtering, _ := press(t, m, "/")
	typed, _ := press(t, filtering, "zzz")

	if got := typed.table(); !strings.Contains(got, `no tunnels match "zzz"`) {
		t.Errorf("table = %q, want the no-match explanation", got)
	}
}

func TestTableShowsTheSelectedRowsLastError(t *testing.T) {
	// The error is what tells the user why a row is not carrying traffic, and it
	// only has room to appear under the selection.
	m := newTestModel(t, newFakeActions())

	// Rows sort by name (db, dns, web); dns is the UNSUPPORTED one that carries
	// an error, so the selection has to land on it.
	onDNS, _ := press(t, m, "down")

	if got := onDNS.table(); !strings.Contains(got, "ssh forwards tcp only") {
		t.Errorf("table = %q, want the selected row's error", got)
	}
}

func TestLogPaneSaysWhenItIsEmpty(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	withLog, _ := press(t, m, "l")

	if got := withLog.View(); !strings.Contains(got, "(no log output yet)") {
		t.Errorf("view = %q, want the empty log placeholder", got)
	}
}

func TestFooterShowsTheFilterPrompt(t *testing.T) {
	m := newTestModel(t, newFakeActions())
	filtering, _ := press(t, m, "/")
	typed, _ := press(t, filtering, "web")

	got := typed.footer()
	if !strings.Contains(got, "filter: web") || !strings.Contains(got, "enter to apply") {
		t.Errorf("footer = %q, want the editing prompt", got)
	}
}
