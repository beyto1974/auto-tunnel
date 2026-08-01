package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

func TestNewAllocatorFillsInDefaults(t *testing.T) {
	a := NewAllocator("", 0, 0)

	if got := a.Bind(); got != "127.0.0.1" {
		t.Errorf("Bind() = %q, want loopback: an empty bind must not expose ports", got)
	}
	if a.fallbackBase != DefaultFallbackBase {
		t.Errorf("fallbackBase = %d, want %d", a.fallbackBase, DefaultFallbackBase)
	}
	if a.fallbackSize != DefaultFallbackSize {
		t.Errorf("fallbackSize = %d, want %d", a.fallbackSize, DefaultFallbackSize)
	}
}

func TestSSHDialerReportsNilWhileTheConnectionIsDown(t *testing.T) {
	// The forwarder asks for a dialer on every accepted connection and must be
	// told "not now" rather than handed a nil *ssh.Client wrapped in a non-nil
	// interface, which would panic on Dial.
	conn := sshconn.New(&sshconn.Target{User: "test", Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))

	if d := SSHDialer(conn)(); d != nil {
		t.Errorf("SSHDialer returned %v before Run connected, want nil", d)
	}
}

func TestReconcileReportsAnUnbindablePort(t *testing.T) {
	// Every candidate port is taken, so the tunnel cannot start. It still has to
	// appear as an ERROR row: a silently missing row looks like the container
	// was never discovered.
	preferred := occupiedPort(t)
	fallback := occupiedPort(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewManager(func() Dialer { return nil }, NewAllocator("127.0.0.1", fallback, 1), slog.New(slog.DiscardHandler))
	t.Cleanup(m.Close)

	m.Reconcile(ctx, []discovery.PortMap{portMap("web", 80, preferred, discovery.ProtoTCP)})

	row, ok := rowFor(m.Tunnels(), "web")
	if !ok {
		t.Fatal("no row for the tunnel that could not bind")
	}
	if row.State != "ERROR" {
		t.Errorf("state = %q, want ERROR", row.State)
	}
	if !strings.Contains(row.LastError, "no free local port") {
		t.Errorf("LastError = %q, want it to explain the binding failure", row.LastError)
	}
	if row.LocalPort != 0 {
		t.Errorf("LocalPort = %d, want 0 for a tunnel that never bound", row.LocalPort)
	}
}

func TestReconcileDropsProblemRowsWhoseContainerIsGone(t *testing.T) {
	// Problem rows live outside the forwarder map, so nothing else would ever
	// clear them and a stopped container would haunt the dashboard forever.
	m, ctx := testManager(t, func() Dialer { return nil })

	m.Reconcile(ctx, []discovery.PortMap{portMap("dns", 53, 53, discovery.ProtoUDP)})
	if _, ok := rowFor(m.Tunnels(), "dns"); !ok {
		t.Fatal("the unsupported port produced no row")
	}

	m.Reconcile(ctx, nil)
	if _, ok := rowFor(m.Tunnels(), "dns"); ok {
		t.Error("the row survived the container disappearing")
	}
}

func TestTunnelsSortsPortsOfOneContainerByContainerPort(t *testing.T) {
	m, ctx := testManager(t, func() Dialer { return nil })

	// UDP rows need no listener, which keeps this about ordering alone.
	m.Reconcile(ctx, []discovery.PortMap{
		portMap("dns", 5353, 5353, discovery.ProtoUDP),
		portMap("dns", 53, 53, discovery.ProtoUDP),
	})

	rows := m.Tunnels()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].ContainerPort != 53 || rows[1].ContainerPort != 5353 {
		t.Errorf("rows ordered %d, %d; want 53 before 5353",
			rows[0].ContainerPort, rows[1].ContainerPort)
	}
}

func TestTogglePauseRejectsAnUnknownKey(t *testing.T) {
	m, _ := testManager(t, func() Dialer { return nil })

	if paused, ok := m.TogglePause("nothing:80/tcp"); paused || ok {
		t.Errorf("TogglePause(unknown) = (%v, %v), want (false, false)", paused, ok)
	}
}

func TestHandleReportsAFailedRemoteDial(t *testing.T) {
	// The remote container can vanish between discovery and the first
	// connection; the row has to say so instead of looking healthy.
	echo := echoServer(t)
	fake := &fakeSSH{to: echo.Addr().String(), dialErr: errors.New("connection refused")}
	m, ctx := testManager(t, func() Dialer { return fake })

	pm := portMap("web", 80, freePort(t), discovery.ProtoTCP)
	m.Reconcile(ctx, []discovery.PortMap{pm})
	row := waitForRow(t, m, "web", "bind a local port", func(r state.Tunnel) bool { return r.LocalPort != 0 })

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(row.LocalPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local port: %v", err)
	}
	c.Close()

	got := waitForRow(t, m, "web", "record the dial failure", func(r state.Tunnel) bool { return r.LastError != "" })
	if !strings.Contains(got.LastError, "connection refused") {
		t.Errorf("LastError = %q, want the dial error", got.LastError)
	}
	if got.TotalConns != 0 {
		t.Errorf("TotalConns = %d, want 0: a connection that never reached the container does not count", got.TotalConns)
	}
}

func TestAcceptLoopSurvivesATransientAcceptError(t *testing.T) {
	// A listener can fail an Accept without being closed (fd exhaustion, for
	// one). The forwarder has to record it and keep accepting rather than exit
	// and leave a bound port that answers nothing.
	ln := &flakyListener{failures: 1, addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}}
	ln.released = make(chan struct{})

	f := newForwarder("web:80/tcp", portMap("web", 80, 8080, discovery.ProtoTCP), ln, 8080,
		func() Dialer { return nil }, slog.New(slog.DiscardHandler))
	f.start(context.Background())
	t.Cleanup(f.stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ln.calls() >= 2 && f.lastError() != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("accept loop stopped after an error: %d accepts, last error %q", ln.calls(), f.lastError())
}

func TestCloseWriteFallsBackToAFullClose(t *testing.T) {
	// A connection type without CloseWrite must still signal EOF to the peer, or
	// the other copy direction blocks until the deadline.
	c := &noHalfCloseConn{}
	closeWrite(c)

	if !c.closed {
		t.Error("closeWrite left a conn without CloseWrite open")
	}
}

// occupiedPort returns a port that stays bound for the rest of the test.
func occupiedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// flakyListener fails the first `failures` Accepts with a plain error — not
// net.ErrClosed — then blocks until it is closed.
type flakyListener struct {
	addr     net.Addr
	released chan struct{}

	mu       sync.Mutex
	failures int
	accepts  int
	closed   bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.accepts++
	remaining := l.failures
	if remaining > 0 {
		l.failures--
	}
	l.mu.Unlock()

	if remaining > 0 {
		return nil, errors.New("accept: too many open files")
	}
	<-l.released
	return nil, net.ErrClosed
}

func (l *flakyListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.released)
	}
	return nil
}

func (l *flakyListener) Addr() net.Addr { return l.addr }

func (l *flakyListener) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// noHalfCloseConn is a net.Conn that cannot half-close, like a pipe.
type noHalfCloseConn struct {
	net.Conn
	closed bool
}

func (c *noHalfCloseConn) Close() error {
	c.closed = true
	return nil
}
