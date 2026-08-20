package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/openurl"
	"github.com/beyto1974/auto-tunnel/internal/sanitize"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
	"github.com/beyto1974/auto-tunnel/internal/tunnel"
	"github.com/beyto1974/auto-tunnel/internal/ui"
)

// engine drives discovery and reconciliation, and publishes snapshots. It is the
// only writer of the tunnel set; the UI reads snapshots and asks for actions.
type engine struct {
	cfg     *config
	conn    *sshconn.Conn
	disc    *discovery.Discoverer
	host    *discovery.HostDiscoverer // nil when host ports are switched off
	manager *tunnel.Manager
	logger  *slog.Logger
	started time.Time
	static  []discovery.PortMap // declared with -forward, published on every poll
	// selectionPath is where the user's picks are remembered, resolved from the
	// flag and the target. Empty means they are not remembered at all.
	selectionPath string
	// prefsPath is where dashboard settings live. Unlike the picks, these are
	// not per-target: a sort order is a habit, not a property of one host.
	prefsPath string

	mu           sync.Mutex
	ctx          context.Context // lifetime handed to forwarders started outside a poll
	containers   int
	discoveryErr string
	warnings     []string
	lastScan     time.Time
	nextScan     time.Time
	lastDocker   []discovery.PortMap // last good docker poll, kept across a failing one
	lastHost     []discovery.PortMap // and the same for the host's own sockets
	lastMaps     []discovery.PortMap // what the most recent reconcile was given

	selMu    sync.Mutex
	selected map[string]bool // the user's explicit choices, by tunnel key

	// reconcileMu serialises the two callers of Reconcile: the poll loop and a
	// dashboard keypress. The manager is safe either way, but letting them
	// interleave would let one undo the other's decision halfway through.
	reconcileMu sync.Mutex

	rescan chan struct{}
}

func newEngine(cfg *config, conn *sshconn.Conn, logger *slog.Logger) *engine {
	alloc := tunnel.NewAllocator(cfg.bind, cfg.fallbackBase, tunnel.DefaultFallbackSize)
	e := &engine{
		cfg:      cfg,
		conn:     conn,
		manager:  tunnel.NewManager(tunnel.SSHDialer(conn), alloc, logger, cfg.probeHTTP),
		logger:   logger,
		started:  time.Now(),
		static:   cfg.static,
		ctx:      context.Background(),
		selected: map[string]bool{},
		rescan:   make(chan struct{}, 1),
	}

	// A host that runs no Docker at all is a legitimate target: -no-docker keeps
	// the dashboard from carrying a permanent `docker ps` failure banner while
	// host ports and declared forwards work perfectly well.
	if !cfg.noDocker {
		e.disc = discovery.New(discovery.Options{
			PSCommand:          cfg.psCommand,
			InspectCommand:     cfg.inspectCommand,
			Include:            cfg.include,
			Exclude:            cfg.exclude,
			IncludeUnpublished: cfg.includeUnpublished,
			Log:                logger,
		})
	}
	if cfg.hostMode.Enabled() {
		e.host = discovery.NewHost(discovery.HostOptions{
			Command: cfg.listenCommand,
			Exclude: cfg.hostExclude,
			// Forwarding the port we are tunnelling through is never what the
			// user meant, and it is the one port guaranteed to be listening.
			SkipPorts: map[int]bool{conn.Target().Port: true},
			Offered:   cfg.hostMode.Offered(),
			Log:       logger,
		})
	}
	e.selectionPath = resolveSelectionPath(cfg.selectionPath, conn.Target(), logger)
	if e.selectionPath != "" {
		e.selected = loadSelection(e.selectionPath, logger)
	}
	// The same switch governs both files: somebody who asked for nothing to be
	// remembered did not mean "except the sort order".
	if cfg.selectionPath != SelectionOff {
		if dir, err := os.UserConfigDir(); err == nil {
			e.prefsPath = filepath.Join(dir, "auto-tunnel", "view.json")
		}
	}
	return e
}

// SelectionOff is the -selection value that turns remembering off entirely.
const SelectionOff = "off"

// resolveSelectionPath decides where the forwarding choices live. The default
// is a file per remote target under the user's config directory: the tunnel
// keys inside are not host-qualified, so one shared file would apply one host's
// picks to every other host. Nothing is written until the user actually picks
// something, so a run that never touches the keys leaves no file behind.
func resolveSelectionPath(flag string, target *sshconn.Target, logger *slog.Logger) string {
	switch flag {
	case SelectionOff:
		return ""
	case "":
		dir, err := os.UserConfigDir()
		if err != nil {
			logger.Warn("no config directory, so forwarding choices will not be remembered", "err", err)
			return ""
		}
		return filepath.Join(dir, "auto-tunnel", selectionFileName(target.String()))
	default:
		return flag
	}
}

// selectionFileName turns a target into something safe to name a file with,
// keeping it recognisable: deploy@10.0.0.5:22 becomes deploy@10.0.0.5_22.json.
func selectionFileName(target string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_', r == '@':
			return r
		}
		return '_'
	}, target)
	// Path separators are already gone, but a name made only of dots and
	// underscores is not one worth writing either.
	if strings.Trim(safe, "._") == "" {
		safe = "default"
	}
	return safe + ".json"
}

// run polls until ctx ends, then tears every tunnel down.
func (e *engine) run(ctx context.Context) {
	defer e.manager.Close()

	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()

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

// poll runs one discovery pass over every source and reconciles the result. A
// source that fails contributes what it last reported instead of nothing, so a
// hiccup talking to Docker never drops working forwards — and never takes the
// declared forwards, which depend on no remote command at all, down with it.
func (e *engine) poll(ctx context.Context) {
	client := e.conn.Client()
	if client == nil {
		e.setDiscoveryError("")
		return
	}

	dockerMaps, containers, warnings, ok := e.pollDocker(ctx, client)
	if ctx.Err() != nil {
		return
	}
	hostMaps, hostWarnings := e.pollHost(ctx, client)
	warnings = append(warnings, hostWarnings...)

	maps := make([]discovery.PortMap, 0, len(dockerMaps)+len(hostMaps)+len(e.static))
	maps = append(maps, dockerMaps...)
	maps = append(maps, dropDockerPublished(dockerMaps, hostMaps)...)
	maps = append(maps, e.static...)
	maps, warnings = dedupeKeys(maps, warnings)

	warnings = sanitize.Strings(warnings)
	for _, w := range warnings {
		e.logger.Warn("discovery warning", "detail", w)
	}

	e.reconcile(ctx, maps)

	e.mu.Lock()
	e.containers = containers
	e.warnings = warnings
	if ok {
		e.discoveryErr = ""
		e.lastScan = time.Now()
	}
	e.mu.Unlock()
}

// pollDocker runs one Docker pass, reporting whether it succeeded. On failure
// it returns the last good answer: the containers are almost certainly still
// running, and rebinding every local port over a momentary `docker ps` failure
// would be worse than a stale row.
func (e *engine) pollDocker(ctx context.Context, client *ssh.Client) (maps []discovery.PortMap, containers int, warnings []string, ok bool) {
	if e.disc == nil {
		return nil, 0, nil, true
	}
	result, err := e.disc.Discover(ctx, client)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, nil, false
		}
		// Remote stderr reaches this message, so it is scrubbed before it can
		// reach a terminal via the banner or the log pane.
		e.setDiscoveryError(sanitize.Error(err))
		e.logger.Warn("discovery failed", "err", e.discoveryError())
		cached, count := e.cachedDocker()
		return cached, count, nil, false
	}

	e.mu.Lock()
	e.lastDocker = result.Maps
	e.mu.Unlock()
	return result.Maps, result.Containers, result.Warnings, true
}

func (e *engine) cachedDocker() ([]discovery.PortMap, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastDocker, e.containers
}

// pollHost asks the remote host what it is listening on. A failure here is a
// warning rather than an error: the last good answer is reused so a host that
// briefly cannot run `ss` does not drop every host tunnel and rebind it a
// second later, and Docker discovery carries on regardless.
func (e *engine) pollHost(ctx context.Context, client *ssh.Client) ([]discovery.PortMap, []string) {
	if e.host == nil {
		return nil, nil
	}
	result, err := e.host.Discover(ctx, client)
	if err != nil {
		if ctx.Err() != nil {
			return e.cachedHost(), nil
		}
		e.logger.Warn("host port discovery failed", "err", sanitize.Error(err))
		return e.cachedHost(), []string{sanitize.Error(err)}
	}
	e.mu.Lock()
	e.lastHost = result.Maps
	e.mu.Unlock()
	return result.Maps, result.Warnings
}

func (e *engine) cachedHost() []discovery.PortMap {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastHost
}

// dropDockerPublished removes the host sockets that are really Docker's own
// published ports. docker-proxy listens on every published port, so without
// this each container port would appear twice: once as a container row and
// once as a nameless host row forwarding to the same place.
func dropDockerPublished(docker, host []discovery.PortMap) []discovery.PortMap {
	if len(host) == 0 {
		return nil
	}
	taken := make(map[int]bool, len(docker))
	for _, pm := range docker {
		if pm.Published() {
			taken[pm.HostPort] = true
		}
	}
	kept := make([]discovery.PortMap, 0, len(host))
	for _, pm := range host {
		if taken[pm.ContainerPort] {
			continue
		}
		kept = append(kept, pm)
	}
	return kept
}

// dedupeKeys keeps the first candidate for each tunnel key. Discovery cannot
// produce a collision on its own, but two -forward specs sharing a label and a
// port can, and silently forwarding only one of them would be a puzzle.
func dedupeKeys(maps []discovery.PortMap, warnings []string) ([]discovery.PortMap, []string) {
	seen := make(map[string]bool, len(maps))
	kept := maps[:0]
	for _, pm := range maps {
		key := pm.Key()
		if seen[key] {
			warnings = append(warnings, "ignoring a second forward for "+key)
			continue
		}
		seen[key] = true
		kept = append(kept, pm)
	}
	return kept, warnings
}

// reconcile applies the user's choices to the discovered set and hands it to
// the manager, remembering the set so a keypress can re-apply it without
// waiting for the next poll.
func (e *engine) reconcile(ctx context.Context, maps []discovery.PortMap) {
	e.mu.Lock()
	e.lastMaps = maps
	e.mu.Unlock()

	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	e.manager.Reconcile(ctx, e.applySelection(maps))
}

// reapply reconciles the last discovered set again, which is how a selection
// change takes effect immediately instead of at the next poll.
func (e *engine) reapply() {
	e.mu.Lock()
	maps, ctx := e.lastMaps, e.ctx
	e.mu.Unlock()
	if len(maps) == 0 {
		return
	}
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	e.manager.Reconcile(ctx, e.applySelection(maps))
}

// applySelection overrides each candidate's default with the user's choice, on
// a copy: the stored set has to stay as discovery reported it.
func (e *engine) applySelection(maps []discovery.PortMap) []discovery.PortMap {
	out := make([]discovery.PortMap, len(maps))
	copy(out, maps)

	e.selMu.Lock()
	defer e.selMu.Unlock()
	for i := range out {
		if on, chosen := e.selected[out[i].Key()]; chosen {
			out[i].Offered = !on
		}
	}
	return out
}

// Rescan requests an immediate poll. It never blocks: a request already queued
// is as good as a second one.
func (e *engine) Rescan() {
	select {
	case e.rescan <- struct{}{}:
	default:
	}
}

// Prefs returns the dashboard settings remembered from earlier runs. A missing
// or unreadable file is a first run, not an error worth refusing to start over.
func (e *engine) Prefs() ui.Prefs {
	var prefs ui.Prefs
	if e.prefsPath == "" {
		return prefs
	}
	data, err := os.ReadFile(e.prefsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			e.logger.Warn("could not read the dashboard preferences", "path", e.prefsPath, "err", err)
		}
		return ui.Prefs{}
	}
	if err := json.Unmarshal(data, &prefs); err != nil {
		e.logger.Warn("could not parse the dashboard preferences", "path", e.prefsPath, "err", err)
		return ui.Prefs{}
	}
	return prefs
}

// SavePrefs remembers the dashboard settings for the next run.
func (e *engine) SavePrefs(prefs ui.Prefs) {
	if e.prefsPath == "" {
		return
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		e.logger.Warn("could not encode the dashboard preferences", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(e.prefsPath), 0o700); err != nil {
		e.logger.Warn("could not create the preferences directory", "err", err)
		return
	}
	// 0600 for consistency with its neighbours, though this file names nothing.
	if err := os.WriteFile(e.prefsPath, append(data, '\n'), 0o600); err != nil {
		e.logger.Warn("could not write the dashboard preferences", "path", e.prefsPath, "err", err)
	}
}

// OpenURL hands a forwarded service to the desktop's browser. The dashboard
// asks for this rather than running a command itself, so the UI stays a
// renderer.
func (e *engine) OpenURL(rawURL string) error {
	e.logger.Info("opening a forwarded service", "url", rawURL)
	return openurl.Open(rawURL)
}

// TogglePause flips whether a tunnel accepts new connections.
func (e *engine) TogglePause(key string) (paused, ok bool) {
	return e.manager.TogglePause(key)
}

// SetEnabled starts or stops forwarding one discovered port, reporting whether
// the key is one the engine knows about.
func (e *engine) SetEnabled(key string, on bool) bool {
	if !e.known(key) {
		return false
	}
	e.selMu.Lock()
	e.selected[key] = on
	e.selMu.Unlock()

	e.logger.Info("tunnel enablement changed", "tunnel", key, "enabled", on)
	e.saveSelection()
	e.reapply()
	return true
}

// SetEnabledAll applies one choice to a set of keys, returning how many it
// changed. The dashboard passes the rows it is currently showing, so the bulk
// keys act on exactly what the user can see.
func (e *engine) SetEnabledAll(keys []string, on bool) int {
	known := map[string]bool{}
	e.mu.Lock()
	for _, pm := range e.lastMaps {
		known[pm.Key()] = true
	}
	e.mu.Unlock()

	changed := 0
	e.selMu.Lock()
	for _, key := range keys {
		if !known[key] {
			continue
		}
		if prev, chosen := e.selected[key]; chosen && prev == on {
			continue
		}
		e.selected[key] = on
		changed++
	}
	e.selMu.Unlock()

	if changed == 0 {
		return 0
	}
	e.logger.Info("bulk enablement changed", "tunnels", changed, "enabled", on)
	e.saveSelection()
	e.reapply()
	return changed
}

func (e *engine) known(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pm := range e.lastMaps {
		if pm.Key() == key {
			return true
		}
	}
	return false
}

// loadSelection reads the remembered choices. A missing file is the normal
// first run, and an unreadable one is not worth refusing to start over.
func loadSelection(path string, logger *slog.Logger) map[string]bool {
	selected := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("could not read the selection file", "path", path, "err", err)
		}
		return selected
	}
	if err := json.Unmarshal(data, &selected); err != nil {
		logger.Warn("could not parse the selection file", "path", path, "err", err)
		return map[string]bool{}
	}
	return selected
}

// saveSelection writes the choices back. 0600 for the same reason as the log
// file: the keys name every container and service found on the remote host, and
// 0700 on the directory for the same reason again.
func (e *engine) saveSelection() {
	if e.selectionPath == "" {
		return
	}
	e.selMu.Lock()
	data, err := json.MarshalIndent(e.selected, "", "  ")
	e.selMu.Unlock()
	if err != nil {
		e.logger.Warn("could not encode the selection file", "err", err)
		return
	}
	if dir := filepath.Dir(e.selectionPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.logger.Warn("could not create the selection directory", "path", dir, "err", err)
			return
		}
	}
	if err := os.WriteFile(e.selectionPath, append(data, '\n'), 0o600); err != nil {
		e.logger.Warn("could not write the selection file", "path", e.selectionPath, "err", err)
	}
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

func (e *engine) discoveryError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.discoveryErr
}

func (e *engine) setNextScan(t time.Time) {
	e.mu.Lock()
	e.nextScan = t
	e.mu.Unlock()
}
