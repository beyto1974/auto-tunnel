package sshconn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/sanitize"
)

const (
	keepaliveInterval = 15 * time.Second
	keepaliveTimeout  = 10 * time.Second
	keepaliveFailsMax = 2
	minBackoff        = 1 * time.Second
	maxBackoff        = 30 * time.Second
)

// timings are the intervals Run works to. They are a field rather than the
// constants above so tests can drive the reconnect and keepalive paths in
// milliseconds instead of waiting out a real 15-second cycle.
type timings struct {
	keepaliveInterval time.Duration
	keepaliveTimeout  time.Duration
	minBackoff        time.Duration
	maxBackoff        time.Duration
}

func defaultTimings() timings {
	return timings{
		keepaliveInterval: keepaliveInterval,
		keepaliveTimeout:  keepaliveTimeout,
		minBackoff:        minBackoff,
		maxBackoff:        maxBackoff,
	}
}

// State is the lifecycle of the managed SSH connection.
type State string

const (
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateClosed       State = "closed"
)

// Status is a point-in-time snapshot, safe to copy and hand to the UI.
type Status struct {
	State      State
	RTT        time.Duration
	Since      time.Time // when the current state began
	LastError  string
	Attempts   int    // consecutive failed dial attempts
	Generation uint64 // bumped on every successful connect
}

// Conn maintains a single SSH connection, reconnecting with backoff for as long
// as Run's context lives. Callers never hold a *ssh.Client across a drop; they
// ask for the current one each time they need to dial.
type Conn struct {
	target  *Target
	timeout time.Duration
	log     *slog.Logger
	timings timings

	mu      sync.RWMutex
	client  *ssh.Client
	status  Status
	readyCh chan struct{} // closed while connected, fresh and open while not
}

// New creates a Conn. Nothing is dialed until Run is called.
func New(t *Target, timeout time.Duration, log *slog.Logger) *Conn {
	if log == nil {
		log = slog.Default()
	}
	return &Conn{
		target:  t,
		timeout: timeout,
		log:     log,
		timings: defaultTimings(),
		status:  Status{State: StateConnecting, Since: time.Now()},
		readyCh: make(chan struct{}),
	}
}

// Target returns the resolved destination.
func (c *Conn) Target() *Target { return c.target }

// Run blocks until ctx is cancelled, keeping the connection alive throughout.
func (c *Conn) Run(ctx context.Context) {
	defer c.setClosed()

	backoff := c.timings.minBackoff
	for ctx.Err() == nil {
		client, err := Dial(c.target, c.timeout)
		if err != nil {
			wait := withJitter(backoff)
			c.recordFailure(err)
			c.log.Warn("ssh connect failed", "target", c.target.String(), "err", err, "retry_in", wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, c.timings.maxBackoff)
			continue
		}

		backoff = c.timings.minBackoff
		c.setConnected(client)
		c.log.Info("ssh connected", "target", c.target.String())

		reason := c.monitor(ctx, client)
		client.Close()
		c.setDisconnected(reason)
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("ssh connection lost", "target", c.target.String(), "reason", reason)
	}
}

// monitor pings until the connection dies or ctx ends, returning why.
func (c *Conn) monitor(ctx context.Context, client *ssh.Client) error {
	closed := make(chan error, 1)
	go func() { closed <- client.Wait() }()

	ticker := time.NewTicker(c.timings.keepaliveInterval)
	defer ticker.Stop()

	fails := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-closed:
			if err == nil {
				return errors.New("connection closed by remote")
			}
			return err
		case <-ticker.C:
			rtt, err := ping(client, c.timings.keepaliveTimeout)
			if err != nil {
				fails++
				c.log.Debug("keepalive failed", "attempt", fails, "err", err)
				if fails >= keepaliveFailsMax {
					return fmt.Errorf("keepalive failed %d times: %w", fails, err)
				}
				continue
			}
			fails = 0
			c.setRTT(rtt)
		}
	}
}

// ping measures round-trip time, bounded so a black-holed network still fails fast.
func ping(client *ssh.Client, timeout time.Duration) (time.Duration, error) {
	type result struct {
		rtt time.Duration
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- result{time.Since(start), err}
	}()
	select {
	case r := <-done:
		return r.rtt, r.err
	case <-time.After(timeout):
		return 0, fmt.Errorf("keepalive timed out after %s", timeout)
	}
}

// Client returns the live client, or nil while disconnected.
func (c *Conn) Client() *ssh.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// Status returns a copy of the current connection status.
func (c *Conn) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// WaitReady blocks until the connection is up, ctx ends, or Run exits.
func (c *Conn) WaitReady(ctx context.Context) (*ssh.Client, error) {
	for {
		c.mu.RLock()
		client, ready, state := c.client, c.readyCh, c.status.State
		lastErr := c.status.LastError
		c.mu.RUnlock()

		if client != nil {
			return client, nil
		}
		if state == StateClosed {
			if lastErr != "" {
				return nil, fmt.Errorf("ssh connection closed: %s", lastErr)
			}
			return nil, errors.New("ssh connection closed")
		}
		select {
		case <-ready:
		case <-ctx.Done():
			if lastErr != "" {
				return nil, fmt.Errorf("%w (last ssh error: %s)", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		}
	}
}

func (c *Conn) setConnected(client *ssh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = client
	c.status.State = StateConnected
	c.status.Since = time.Now()
	c.status.Attempts = 0
	c.status.LastError = ""
	c.status.Generation++
	select {
	case <-c.readyCh: // already closed
	default:
		close(c.readyCh)
	}
}

func (c *Conn) setDisconnected(reason error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = nil
	c.status.State = StateReconnecting
	c.status.Since = time.Now()
	c.status.RTT = 0
	if reason != nil {
		// A dial failure can carry the server's banner, so it is scrubbed
		// before the UI renders it.
		c.status.LastError = sanitize.Error(reason)
	}
	c.readyCh = make(chan struct{})
}

func (c *Conn) recordFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = nil
	if c.status.State == StateConnected {
		c.status.Since = time.Now()
	}
	c.status.State = StateReconnecting
	c.status.Attempts++
	if err != nil {
		c.status.LastError = sanitize.Error(err)
	}
}

func (c *Conn) setRTT(rtt time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.RTT = rtt
}

func (c *Conn) setClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
	c.status.State = StateClosed
	c.status.Since = time.Now()
	select {
	case <-c.readyCh:
	default:
		close(c.readyCh) // unblock any WaitReady so it can observe StateClosed
	}
}

// withJitter spreads reconnect attempts so a flapping link does not resync.
func withJitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d/2)+1))
}
