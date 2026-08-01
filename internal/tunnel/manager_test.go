package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

// echoServer stands in for a container listening on the remote host.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// fakeSSH stands in for *ssh.Client: it dials in-process instead of over SSH,
// and records the targets it was asked for.
type fakeSSH struct {
	mu      sync.Mutex
	targets []string
	to      string // where every dial actually goes
}

func (f *fakeSSH) Dial(network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.targets = append(f.targets, addr)
	f.mu.Unlock()
	return net.Dial(network, f.to)
}

func (f *fakeSSH) lastTarget() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.targets) == 0 {
		return ""
	}
	return f.targets[len(f.targets)-1]
}

func testManager(t *testing.T, dial DialerFunc) (*Manager, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base := freePort(t)
	m := NewManager(dial, NewAllocator("127.0.0.1", base, 200), slog.New(slog.DiscardHandler))
	t.Cleanup(m.Close)
	return m, ctx
}

func portMap(name string, containerPort, hostPort int, proto discovery.Proto) discovery.PortMap {
	return discovery.PortMap{
		ContainerID:   name + "id",
		Name:          name,
		Image:         name + ":latest",
		ContainerPort: containerPort,
		HostIP:        "0.0.0.0",
		HostPort:      hostPort,
		Proto:         proto,
		TargetHost:    "127.0.0.1",
	}
}

// waitForRow polls until a tunnel row satisfies cond. Byte counters and the
// active-connection count settle a moment after a connection closes, so tests
// assert on the settled value rather than racing the forwarder's goroutines.
func waitForRow(t *testing.T, m *Manager, name string, what string, cond func(state.Tunnel) bool) state.Tunnel {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last state.Tunnel
	for time.Now().Before(deadline) {
		row, ok := rowFor(m.Tunnels(), name)
		if ok {
			last = row
			if cond(row) {
				return row
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s tunnel to %s; last row: %+v", name, what, last)
	return last
}

func rowFor(tunnels []state.Tunnel, name string) (state.Tunnel, bool) {
	for _, tn := range tunnels {
		if tn.Name == name {
			return tn, true
		}
	}
	return state.Tunnel{}, false
}

// roundTrip sends a payload through the local port and returns the echo.
func roundTrip(t *testing.T, port int, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local port %d: %v", port, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return string(buf)
}

func TestManagerForwardsTrafficAndCountsBytes(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	pm := portMap("web", 80, 8080, discovery.ProtoTCP)
	m.Reconcile(ctx, []discovery.PortMap{pm})

	row, ok := rowFor(m.Tunnels(), "web")
	if !ok {
		t.Fatal("no tunnel row for the discovered container")
	}
	if row.State != state.TunnelListening {
		t.Errorf("state = %s, want LISTENING", row.State)
	}

	const payload = "ping over the tunnel"
	if got := roundTrip(t, row.LocalPort, payload); got != payload {
		t.Errorf("echoed %q, want %q", got, payload)
	}

	// The dial must target the published host port on the remote loopback.
	if got, want := fake.lastTarget(), "127.0.0.1:8080"; got != want {
		t.Errorf("dialed %q, want %q", got, want)
	}

	row = waitForRow(t, m, "web", "record the round trip", func(tn state.Tunnel) bool {
		return tn.BytesIn == int64(len(payload)) && tn.BytesOut == int64(len(payload))
	})
	if row.TotalConns != 1 {
		t.Errorf("TotalConns = %d, want 1", row.TotalConns)
	}
}

func TestManagerReconcileChurn(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	web := portMap("web", 80, 8080, discovery.ProtoTCP)
	db := portMap("db", 5432, 5432, discovery.ProtoTCP)
	m.Reconcile(ctx, []discovery.PortMap{web, db})

	webRow, ok := rowFor(m.Tunnels(), "web")
	if !ok {
		t.Fatal("web tunnel missing after first reconcile")
	}
	dbRow, ok := rowFor(m.Tunnels(), "db")
	if !ok {
		t.Fatal("db tunnel missing after first reconcile")
	}

	// The db container stops. web must be left completely alone.
	m.Reconcile(ctx, []discovery.PortMap{web})

	if _, ok := rowFor(m.Tunnels(), "db"); ok {
		t.Error("db tunnel still present after its container disappeared")
	}
	if _, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dbRow.LocalPort)), time.Second); err == nil {
		t.Error("db local port still accepts connections after teardown")
	}

	after, _ := rowFor(m.Tunnels(), "web")
	if after.LocalPort != webRow.LocalPort {
		t.Errorf("web local port churned from %d to %d", webRow.LocalPort, after.LocalPort)
	}
	if got := roundTrip(t, after.LocalPort, "still up"); got != "still up" {
		t.Errorf("web tunnel broke during unrelated churn, echoed %q", got)
	}

	// The db container comes back on a different published port: same tunnel
	// key, so it must reclaim the same local port.
	dbMoved := portMap("db", 5432, 15432, discovery.ProtoTCP)
	m.Reconcile(ctx, []discovery.PortMap{web, dbMoved})

	restored, ok := rowFor(m.Tunnels(), "db")
	if !ok {
		t.Fatal("db tunnel missing after the container restarted")
	}
	if restored.LocalPort != dbRow.LocalPort {
		t.Errorf("db came back on local port %d, want the original %d", restored.LocalPort, dbRow.LocalPort)
	}
	roundTrip(t, restored.LocalPort, "back")
	if got, want := fake.lastTarget(), "127.0.0.1:15432"; got != want {
		t.Errorf("after republish dialed %q, want %q", got, want)
	}
}

func TestManagerRetargetsWhenPublishedPortMoves(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	m.Reconcile(ctx, []discovery.PortMap{portMap("api", 80, 8080, discovery.ProtoTCP)})
	before, _ := rowFor(m.Tunnels(), "api")
	roundTrip(t, before.LocalPort, "a")
	if got := fake.lastTarget(); got != "127.0.0.1:8080" {
		t.Fatalf("dialed %q, want 127.0.0.1:8080", got)
	}

	m.Reconcile(ctx, []discovery.PortMap{portMap("api", 80, 9090, discovery.ProtoTCP)})
	after, _ := rowFor(m.Tunnels(), "api")
	roundTrip(t, after.LocalPort, "b")
	if got, want := fake.lastTarget(), "127.0.0.1:9090"; got != want {
		t.Errorf("dialed %q after retarget, want %q", got, want)
	}
}

// TestManagerScrubsRemoteStrings covers the case where the remote host is
// hostile: container names and images end up on the user's terminal, so an
// escape sequence in one must never survive as far as a dashboard row.
func TestManagerScrubsRemoteStrings(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	pm := portMap("web", 80, 8080, discovery.ProtoTCP)
	pm.Name = "\x1b[2Jweb"
	pm.Image = "nginx\x1b]52;c;cGF5bG9hZA==\x07"

	// A UDP row and a bind failure take different code paths to the same table.
	udp := portMap("dns", 53, 53, discovery.ProtoUDP)
	udp.Name = "dns\rEVIL"

	m.Reconcile(ctx, []discovery.PortMap{pm, udp})

	rows := m.Tunnels()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		for _, field := range []string{row.Name, row.Image, row.RemoteTarget, row.LastError} {
			if strings.ContainsAny(field, "\x1b\r\n\x07") {
				t.Errorf("row %+v still carries a control character in %q", row, field)
			}
		}
	}
	if _, ok := rowFor(rows, "\x1b[2Jweb"); ok {
		t.Error("the raw escaped name reached a row")
	}
	if _, ok := rowFor(rows, "�[2Jweb"); !ok {
		t.Errorf("scrubbed name missing; rows: %+v", rows)
	}
}

func TestManagerReportsUnsupportedProtocols(t *testing.T) {
	fake := &fakeSSH{to: "127.0.0.1:1"}
	m, ctx := testManager(t, func() Dialer { return fake })

	m.Reconcile(ctx, []discovery.PortMap{portMap("dns", 53, 53, discovery.ProtoUDP)})

	row, ok := rowFor(m.Tunnels(), "dns")
	if !ok {
		t.Fatal("udp port should still be listed, just not forwarded")
	}
	if row.State != state.TunnelUnsupported {
		t.Errorf("state = %s, want UNSUPPORTED", row.State)
	}
	if row.LocalPort != 0 {
		t.Errorf("udp row bound local port %d, want none", row.LocalPort)
	}
}

func TestManagerDegradedWhileSSHIsDown(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}

	var mu sync.Mutex
	up := true
	m, ctx := testManager(t, func() Dialer {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			return nil
		}
		return fake
	})

	m.Reconcile(ctx, []discovery.PortMap{portMap("web", 80, 8080, discovery.ProtoTCP)})
	row, _ := rowFor(m.Tunnels(), "web")
	localPort := row.LocalPort

	mu.Lock()
	up = false
	mu.Unlock()

	row, _ = rowFor(m.Tunnels(), "web")
	if row.State != state.TunnelDegraded {
		t.Errorf("state = %s, want DEGRADED while ssh is down", row.State)
	}

	// The listener stays bound so the local port never moves under the user's
	// feet; connections simply fail until SSH returns.
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)), time.Second)
	if err != nil {
		t.Fatalf("local port stopped listening during an ssh outage: %v", err)
	}
	c.Close()

	mu.Lock()
	up = true
	mu.Unlock()

	if got := roundTrip(t, localPort, "recovered"); got != "recovered" {
		t.Errorf("tunnel did not recover after ssh came back, echoed %q", got)
	}
	waitForRow(t, m, "web", "return to LISTENING", func(tn state.Tunnel) bool {
		return tn.State == state.TunnelListening
	})
}

func TestManagerPauseRejectsNewConnections(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	m.Reconcile(ctx, []discovery.PortMap{portMap("web", 80, 8080, discovery.ProtoTCP)})
	row, _ := rowFor(m.Tunnels(), "web")

	paused, ok := m.TogglePause(row.Key)
	if !ok || !paused {
		t.Fatalf("TogglePause = (%v, %v), want (true, true)", paused, ok)
	}
	row, _ = rowFor(m.Tunnels(), "web")
	if row.State != state.TunnelPaused {
		t.Errorf("state = %s, want PAUSED", row.State)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(row.LocalPort)), time.Second)
	if err != nil {
		t.Fatalf("dial while paused: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c, "hello")
	if _, err := io.ReadFull(c, make([]byte, 1)); err == nil {
		t.Error("paused tunnel served traffic; the connection should be closed immediately")
	}

	if paused, _ := m.TogglePause(row.Key); paused {
		t.Fatal("second TogglePause should have resumed the tunnel")
	}
	if got := roundTrip(t, row.LocalPort, "resumed"); got != "resumed" {
		t.Errorf("resumed tunnel echoed %q", got)
	}
}

func TestManagerCloseReleasesPorts(t *testing.T) {
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String()}
	m, ctx := testManager(t, func() Dialer { return fake })

	m.Reconcile(ctx, []discovery.PortMap{portMap("web", 80, 8080, discovery.ProtoTCP)})
	row, _ := rowFor(m.Tunnels(), "web")

	m.Close()

	if _, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(row.LocalPort)), time.Second); err == nil {
		t.Error("local port is still listening after Close")
	}
	if len(m.Tunnels()) != 0 {
		t.Errorf("Tunnels() = %d rows after Close, want 0", len(m.Tunnels()))
	}
}
