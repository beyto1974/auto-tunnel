package tunnel

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/sanitize"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

// Manager keeps the set of live local listeners equal to what discovery reports.
// It never tears down a working tunnel that is still wanted: only genuinely new,
// gone, or re-targeted ports cause a change.
type Manager struct {
	dial  DialerFunc
	alloc *Allocator
	log   *slog.Logger

	mu         sync.Mutex
	forwarders map[string]*forwarder
	problems   map[string]state.Tunnel // rows that exist but carry no traffic
}

// NewManager creates a manager. dial supplies the current SSH client and must
// return nil while the connection is down.
func NewManager(dial DialerFunc, alloc *Allocator, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		dial:       dial,
		alloc:      alloc,
		log:        log,
		forwarders: map[string]*forwarder{},
		problems:   map[string]state.Tunnel{},
	}
}

// SSHDialer adapts a managed SSH connection to a DialerFunc.
func SSHDialer(conn *sshconn.Conn) DialerFunc {
	return func() Dialer {
		client := conn.Client()
		if client == nil {
			return nil
		}
		return client
	}
}

// Reconcile brings the live tunnel set in line with the discovered ports.
func (m *Manager) Reconcile(ctx context.Context, maps []discovery.PortMap) {
	desired := make(map[string]discovery.PortMap, len(maps))
	for _, pm := range maps {
		desired[pm.Key()] = scrub(pm)
	}

	var toStop []*forwarder

	m.mu.Lock()
	// Drop tunnels whose port is gone, and those whose remote target moved
	// (a container republished on a different host port).
	for key, f := range m.forwarders {
		pm, wanted := desired[key]
		if !wanted {
			m.log.Info("tunnel removed", "tunnel", key, "local_port", f.localPort)
			toStop = append(toStop, f)
			delete(m.forwarders, key)
			continue
		}
		if pm.Target() != f.portMap.Target() {
			m.log.Info("tunnel target changed, restarting",
				"tunnel", key, "from", f.portMap.Target(), "to", pm.Target())
			toStop = append(toStop, f)
			delete(m.forwarders, key)
		}
	}
	for key := range m.problems {
		if _, wanted := desired[key]; !wanted {
			delete(m.problems, key)
		}
	}

	// Add what is missing.
	for key, pm := range desired {
		if !pm.Forwardable() {
			m.problems[key] = unsupportedRow(pm)
			continue
		}
		if _, live := m.forwarders[key]; live {
			continue
		}
		m.start(ctx, key, pm)
	}
	m.mu.Unlock()

	// Stopping blocks until the accept loop drains, so do it outside the lock.
	for _, f := range toStop {
		f.stop()
	}
}

// start binds a local port and begins forwarding. Callers hold m.mu.
func (m *Manager) start(ctx context.Context, key string, pm discovery.PortMap) {
	ln, port, err := m.alloc.Listen(key, pm.RemotePort())
	if err != nil {
		m.log.Error("could not bind a local port", "tunnel", key, "err", err)
		m.problems[key] = errorRow(pm, err.Error())
		return
	}
	delete(m.problems, key)

	f := newForwarder(key, pm, ln, port, m.dial, m.log)
	f.start(ctx)
	m.forwarders[key] = f
	m.log.Info("tunnel started",
		"tunnel", key, "container", pm.Name,
		"local", net.JoinHostPort(m.alloc.Bind(), strconv.Itoa(port)), "remote", pm.Target())
}

// Tunnels returns the current dashboard rows, sorted for stable display.
func (m *Manager) Tunnels() []state.Tunnel {
	sshUp := m.dial() != nil

	m.mu.Lock()
	rows := make([]state.Tunnel, 0, len(m.forwarders)+len(m.problems))
	for _, f := range m.forwarders {
		rows = append(rows, f.status(sshUp))
	}
	for _, row := range m.problems {
		rows = append(rows, row)
	}
	m.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ContainerPort < rows[j].ContainerPort
	})
	return rows
}

// TogglePause flips whether a tunnel accepts new connections. It reports the new
// paused value and whether the key matched a live tunnel.
func (m *Manager) TogglePause(key string) (paused, ok bool) {
	m.mu.Lock()
	f := m.forwarders[key]
	m.mu.Unlock()
	if f == nil {
		return false, false
	}
	paused = f.togglePause()
	m.log.Info("tunnel pause toggled", "tunnel", key, "paused", paused)
	return paused, true
}

// Close tears down every tunnel.
func (m *Manager) Close() {
	m.mu.Lock()
	forwarders := make([]*forwarder, 0, len(m.forwarders))
	for key, f := range m.forwarders {
		forwarders = append(forwarders, f)
		delete(m.forwarders, key)
	}
	m.mu.Unlock()

	for _, f := range forwarders {
		f.stop()
	}
}

// scrub strips terminal control sequences from the strings that came off the
// remote host, once, at the point where discovery output becomes state the UI
// and the log will print. Doing it here means every row builder below, both
// renderers, and the log file all get clean text.
func scrub(pm discovery.PortMap) discovery.PortMap {
	pm.Name = sanitize.String(pm.Name)
	pm.Image = sanitize.String(pm.Image)
	pm.TargetHost = sanitize.String(pm.TargetHost)
	return pm
}

func unsupportedRow(pm discovery.PortMap) state.Tunnel {
	return state.Tunnel{
		Key:           pm.Key(),
		Name:          pm.Name,
		Image:         pm.Image,
		Proto:         string(pm.Proto),
		ContainerPort: pm.ContainerPort,
		RemotePort:    pm.RemotePort(),
		RemoteTarget:  pm.Target(),
		Published:     pm.Published(),
		State:         state.TunnelUnsupported,
		LastError:     "ssh forwards tcp only",
		Since:         time.Now(),
	}
}

func errorRow(pm discovery.PortMap, reason string) state.Tunnel {
	return state.Tunnel{
		Key:           pm.Key(),
		Name:          pm.Name,
		Image:         pm.Image,
		Proto:         string(pm.Proto),
		ContainerPort: pm.ContainerPort,
		RemotePort:    pm.RemotePort(),
		RemoteTarget:  pm.Target(),
		Published:     pm.Published(),
		State:         state.TunnelError,
		LastError:     reason,
		Since:         time.Now(),
	}
}
