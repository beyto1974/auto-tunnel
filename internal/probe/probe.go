// Package probe asks a forwarded service whether it speaks HTTP, so the
// dashboard can offer a URL worth clicking. It is deliberately minimal: one
// HEAD request, no body read, nothing sent that a web server does not see from
// every uptime checker on the internet.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"
)

// Scheme is what a probed port turned out to speak.
type Scheme string

const (
	// SchemeNone means the port did not answer as HTTP, which includes every
	// port that is not a web server and every one that could not be reached.
	SchemeNone Scheme = ""
	// SchemeHTTP is plaintext HTTP.
	SchemeHTTP Scheme = "http"
	// SchemeHTTPS is HTTP inside TLS.
	SchemeHTTPS Scheme = "https"
)

// DefaultTimeout bounds one probe. A service that has not answered a HEAD
// request in this long is not one whose URL we want to advertise anyway.
const DefaultTimeout = 3 * time.Second

// DialFunc opens a connection to the remote side; it is the SSH client's Dial
// in production and a plain net.Dial in tests.
type DialFunc func(network, addr string) (net.Conn, error)

// Detect reports what target speaks, using a separate connection per attempt.
//
// TLS is tried first, and the order matters: a plaintext HEAD sent to an HTTPS
// port does not fail cleanly — Go's own server, nginx, and most others answer it
// with a plaintext "400, you sent an HTTP request to an HTTPS port", which is a
// perfectly good HTTP status line and would have every HTTPS service reported as
// http. A TLS ClientHello sent to a plaintext port simply fails the handshake,
// so guessing wrong in this direction costs one connection and nothing else.
func Detect(ctx context.Context, dial DialFunc, target string, timeout time.Duration) Scheme {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if speaksHTTP(ctx, dial, target, timeout, true) {
		return SchemeHTTPS
	}
	if speaksHTTP(ctx, dial, target, timeout, false) {
		return SchemeHTTP
	}
	return SchemeNone
}

// speaksHTTP opens one connection and asks it for a status line.
func speaksHTTP(ctx context.Context, dial DialFunc, target string, timeout time.Duration, useTLS bool) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dial("tcp", target)
	if err != nil {
		return false
	}
	defer conn.Close()

	// SSH channels do not support deadlines — SetDeadline on one is an error,
	// not a timeout — so the context closes the connection instead. That is
	// what unblocks a read from a service that accepts and then says nothing.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	var rw io.ReadWriter = conn
	if useTLS {
		// The certificate is not verified: this is an identification probe, it
		// carries no credentials, and a tunnel to a service with a self-signed
		// certificate is exactly the case worth detecting. Only HTTP/1.1 is
		// offered, because the request below is an HTTP/1.1 one.
		tc := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"http/1.1"},
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			return false
		}
		rw = tc
	}

	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	if _, err := fmt.Fprintf(rw,
		"HEAD / HTTP/1.1\r\nHost: %s\r\nUser-Agent: auto-tunnel-probe\r\nConnection: close\r\n\r\n",
		host); err != nil {
		return false
	}

	// Only the status line's first token is needed, and reading no further
	// keeps a chatty service from filling a buffer with a response we discard.
	prefix := make([]byte, len("HTTP/"))
	if _, err := io.ReadFull(rw, prefix); err != nil {
		return false
	}
	return string(prefix) == "HTTP/"
}
