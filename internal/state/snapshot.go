// Package state defines the read-only view of the system that the tunnel
// manager publishes and the UI consumes. Nothing here is shared mutable state:
// a snapshot is a copy, which is what keeps the renderer race-free.
package state

import (
	"time"

	"github.com/beyto1974/auto-tunnel/internal/sshconn"
)

// TunnelState is the display status of one forwarding entry.
type TunnelState string

const (
	// TunnelListening means the local port is bound and idle.
	TunnelListening TunnelState = "LISTENING"
	// TunnelActive means at least one connection is currently flowing.
	TunnelActive TunnelState = "ACTIVE"
	// TunnelDegraded means the local port is still bound but SSH is down, so
	// new connections cannot be carried through yet.
	TunnelDegraded TunnelState = "DEGRADED"
	// TunnelPaused means the user stopped accepting connections on this port.
	TunnelPaused TunnelState = "PAUSED"
	// TunnelError means the tunnel could not be established, typically because
	// no local port could be bound.
	TunnelError TunnelState = "ERROR"
	// TunnelUnsupported means the port was discovered but cannot be forwarded,
	// which today means UDP or SCTP.
	TunnelUnsupported TunnelState = "UNSUPPORTED"
)

// Tunnel is one row of the dashboard.
type Tunnel struct {
	Key           string
	Name          string
	Image         string
	Proto         string
	ContainerPort int
	RemotePort    int
	RemoteTarget  string
	LocalPort     int
	Published     bool
	State         TunnelState
	ActiveConns   int64
	TotalConns    int64
	BytesIn       int64
	BytesOut      int64
	LastError     string
	Since         time.Time
}

// Snapshot is the whole picture at one instant.
type Snapshot struct {
	Target         string
	SSH            sshconn.Status
	Tunnels        []Tunnel
	Containers     int
	DiscoveryError string
	Warnings       []string
	LastScan       time.Time
	NextScan       time.Time
	Started        time.Time
}

// Counts summarises tunnel states for the header line.
func (s Snapshot) Counts() (active, listening, degraded, broken int) {
	for _, t := range s.Tunnels {
		switch t.State {
		case TunnelActive:
			active++
		case TunnelListening, TunnelPaused:
			listening++
		case TunnelDegraded:
			degraded++
		case TunnelError, TunnelUnsupported:
			broken++
		}
	}
	return
}
