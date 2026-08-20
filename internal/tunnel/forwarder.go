package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/probe"
	"github.com/beyto1974/auto-tunnel/internal/sanitize"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

// Dialer is the slice of *ssh.Client the forwarder needs, kept narrow so tests
// can substitute a plain in-process dialer.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// maxConcurrentProbes bounds how many ports are being identified at once. A run
// with -host-ports all can start a hundred tunnels in the same second, and
// letting each one open its own SSH channel immediately would burst the
// connection for no benefit: the answers are only needed as fast as a user can
// read them.
const maxConcurrentProbes = 8

// probeSlots is the semaphore enforcing that bound across every forwarder.
var probeSlots = make(chan struct{}, maxConcurrentProbes)

// DialerFunc returns the currently usable dialer, or nil while SSH is down.
// It must return an untyped nil rather than a nil *ssh.Client, otherwise the
// nil check on the other side never fires.
type DialerFunc func() Dialer

// forwarder owns one local listener and every connection accepted on it.
type forwarder struct {
	key       string
	portMap   discovery.PortMap
	localPort int
	bind      string
	listener  net.Listener
	client    DialerFunc
	log       *slog.Logger

	since  time.Time
	paused atomic.Bool
	scheme atomic.Value // probe.Scheme, once the port has been asked what it speaks

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

func newForwarder(key string, pm discovery.PortMap, ln net.Listener, localPort int, bind string, client DialerFunc, log *slog.Logger) *forwarder {
	return &forwarder{
		key:       key,
		portMap:   pm,
		localPort: localPort,
		bind:      bind,
		listener:  ln,
		client:    client,
		log:       log,
		since:     time.Now(),
		done:      make(chan struct{}),
		conns:     map[net.Conn]struct{}{},
	}
}

// start begins accepting connections until ctx ends or stop is called. When
// probing is on it also asks the remote service, once, whether it speaks HTTP.
func (f *forwarder) start(ctx context.Context, probeHTTP bool) {
	ctx, f.cancel = context.WithCancel(ctx)
	go f.acceptLoop(ctx)
	if probeHTTP {
		go f.probe(ctx)
	}
}

// probe identifies the service behind this tunnel so the dashboard can offer a
// URL. It waits for SSH rather than giving up, since a tunnel started while the
// link is down would otherwise never be identified, and it asks exactly once:
// a port that is not a web server now is not going to become one.
func (f *forwarder) probe(ctx context.Context) {
	select {
	case probeSlots <- struct{}{}:
		defer func() { <-probeSlots }()
	case <-ctx.Done():
		return
	}

	for {
		if dialer := f.client(); dialer != nil {
			scheme := probe.Detect(ctx, dialer.Dial, f.portMap.Target(), probe.DefaultTimeout)
			if ctx.Err() != nil {
				return
			}
			f.scheme.Store(scheme)
			if scheme != probe.SchemeNone {
				f.log.Info("tunnel speaks http", "tunnel", f.key, "scheme", string(scheme))
			}
			return
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// url is the address to open in a browser, empty until a probe says the service
// speaks HTTP. A wildcard bind is advertised as loopback: 0.0.0.0 is not
// somewhere a browser can go.
func (f *forwarder) url() string {
	scheme, _ := f.scheme.Load().(probe.Scheme)
	if scheme == probe.SchemeNone {
		return ""
	}
	host := f.bind
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return string(scheme) + "://" + net.JoinHostPort(host, strconv.Itoa(f.localPort))
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
		Source:        string(f.portMap.Src()),
		Enabled:       true,
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
		URL:           f.url(),
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
