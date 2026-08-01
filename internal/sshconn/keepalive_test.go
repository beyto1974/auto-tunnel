package sshconn

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

// fastConn builds a Conn whose timings are measured in milliseconds, so the
// reconnect and keepalive paths run inside a test instead of on a 15-second
// production cycle.
func fastConn(t *testing.T, target *Target) *Conn {
	t.Helper()
	c := New(target, 2*time.Second, slog.New(slog.DiscardHandler))
	c.timings = timings{
		keepaliveInterval: 20 * time.Millisecond,
		keepaliveTimeout:  20 * time.Millisecond,
		minBackoff:        10 * time.Millisecond,
		maxBackoff:        40 * time.Millisecond,
	}
	return c
}

// waitForStatus polls until cond holds, which is how every test here observes a
// background goroutine rather than sleeping for a fixed guess.
func waitForStatus(t *testing.T, c *Conn, what string, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last Status
	for time.Now().Before(deadline) {
		last = c.Status()
		if cond(last) {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the connection to %s; last status: %+v", what, last)
	return last
}

func TestRunConnectsAndPublishesStatus(t *testing.T) {
	srv := newTrustedServer(t)

	c := fastConn(t, fixtureTarget(srv))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()

	client, err := c.WaitReady(ctx)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if client == nil {
		t.Fatal("WaitReady returned no client")
	}

	status := c.Status()
	if status.State != StateConnected {
		t.Errorf("state = %q, want %q", status.State, StateConnected)
	}
	if status.Generation != 1 {
		t.Errorf("generation = %d, want 1", status.Generation)
	}
	if status.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 after a first-try connect", status.Attempts)
	}

	// A live connection is pinged, and the measured RTT reaches the status.
	waitForStatus(t, c, "measure a keepalive round trip", func(s Status) bool { return s.RTT > 0 })
	if srv.PingCount() == 0 {
		t.Error("the server saw no keepalive requests")
	}

	cancel()
	<-done
	if got := c.Status().State; got != StateClosed {
		t.Errorf("state after Run returned = %q, want %q", got, StateClosed)
	}
	if c.Client() != nil {
		t.Error("Client() still returns a client after Run returned")
	}
}

func TestRunReconnectsAfterTheRemoteDropsTheLink(t *testing.T) {
	// This is the whole reason Conn exists: an sshd restart or a laptop lid must
	// not end the session, it must be reconnected through.
	srv := newTrustedServer(t)

	c := fastConn(t, fixtureTarget(srv))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go c.Run(ctx)

	if _, err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	srv.WaitForConnection(t)
	srv.DropConnections()

	status := waitForStatus(t, c, "reconnect", func(s Status) bool {
		return s.State == StateConnected && s.Generation >= 2
	})
	if status.Generation < 2 {
		t.Errorf("generation = %d, want at least 2 after a reconnect", status.Generation)
	}
}

func TestRunRetriesWhileTheHostIsUnreachable(t *testing.T) {
	srv := newTrustedServer(t) // a real known_hosts, so the failure is the dial and not the config

	target := fixtureTarget(srv)
	target.Port = sshtest.ClosedPort(t)

	c := fastConn(t, target)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go c.Run(ctx)

	status := waitForStatus(t, c, "retry the dial", func(s Status) bool { return s.Attempts >= 2 })
	if status.State != StateReconnecting {
		t.Errorf("state = %q, want %q while retrying", status.State, StateReconnecting)
	}
	if status.LastError == "" {
		t.Error("LastError is empty after failed dials")
	}
}

func TestMonitorGivesUpAfterRepeatedKeepaliveFailures(t *testing.T) {
	// A black-holed link answers nothing while the TCP connection stays open, so
	// only the keepalive notices. Two misses is the point of no return.
	srv := newTrustedServer(t)
	srv.SetKeepaliveGap(2 * time.Second)

	c := fastConn(t, fixtureTarget(srv))
	client := dialFixture(t, srv)

	err := c.monitor(t.Context(), client)

	if err == nil {
		t.Fatal("monitor returned no error for a connection that stopped answering")
	}
	if !strings.Contains(err.Error(), "keepalive failed 2 times") {
		t.Errorf("error = %v, want the keepalive failure count", err)
	}
	if !strings.Contains(err.Error(), "keepalive timed out after") {
		t.Errorf("error = %v, want the underlying timeout", err)
	}
}

func TestMonitorReturnsWhenTheRemoteHangsUp(t *testing.T) {
	srv := newTrustedServer(t)

	c := fastConn(t, fixtureTarget(srv))
	client := dialFixture(t, srv)

	done := make(chan error, 1)
	go func() { done <- c.monitor(t.Context(), client) }()

	srv.WaitForConnection(t)
	srv.DropConnections()

	select {
	case err := <-done:
		if err == nil {
			t.Error("monitor returned nil for a dropped connection, want a reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not notice the remote hanging up")
	}
}

func TestMonitorStopsWithItsContext(t *testing.T) {
	srv := newTrustedServer(t)

	c := fastConn(t, fixtureTarget(srv))
	client := dialFixture(t, srv)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := c.monitor(ctx, client); !errors.Is(err, context.Canceled) {
		t.Errorf("monitor returned %v, want context.Canceled", err)
	}
}

func TestPingMeasuresARoundTrip(t *testing.T) {
	srv := newTrustedServer(t)
	client := dialFixture(t, srv)

	rtt, err := ping(client, 5*time.Second)

	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if rtt <= 0 {
		t.Errorf("rtt = %s, want a positive duration", rtt)
	}
}

func TestPingGivesUpOnASilentServer(t *testing.T) {
	srv := newTrustedServer(t)
	srv.SetKeepaliveGap(2 * time.Second)
	client := dialFixture(t, srv)

	_, err := ping(client, 20*time.Millisecond)

	if err == nil {
		t.Fatal("ping succeeded against a server that never answered")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
}

func TestWaitReadyReportsAClosedConnection(t *testing.T) {
	// Run has exited, so nothing will ever connect. Blocking forever here would
	// hang startup with no explanation.
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))
	c.setClosed()

	_, err := c.WaitReady(t.Context())

	if err == nil {
		t.Fatal("WaitReady succeeded on a closed connection, want an error")
	}
	if !strings.Contains(err.Error(), "ssh connection closed") {
		t.Errorf("error = %v, want the closed-connection message", err)
	}
}

func TestWaitReadyCarriesTheLastErrorFromAClosedConnection(t *testing.T) {
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))
	c.recordFailure(errors.New("no route to host"))
	c.setClosed()

	_, err := c.WaitReady(t.Context())

	if err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("error = %v, want it to carry the last dial failure", err)
	}
}

func TestWaitReadyHonoursItsContext(t *testing.T) {
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := c.WaitReady(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitReady returned %v, want context.Canceled", err)
	}
}

func TestWaitReadyAddsTheLastErrorWhenItsContextEnds(t *testing.T) {
	// "context deadline exceeded" alone says nothing about why the connection
	// never came up; the dial error is what the user needs.
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))
	c.recordFailure(errors.New("connection refused"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := c.WaitReady(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitReady returned %v, want it to wrap context.Canceled", err)
	}
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %v, want it to name the last ssh error", err)
	}
}

func TestRecordFailureScrubsAndCounts(t *testing.T) {
	// A dial error can carry the server's banner verbatim, and that banner ends
	// up in the dashboard header and the log file.
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))

	c.recordFailure(errors.New("ssh: handshake failed: \x1b[31mbanner\x1b[0m"))
	c.recordFailure(errors.New("ssh: handshake failed again"))

	status := c.Status()
	if status.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", status.Attempts)
	}
	if status.State != StateReconnecting {
		t.Errorf("state = %q, want %q", status.State, StateReconnecting)
	}
	if strings.Contains(c.Status().LastError, "\x1b") {
		t.Errorf("LastError = %q, want the escape sequences stripped", c.Status().LastError)
	}
}

func TestSetDisconnectedClearsTheLiveConnection(t *testing.T) {
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))
	c.setConnected(nil)
	c.setRTT(7 * time.Millisecond)

	c.setDisconnected(errors.New("connection closed by remote"))

	status := c.Status()
	if status.State != StateReconnecting {
		t.Errorf("state = %q, want %q", status.State, StateReconnecting)
	}
	if status.RTT != 0 {
		t.Errorf("rtt = %s, want it cleared on disconnect", status.RTT)
	}
	if !strings.Contains(status.LastError, "closed by remote") {
		t.Errorf("LastError = %q, want the disconnect reason", status.LastError)
	}
}

func TestSetConnectedIsIdempotent(t *testing.T) {
	// setConnected closes readyCh; doing it twice without the guard would panic
	// on a double close, which a reconnect race can reach.
	c := New(&Target{Host: "127.0.0.1", Port: 22}, time.Second, slog.New(slog.DiscardHandler))

	c.setConnected(nil)
	c.setConnected(nil)
	c.setClosed()
	c.setClosed()

	if got := c.Status().Generation; got != 2 {
		t.Errorf("generation = %d, want 2", got)
	}
}

func TestWithJitterStaysWithinHalfOfTheBackoff(t *testing.T) {
	// Jitter exists so a flapping link does not resynchronise every client onto
	// the same retry instant; it must never shorten the backoff below its floor.
	const base = 2 * time.Second
	for i := 0; i < 200; i++ {
		got := withJitter(base)
		if got < base || got > base+base/2 {
			t.Fatalf("withJitter(%s) = %s, want it within [%s, %s]", base, got, base, base+base/2)
		}
	}
}
