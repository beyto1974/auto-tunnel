package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/sshconn"
)

// DefaultListenCommand asks the remote host what it is listening on. `ss` is
// preferred and `netstat` is the fallback, because a host old enough to lack
// `ss` is exactly the kind of host this is useful on. Neither needs root: both
// list the sockets either way and only the process names require privilege, so
// a plain user gets rows named after their port instead of nothing at all.
const DefaultListenCommand = `ss -Hltnp 2>/dev/null || netstat -tlnp 2>/dev/null`

// HostMode decides what happens to the host sockets that are discovered.
type HostMode string

const (
	// HostPortsOff does not look at host sockets at all.
	HostPortsOff HostMode = "off"
	// HostPortsSelect lists them without forwarding, leaving the choice to the
	// user. It is the default, and the safe setting: a remote host is typically
	// listening on far more than the user wants republished on their own
	// machine, so the dashboard shows the choice rather than making it.
	HostPortsSelect HostMode = "select"
	// HostPortsAll forwards every host socket found.
	HostPortsAll HostMode = "all"
)

// ParseHostMode validates the -host-ports flag.
func ParseHostMode(s string) (HostMode, error) {
	switch HostMode(s) {
	case HostPortsOff, HostPortsSelect, HostPortsAll:
		return HostMode(s), nil
	}
	return "", fmt.Errorf("unknown host port mode %q, want off, select, or all", s)
}

// Enabled reports whether host sockets are polled at all. The zero value polls,
// matching the flag default, so a config assembled in code behaves like one
// assembled from the command line.
func (m HostMode) Enabled() bool { return m != HostPortsOff }

// Offered reports whether a host row starts out listed but not forwarded. Only
// HostPortsAll forwards on sight; every other value — including the zero value —
// leaves the choice to the user, because that is the answer that cannot expose
// a service by accident.
func (m HostMode) Offered() bool { return m != HostPortsAll }

// HostOptions configures a HostDiscoverer.
type HostOptions struct {
	Command   string
	Exclude   *regexp.Regexp // drop sockets whose name or port matches
	SkipPorts map[int]bool   // ports to ignore outright, such as our own SSH port
	Offered   bool           // list the rows without forwarding them
	Log       *slog.Logger
}

// HostDiscoverer polls the sockets the remote host itself is listening on.
type HostDiscoverer struct {
	opts HostOptions
}

// NewHost creates a HostDiscoverer, filling in defaults.
func NewHost(opts HostOptions) *HostDiscoverer {
	if opts.Command == "" {
		opts.Command = DefaultListenCommand
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &HostDiscoverer{opts: opts}
}

// Listener is one listening TCP socket on the remote host.
type Listener struct {
	Addr string // the address it is bound to, as reported
	Port int
	Proc string // the process holding it, empty when the remote user may not see it
}

var (
	// ss reports holders as users:(("postgres",pid=812,fd=7)).
	ssProcRe = regexp.MustCompile(`\("([^"]+)"`)
	// netstat reports them as 812/postgres, or "-" without privilege.
	netstatProcRe = regexp.MustCompile(`^\d+/(.+)$`)
)

// ParseListeners reads the output of `ss -Hltnp` or `netstat -tlnp`, in either
// case keeping only listening TCP sockets. Both layouts are accepted because
// the default command falls back from one to the other, and the caller cannot
// know which one answered. Unparseable rows are reported as warnings rather
// than failing the poll, exactly as the Docker parser does.
func ParseListeners(out string) ([]Listener, []string) {
	var (
		listeners []Listener
		warnings  []string
	)
	byPort := map[int]int{} // port -> index into listeners

	add := func(l Listener) {
		if i, seen := byPort[l.Port]; seen {
			// The IPv4 and IPv6 rows of one service collapse into a single
			// entry. Keep the first, but take a process name from the second if
			// the first had none.
			if listeners[i].Proc == "" {
				listeners[i].Proc = l.Proc
			}
			return
		}
		byPort[l.Port] = len(listeners)
		listeners = append(listeners, l)
	}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)

		var local, proc string
		switch {
		case fields[0] == "LISTEN" && len(fields) >= 4:
			// ss: State Recv-Q Send-Q Local:Port Peer:Port [users:(...)]
			local = fields[3]
			if m := ssProcRe.FindStringSubmatch(line); m != nil {
				proc = m[1]
			}
		case strings.HasPrefix(fields[0], "tcp") && len(fields) >= 6 && fields[5] == "LISTEN":
			// netstat: Proto Recv-Q Send-Q Local:Port Peer:Port State [PID/name]
			local = fields[3]
			if len(fields) >= 7 {
				if m := netstatProcRe.FindStringSubmatch(fields[6]); m != nil {
					proc = m[1]
				}
			}
		default:
			// Column headers, UDP rows, and anything else are not listeners.
			continue
		}

		addr, port, err := splitAddrPort(local)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("unrecognised listening address %q: %v", local, err))
			continue
		}
		add(Listener{Addr: addr, Port: port, Proc: proc})
	}

	sort.Slice(listeners, func(i, j int) bool { return listeners[i].Port < listeners[j].Port })
	return listeners, warnings
}

// splitAddrPort splits "127.0.0.1:5432", "[::1]:631", and netstat's ":::80".
// net.SplitHostPort rejects the last of those, which is why this exists.
func splitAddrPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("no port")
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad port")
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("port %d out of range", port)
	}
	return strings.Trim(s[:i], "[]"), port, nil
}

// dialHost is where a socket is reachable from the remote host's own point of
// view. A wildcard bind is dialed on loopback; a specific address is dialed as
// bound, since that may be the only one that answers.
func dialHost(addr string) string {
	switch addr {
	case "", "*", "0.0.0.0", "::":
		return loopback
	}
	return addr
}

// Discover runs one poll of the remote host's listening sockets.
func (d *HostDiscoverer) Discover(ctx context.Context, client *ssh.Client) (*Result, error) {
	out, err := sshconn.RunCommand(ctx, client, d.opts.Command)
	if err != nil {
		return nil, fmt.Errorf("host port discovery: %w", err)
	}

	listeners, warnings := ParseListeners(string(out))
	result := &Result{Warnings: warnings}
	for _, l := range listeners {
		if d.opts.SkipPorts[l.Port] {
			continue
		}
		name := l.Proc
		if name == "" {
			name = "port " + strconv.Itoa(l.Port)
		}
		if d.opts.Exclude != nil &&
			(d.opts.Exclude.MatchString(name) || d.opts.Exclude.MatchString(strconv.Itoa(l.Port))) {
			continue
		}
		result.Maps = append(result.Maps, PortMap{
			Source: SourceHost,
			// Every host socket shares one owner: the port number already makes
			// the key unique, and a process that restarts under a new PID has to
			// come back as the same tunnel on the same local port.
			Owner:         "host",
			Name:          name,
			Image:         l.Addr,
			ContainerPort: l.Port,
			HostIP:        l.Addr,
			HostPort:      l.Port,
			Proto:         ProtoTCP,
			TargetHost:    dialHost(l.Addr),
			Offered:       d.opts.Offered,
		})
	}
	return result, nil
}
