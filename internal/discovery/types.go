// Package discovery inspects a remote host and reports the ports worth
// forwarding: the ports of its running Docker containers, the sockets the host
// itself is listening on, and the forwards the user declared by hand.
package discovery

import (
	"net"
	"strconv"
)

// Source says where a forwarding candidate came from. It is part of the tunnel
// key, so the same remote port found two ways stays two distinct rows rather
// than one row that flickers between descriptions.
type Source string

const (
	// SourceDocker is a container port reported by remote `docker ps`.
	SourceDocker Source = "docker"
	// SourceHost is a socket the remote host itself is listening on, found by
	// asking it what is listening rather than by asking Docker.
	SourceHost Source = "host"
	// SourceStatic is a forward the user declared with -forward. It exists
	// whether or not anything is listening on the other end, and no discovery
	// failure can take it away.
	SourceStatic Source = "static"
)

// Proto is a transport protocol as reported by Docker.
type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoUDP  Proto = "udp"
	ProtoSCTP Proto = "sctp"
)

// PortSpec is one entry parsed out of the `docker ps` Ports column.
type PortSpec struct {
	HostIP        string // publish address on the remote host, empty when unpublished
	HostPort      int    // 0 when the port is only EXPOSEd
	ContainerPort int
	Proto         Proto
}

// Published reports whether Docker mapped this port onto the remote host.
func (p PortSpec) Published() bool { return p.HostPort != 0 }

// PortMap is a discovered forwarding candidate: one remote port, plus the
// address on the remote side that reaches it.
//
// The field names read as Docker's, because Docker is where this started and
// where most rows still come from. For the other sources: ContainerPort is the
// remote port that identifies the service, and Image is whatever detail is
// worth showing beside the name — the bound address for a host socket, the
// original spec for a declared forward.
type PortMap struct {
	// Source is where this candidate came from. The zero value is
	// SourceDocker, so a docker row needs no extra bookkeeping.
	Source Source
	// Owner identifies the thing the port belongs to, within its source: a
	// container ID, the constant "host", or a -forward label. It is what makes
	// the tunnel key stable across polls, so it must not carry anything that
	// changes when the service restarts (a PID, for instance).
	Owner         string
	ContainerID   string
	Name          string
	Image         string
	ContainerPort int
	HostIP        string
	HostPort      int
	// LocalPref is the local port to prefer. Zero means "the same number as the
	// remote port", which is what every discovered row wants.
	LocalPref  int
	Proto      Proto
	TargetHost string // remote-side dial host: 127.0.0.1, a container IP, or a bind address
	// Offered marks a candidate that is listed on the dashboard but not
	// forwarded until the user asks for it. The zero value forwards, which is
	// what a discovered Docker port has always done.
	Offered bool
}

// Src is the source this candidate came from, resolving the zero value.
func (p PortMap) Src() Source {
	if p.Source == "" {
		return SourceDocker
	}
	return p.Source
}

// OwnerID identifies the owning container, host, or declared forward.
func (p PortMap) OwnerID() string {
	if p.Owner != "" {
		return p.Owner
	}
	return p.ContainerID
}

// Published reports whether Docker mapped this port onto the remote host.
func (p PortMap) Published() bool { return p.HostPort != 0 }

// RemotePort is the port to dial on TargetHost.
func (p PortMap) RemotePort() int {
	if p.Published() {
		return p.HostPort
	}
	return p.ContainerPort
}

// Target is the address the SSH client dials on the remote side.
func (p PortMap) Target() string {
	return net.JoinHostPort(p.TargetHost, strconv.Itoa(p.RemotePort()))
}

// PreferredLocal is the local port to try first. Discovered rows want the same
// number as the remote port; a declared forward may ask for another one.
func (p PortMap) PreferredLocal() int {
	if p.LocalPref != 0 {
		return p.LocalPref
	}
	return p.RemotePort()
}

// Forwardable reports whether SSH can carry this port. SSH port forwarding is
// TCP-only, so UDP and SCTP services are listed but never tunneled.
func (p PortMap) Forwardable() bool { return p.Proto == ProtoTCP }

// Key identifies a forwarding candidate across polls. It is deliberately tied to
// the container port rather than the host port, so a container that restarts
// onto a different published port is still recognised as the same tunnel. The
// source is part of the key so that a container port and a host socket on the
// same number stay two rows, each with its own local port.
func (p PortMap) Key() string {
	return string(p.Src()) + ":" + p.OwnerID() + ":" + strconv.Itoa(p.ContainerPort) + "/" + string(p.Proto)
}
