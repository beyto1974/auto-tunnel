// Package discovery inspects a remote host's running Docker containers and
// reports the ports worth forwarding.
package discovery

import (
	"net"
	"strconv"
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

// PortMap is a discovered forwarding candidate: one container port, plus the
// address on the remote side that reaches it.
type PortMap struct {
	ContainerID   string
	Name          string
	Image         string
	ContainerPort int
	HostIP        string
	HostPort      int
	Proto         Proto
	TargetHost    string // remote-side dial host: 127.0.0.1, or the container IP
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

// Forwardable reports whether SSH can carry this port. SSH port forwarding is
// TCP-only, so UDP and SCTP services are listed but never tunneled.
func (p PortMap) Forwardable() bool { return p.Proto == ProtoTCP }

// Key identifies a forwarding candidate across polls. It is deliberately tied to
// the container port rather than the host port, so a container that restarts
// onto a different published port is still recognised as the same tunnel.
func (p PortMap) Key() string {
	return p.ContainerID + ":" + strconv.Itoa(p.ContainerPort) + "/" + string(p.Proto)
}
