package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/sanitize"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

// Dialer is the slice of *ssh.Client the forwarder needs, kept narrow so tests
// can substitute a plain in-process dialer.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// DialerFunc returns the currently usable dialer, or nil while SSH is down.
// It must return an untyped nil rather than a nil *ssh.Client, otherwise the
// nil check on the other side never fires.
type DialerFunc func() Dialer

// forwarder owns one local listener and every connection accepted on it.
type forwarder struct {
	key       string
	portMap   discovery.PortMap
	localPort int
	listener  net.Listener
	client    DialerFunc
	log       *slog.Logger

	since  time.Time
	paused atomic.Bool

	activeConns atomic.Int64
	totalConns  atomic.Int64
	bytesIn     atomic.Int64
	bytesOut    atomic.Int64

	errMu   sync.Mutex
	lastErr string

	cancel context.CancelFunc
	done   chan struct{}

	connMu sync.Mutex
	conns  map[net.Conn]struct{} // live local connections, closed on shutdown
}

func newForwarder(key string, pm discovery.PortMap, ln net.Listener, localPort int, client DialerFunc, log *slog.Logger) *forwarder {
	return &forwarder{
		key:       key,
		portMap:   pm,
		localPort: localPort,
		listener:  ln,
		client:    client,
		log:       log,
		since:     time.Now(),
		done:      make(chan struct{}),
		conns:     map[net.Conn]struct{}{},
	}
}

// start begins accepting connections until ctx ends or stop is called.
func (f *forwarder) start(ctx context.Context) {
	ctx, f.cancel = context.WithCancel(ctx)
	go f.acceptLoop(ctx)
}

func (f *forwarder) acceptLoop(ctx context.Context) {
	defer close(f.done)

	// Unblock a pending Accept when the context ends.
	go func() {
		<-ctx.Done()
		f.listener.Close()
	}()

	for {
		local, err := f.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			f.setError(err)
			f.log.Warn("accept failed", "tunnel", f.key, "err", err)
			// A listener that keeps failing would spin; back off a little.
			select {
			case <-time.After(250 * time.Millisecond):
				continue
			case <-ctx.Done():
				return
			}
		}
		if f.paused.Load() {
			local.Close()
			continue
		}
		go f.handle(ctx, local)
	}
}

// handle bridges one local connection to the remote target over SSH.
func (f *forwarder) handle(ctx context.Context, local net.Conn) {
	f.track(local)
	defer f.untrack(local)
	defer local.Close()

	client := f.client()
	if client == nil {
		f.setError(errors.New("ssh connection is down"))
		return
	}
	remote, err := client.Dial("tcp", f.portMap.Target())
	if err != nil {
		f.setError(err)
		f.log.Warn("remote dial failed", "tunnel", f.key, "target", f.portMap.Target(), "err", err)
		return
	}
	defer remote.Close()

	f.totalConns.Add(1)
	f.activeConns.Add(1)
	defer f.activeConns.Add(-1)
	f.clearError()

	var wg sync.WaitGroup
	wg.Add(2)
	// Local to remote is "out"; remote to local is "in", matching how a user
	// thinks about traffic leaving their machine. Counting per write rather than
	// per copy keeps the dashboard live during long-lived connections.
	go func() {
		defer wg.Done()
		io.Copy(countingWriter{remote, &f.bytesOut}, local)
		closeWrite(remote)
	}()
	go func() {
		defer wg.Done()
		io.Copy(countingWriter{local, &f.bytesIn}, remote)
		closeWrite(local)
	}()

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-ctx.Done():
	}
}

// countingWriter tallies bytes as they are written, so counters reflect traffic
// in flight instead of only settling when the connection closes.
type countingWriter struct {
	w     io.Writer
	total *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.total.Add(int64(n))
	}
	return n, err
}

// closeWrite half-closes so the peer sees EOF while the other direction drains.
func closeWrite(c net.Conn) {
	type writeCloser interface{ CloseWrite() error }
	if wc, ok := c.(writeCloser); ok {
		wc.CloseWrite()
		return
	}
	c.Close()
}

// stop closes the listener and every connection it accepted.
func (f *forwarder) stop() {
	if f.cancel != nil {
		f.cancel()
	}
	f.listener.Close()

	f.connMu.Lock()
	for c := range f.conns {
		c.Close()
	}
	f.connMu.Unlock()

	<-f.done
}

func (f *forwarder) track(c net.Conn) {
	f.connMu.Lock()
	f.conns[c] = struct{}{}
	f.connMu.Unlock()
}

func (f *forwarder) untrack(c net.Conn) {
	f.connMu.Lock()
	delete(f.conns, c)
	f.connMu.Unlock()
}

func (f *forwarder) setError(err error) {
	f.errMu.Lock()
	f.lastErr = sanitize.Error(err)
	f.errMu.Unlock()
}

func (f *forwarder) clearError() {
	f.errMu.Lock()
	f.lastErr = ""
	f.errMu.Unlock()
}

func (f *forwarder) lastError() string {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	return f.lastErr
}

// togglePause flips whether new connections are accepted, returning the new value.
func (f *forwarder) togglePause() bool {
	paused := !f.paused.Load()
	f.paused.Store(paused)
	return paused
}

// status renders this forwarder as a dashboard row. sshUp decides between
// LISTENING and DEGRADED, since a bound port with no usable SSH connection is
// still reachable locally but cannot carry traffic.
func (f *forwarder) status(sshUp bool) state.Tunnel {
	active := f.activeConns.Load()
	t := state.Tunnel{
		Key:           f.key,
		Name:          f.portMap.Name,
		Image:         f.portMap.Image,
		Proto:         string(f.portMap.Proto),
		ContainerPort: f.portMap.ContainerPort,
		RemotePort:    f.portMap.RemotePort(),
		RemoteTarget:  f.portMap.Target(),
		LocalPort:     f.localPort,
		Published:     f.portMap.Published(),
		ActiveConns:   active,
		TotalConns:    f.totalConns.Load(),
		BytesIn:       f.bytesIn.Load(),
		BytesOut:      f.bytesOut.Load(),
		LastError:     f.lastError(),
		Since:         f.since,
	}
	switch {
	case f.paused.Load():
		t.State = state.TunnelPaused
	case !sshUp:
		t.State = state.TunnelDegraded
	case active > 0:
		t.State = state.TunnelActive
	default:
		t.State = state.TunnelListening
	}
	return t
}
