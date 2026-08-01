package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mustiko/auto-tunnel/internal/discovery"
	"github.com/mustiko/auto-tunnel/internal/sshconn"
	"github.com/mustiko/auto-tunnel/internal/state"
	"github.com/mustiko/auto-tunnel/internal/tunnel"
)

// engine drives discovery and reconciliation, and publishes snapshots. It is the
// only writer of the tunnel set; the UI reads snapshots and asks for actions.
type engine struct {
	cfg     *config
	conn    *sshconn.Conn
	disc    *discovery.Discoverer
	manager *tunnel.Manager
	logger  *slog.Logger
	started time.Time

	mu           sync.Mutex
	containers   int
	discoveryErr string
	warnings     []string
	lastScan     time.Time
	nextScan     time.Time

	rescan chan struct{}
}

func newEngine(cfg *config, conn *sshconn.Conn, logger *slog.Logger) *engine {
	alloc := tunnel.NewAllocator(cfg.bind, cfg.fallbackBase, tunnel.DefaultFallbackSize)
	return &engine{
		cfg:  cfg,
		conn: conn,
		disc: discovery.New(discovery.Options{
			PSCommand:          cfg.psCommand,
			InspectCommand:     cfg.inspectCommand,
			Include:            cfg.include,
			Exclude:            cfg.exclude,
			IncludeUnpublished: cfg.includeUnpublished,
			Log:                logger,
		}),
		manager: tunnel.NewManager(tunnel.SSHDialer(conn), alloc, logger),
		logger:  logger,
		started: time.Now(),
		rescan:  make(chan struct{}, 1),
	}
}

// run polls until ctx ends, then tears every tunnel down.
func (e *engine) run(ctx context.Context) {
	defer e.manager.Close()

	ticker := time.NewTicker(e.cfg.interval)
	defer ticker.Stop()

	for {
		e.poll(ctx)
		e.setNextScan(time.Now().Add(e.cfg.interval))

		select {
		case <-ticker.C:
		case <-e.rescan:
			ticker.Reset(e.cfg.interval)
		case <-ctx.Done():
			return
		}
	}
}

// poll runs one discovery pass and reconciles the result. A failure leaves the
// existing tunnels alone: a hiccup talking to Docker is not a reason to drop
// working forwards.
func (e *engine) poll(ctx context.Context) {
	client := e.conn.Client()
	if client == nil {
		e.setDiscoveryError("")
		return
	}

	result, err := e.disc.Discover(ctx, client)
	if err != nil {
		if ctx.Err() == nil {
			e.logger.Warn("discovery failed", "err", err)
			e.setDiscoveryError(err.Error())
		}
		return
	}
	for _, w := range result.Warnings {
		e.logger.Warn("discovery warning", "detail", w)
	}

	e.manager.Reconcile(ctx, result.Maps)

	e.mu.Lock()
	e.containers = result.Containers
	e.warnings = result.Warnings
	e.discoveryErr = ""
	e.lastScan = time.Now()
	e.mu.Unlock()
}

// Rescan requests an immediate poll. It never blocks: a request already queued
// is as good as a second one.
func (e *engine) Rescan() {
	select {
	case e.rescan <- struct{}{}:
	default:
	}
}

// TogglePause flips whether a tunnel accepts new connections.
func (e *engine) TogglePause(key string) (paused, ok bool) {
	return e.manager.TogglePause(key)
}

// snapshot builds the read-only view handed to the renderer.
func (e *engine) snapshot() state.Snapshot {
	e.mu.Lock()
	snap := state.Snapshot{
		Containers:     e.containers,
		DiscoveryError: e.discoveryErr,
		Warnings:       append([]string(nil), e.warnings...),
		LastScan:       e.lastScan,
		NextScan:       e.nextScan,
	}
	e.mu.Unlock()

	snap.Target = e.conn.Target().String()
	snap.SSH = e.conn.Status()
	snap.Tunnels = e.manager.Tunnels()
	snap.Started = e.started
	return snap
}

func (e *engine) setDiscoveryError(msg string) {
	e.mu.Lock()
	e.discoveryErr = msg
	e.mu.Unlock()
}

func (e *engine) setNextScan(t time.Time) {
	e.mu.Lock()
	e.nextScan = t
	e.mu.Unlock()
}
