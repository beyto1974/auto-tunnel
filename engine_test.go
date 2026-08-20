package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/sshtest"
	"github.com/beyto1974/auto-tunnel/internal/state"
	"github.com/beyto1974/auto-tunnel/internal/ui"
)

// TestMain points HOME at an empty directory. The engine dials through
// sshconn.Dial, which reads ~/.ssh/known_hosts, and the fixture writes the entry
// it needs there — without this the suite would touch the developer's own SSH
// configuration and behave differently on every machine.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "auto-tunnel-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp home: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	// The engine now saves forwarding choices under the user's config directory
	// by default; without this the suite would write into the developer's own.
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	code := m.Run()
	os.RemoveAll(home) // os.Exit skips deferred calls
	os.Exit(code)
}

const containerID = "0123456789ab"

// dockerPS renders a `docker ps --format '{{json .}}'` line.
func dockerPS(name, ports string) string {
	return fmt.Sprintf(`{"ID":%q,"Names":%q,"Image":"nginx:alpine","Ports":%q,"State":"running"}`+"\n",
		containerID, name, ports)
}

func testConfig(t *testing.T) *config {
	t.Helper()
	return &config{
		interval:       50 * time.Millisecond,
		bind:           "127.0.0.1",
		fallbackBase:   freeLocalPort(t),
		psCommand:      discovery.DefaultPSCommand,
		inspectCommand: discovery.DefaultInspectCommand,
	}
}

// testEngine wires an engine to a live fixture over a real SSH connection, so
// discovery, the manager, and the connection all behave as they do in
// production. The returned engine is ready to poll.
func testEngine(t *testing.T, cfg *config, exec func(cmd string) sshtest.ExecResult) (*engine, *sshtest.Server) {
	t.Helper()

	srv := sshtest.New(t)
	sshtest.Trust(t, srv)
	if exec != nil {
		srv.SetExec(exec)
	}

	target := &sshconn.Target{
		Spec:          srv.Addr(),
		Alias:         "127.0.0.1",
		User:          "tester",
		Host:          "127.0.0.1",
		Port:          srv.Port(),
		IdentityFiles: []string{srv.KeyPath()},
	}

	logger := slog.New(slog.DiscardHandler)
	conn := sshconn.New(target, 5*time.Second, logger)

	ctx, cancel := context.WithCancel(t.Context())
	connDone := make(chan struct{})
	go func() { defer close(connDone); conn.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-connDone })

	if _, err := conn.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	eng := newEngine(cfg, conn, logger)
	t.Cleanup(eng.manager.Close)
	return eng, srv
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForSnapshot polls until cond holds. The manager starts forwarders in the
// background, so a row appears a moment after Reconcile returns.
func waitForSnapshot(t *testing.T, eng *engine, what string, cond func(state.Snapshot) bool) state.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last state.Snapshot
	for time.Now().Before(deadline) {
		last = eng.snapshot()
		if cond(last) {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the engine to %s; last snapshot: %+v", what, last)
	return last
}

func TestEnginePollPublishesWhatItFound(t *testing.T) {
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(cmd string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	eng.poll(t.Context())

	snap := waitForSnapshot(t, eng, "bind the discovered port", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 1 && s.Tunnels[0].LocalPort != 0
	})

	if snap.Containers != 1 {
		t.Errorf("Containers = %d, want 1", snap.Containers)
	}
	if snap.DiscoveryError != "" {
		t.Errorf("DiscoveryError = %q, want none", snap.DiscoveryError)
	}
	if snap.LastScan.IsZero() {
		t.Error("LastScan is zero after a successful poll")
	}
	if snap.SSH.State != sshconn.StateConnected {
		t.Errorf("ssh state = %q, want %q", snap.SSH.State, sshconn.StateConnected)
	}
	if !strings.Contains(snap.Target, "tester@127.0.0.1") {
		t.Errorf("Target = %q, want the resolved destination", snap.Target)
	}
	if snap.Started.IsZero() {
		t.Error("Started is zero; the header renders an uptime from it")
	}
	if got := snap.Tunnels[0]; got.Name != "web" || got.ContainerPort != 80 {
		t.Errorf("tunnel = %+v, want the discovered web container", got)
	}
}

func TestEnginePollWaitsForTheSSHConnection(t *testing.T) {
	// Discovery cannot run without a client, and a poll in that window must not
	// invent a discovery error — the banner would blame Docker for an SSH outage.
	cfg := testConfig(t)
	logger := slog.New(slog.DiscardHandler)
	conn := sshconn.New(&sshconn.Target{User: "tester", Host: "127.0.0.1", Port: 22}, time.Second, logger)
	eng := newEngine(cfg, conn, logger)
	t.Cleanup(eng.manager.Close)

	eng.poll(t.Context())

	if got := eng.discoveryError(); got != "" {
		t.Errorf("discoveryError = %q, want none while ssh is still connecting", got)
	}
	if got := eng.snapshot(); len(got.Tunnels) != 0 {
		t.Errorf("tunnels = %+v, want none", got.Tunnels)
	}
}

func TestEnginePollScrubsAFailedDiscovery(t *testing.T) {
	// The message carries remote stderr, and it reaches a terminal through the
	// banner and the log pane.
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{
			Stderr: "\x1b[31mpermission denied\x1b[0m while trying to connect to the Docker daemon",
			Exit:   1,
		}
	})

	eng.poll(t.Context())

	got := eng.discoveryError()
	if got == "" {
		t.Fatal("discoveryError is empty after a failed poll")
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("discoveryError = %q, want the remote reason", got)
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("discoveryError = %q, want the escape sequences stripped", got)
	}
}

func TestEnginePollKeepsRunningTunnelsThroughAFailure(t *testing.T) {
	// A hiccup talking to Docker is not a reason to drop working forwards.
	cfg := testConfig(t)
	var fail bool
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		if fail {
			return sshtest.ExecResult{Stderr: "docker daemon not responding", Exit: 1}
		}
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	eng.poll(t.Context())
	before := waitForSnapshot(t, eng, "start a tunnel", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 1 && s.Tunnels[0].LocalPort != 0
	})

	fail = true
	eng.poll(t.Context())

	after := eng.snapshot()
	if len(after.Tunnels) != 1 {
		t.Fatalf("tunnels = %+v, want the existing one kept", after.Tunnels)
	}
	if after.Tunnels[0].LocalPort != before.Tunnels[0].LocalPort {
		t.Errorf("local port moved from %d to %d across a failed poll",
			before.Tunnels[0].LocalPort, after.Tunnels[0].LocalPort)
	}
	if after.DiscoveryError == "" {
		t.Error("DiscoveryError is empty after the failing poll")
	}
}

func TestEnginePollSurfacesDiscoveryWarnings(t *testing.T) {
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "\x1b[31mnonsense\x1b[0m")}
	})

	eng.poll(t.Context())

	snap := eng.snapshot()
	if len(snap.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", snap.Warnings)
	}
	if strings.ContainsRune(snap.Warnings[0], '\x1b') {
		t.Errorf("warning = %q, want it scrubbed before it can reach a terminal", snap.Warnings[0])
	}
}

func TestSnapshotWarningsAreACopy(t *testing.T) {
	// The renderer holds a snapshot while the next poll runs; sharing the slice
	// would be a data race and a rewritten history.
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "nonsense")}
	})
	eng.poll(t.Context())

	snap := eng.snapshot()
	if len(snap.Warnings) == 0 {
		t.Fatal("no warnings to copy")
	}
	snap.Warnings[0] = "mutated by the caller"

	if got := eng.snapshot().Warnings[0]; got == "mutated by the caller" {
		t.Error("snapshot shares the warnings slice with the engine")
	}
}

func TestEngineRescanNeverBlocks(t *testing.T) {
	// Rescan is called from the UI goroutine; a full channel must be treated as
	// "a request is already queued", not as a reason to stall the dashboard.
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			eng.Rescan()
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Rescan blocked with a request already queued")
	}
}

func TestEngineTogglePauseReachesTheManager(t *testing.T) {
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "start a tunnel", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 1 && s.Tunnels[0].LocalPort != 0
	})

	if paused, ok := eng.TogglePause(snap.Tunnels[0].Key); !ok || !paused {
		t.Errorf("TogglePause = (%v, %v), want (true, true)", paused, ok)
	}
	if paused, ok := eng.TogglePause(snap.Tunnels[0].Key); !ok || paused {
		t.Errorf("TogglePause again = (%v, %v), want (false, true)", paused, ok)
	}
	if _, ok := eng.TogglePause("nothing:80/tcp"); ok {
		t.Error("TogglePause accepted an unknown key")
	}
}

func TestEngineRunPollsUntilItsContextEndsThenTearsDown(t *testing.T) {
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); eng.run(ctx) }()

	snap := waitForSnapshot(t, eng, "poll and bind a port", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 1 && s.Tunnels[0].LocalPort != 0
	})
	if snap.NextScan.IsZero() {
		t.Error("NextScan is zero while run is polling; the header counts down from it")
	}

	// An immediate rescan resets the ticker rather than waiting out the interval.
	eng.Rescan()
	waitForSnapshot(t, eng, "poll again after a rescan", func(s state.Snapshot) bool {
		return s.LastScan.After(snap.LastScan)
	})

	port := snap.Tunnels[0].LocalPort
	cancel()
	<-done

	// run defers manager.Close, so the forwarded port has to be released.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("local port %d is still bound after run returned: %v", port, err)
	}
	ln.Close()
}

func TestNewEngineAppliesTheConfiguredFilters(t *testing.T) {
	cfg := testConfig(t)
	cfg.exclude = regexp.MustCompile(`^web`)
	eng, _ := testEngine(t, cfg, func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	eng.poll(t.Context())

	if got := eng.snapshot(); got.Containers != 0 || len(got.Tunnels) != 0 {
		t.Errorf("snapshot = %+v, want the excluded container filtered out", got)
	}
}

// ssLines renders the output of the default listen command.
const ssListening = `LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=812,fd=7))
LISTEN 0 4096 0.0.0.0:8080 0.0.0.0:* users:(("docker-proxy",pid=99,fd=4))
`

// remoteExec answers the docker and socket commands from fixed strings, the way
// a real host answers two different questions over one connection.
func remoteExec(dockerOut, listenOut string) func(string) sshtest.ExecResult {
	return func(cmd string) sshtest.ExecResult {
		switch {
		case strings.HasPrefix(cmd, "docker ps"):
			return sshtest.ExecResult{Stdout: dockerOut}
		case strings.Contains(cmd, "ss -Hltnp"):
			return sshtest.ExecResult{Stdout: listenOut}
		default:
			return sshtest.ExecResult{Stderr: "unexpected command: " + cmd, Exit: 127}
		}
	}
}

func rowNamed(snap state.Snapshot, name string) (state.Tunnel, bool) {
	for _, t := range snap.Tunnels {
		if t.Name == name {
			return t, true
		}
	}
	return state.Tunnel{}, false
}

func TestEngineForwardsHostPortsBesideContainerPorts(t *testing.T) {
	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsAll
	cfg.listenCommand = discovery.DefaultListenCommand
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())

	snap := waitForSnapshot(t, eng, "bind both sources", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 2
	})

	web, ok := rowNamed(snap, "web")
	if !ok || web.Source != string(discovery.SourceDocker) {
		t.Fatalf("tunnels = %+v, want the container row", snap.Tunnels)
	}
	pg, ok := rowNamed(snap, "postgres")
	if !ok {
		t.Fatalf("tunnels = %+v, want the host socket row", snap.Tunnels)
	}
	if pg.Source != string(discovery.SourceHost) || pg.RemoteTarget != "127.0.0.1:5432" {
		t.Errorf("postgres row = %+v, want a host row dialed on the remote loopback", pg)
	}
	if pg.LocalPort == 0 {
		t.Error("host port was discovered but never bound in -host-ports all")
	}
}

func TestEngineDropsHostRowsThatAreReallyDockerPublishedPorts(t *testing.T) {
	// docker-proxy listens on every published port, so 8080 shows up in both
	// answers. Two rows for one service would race for the same local port.
	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsAll
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "reconcile both sources", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 2
	})

	for _, tn := range snap.Tunnels {
		if tn.Source == string(discovery.SourceHost) && tn.ContainerPort == 8080 {
			t.Errorf("tunnels = %+v, want docker's own published port dropped from the host rows", snap.Tunnels)
		}
	}
}

func TestEngineSelectModeListsHostPortsUntilTheUserAsks(t *testing.T) {
	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsSelect
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "list the host port", func(s state.Snapshot) bool {
		_, ok := rowNamed(s, "postgres")
		return ok
	})

	pg, _ := rowNamed(snap, "postgres")
	if pg.State != state.TunnelOffered || pg.LocalPort != 0 {
		t.Fatalf("postgres row = %+v, want it listed without a local port", pg)
	}
	if pg.Enabled {
		t.Error("a row nobody asked for reports itself as forwarded")
	}

	// The choice takes effect at once, without waiting for the next poll.
	if !eng.SetEnabled(pg.Key, true) {
		t.Fatalf("SetEnabled(%q) rejected a key from the snapshot", pg.Key)
	}
	bound := waitForSnapshot(t, eng, "bind the enabled host port", func(s state.Snapshot) bool {
		row, ok := rowNamed(s, "postgres")
		return ok && row.LocalPort != 0
	})
	row, _ := rowNamed(bound, "postgres")
	if row.State == state.TunnelOffered || !row.Enabled {
		t.Errorf("postgres row = %+v, want it forwarded after being enabled", row)
	}

	// And switching it back off releases the port again.
	if !eng.SetEnabled(pg.Key, false) {
		t.Fatal("SetEnabled could not switch the row back off")
	}
	waitForSnapshot(t, eng, "release the port again", func(s state.Snapshot) bool {
		r, ok := rowNamed(s, "postgres")
		return ok && r.LocalPort == 0
	})
}

func TestEngineSetEnabledRejectsAnUnknownKey(t *testing.T) {
	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ""))
	eng.poll(t.Context())

	if eng.SetEnabled("host:host:9999/tcp", true) {
		t.Error("SetEnabled accepted a key the engine never discovered")
	}
}

func TestEngineSetEnabledAllChangesOnlyTheKeysItIsGiven(t *testing.T) {
	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsSelect
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "list both sources", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 2
	})
	pg, _ := rowNamed(snap, "postgres")

	if got := eng.SetEnabledAll([]string{pg.Key, "nothing:here:1/tcp"}, true); got != 1 {
		t.Errorf("SetEnabledAll changed %d rows, want only the known one", got)
	}
	waitForSnapshot(t, eng, "bind the enabled host port", func(s state.Snapshot) bool {
		row, ok := rowNamed(s, "postgres")
		return ok && row.LocalPort != 0
	})

	// Asking again for what is already true changes nothing.
	if got := eng.SetEnabledAll([]string{pg.Key}, true); got != 0 {
		t.Errorf("SetEnabledAll changed %d rows, want none: the choice was already made", got)
	}
}

func TestEngineDeclaredForwardsSurviveADockerFailure(t *testing.T) {
	// A declared forward depends on no remote command, so nothing a failing
	// `docker ps` can do should take it away.
	cfg := testConfig(t)
	var fail bool
	cfg.static = mustForward(t, "db=5432")
	eng, _ := testEngine(t, cfg, func(cmd string) sshtest.ExecResult {
		if fail {
			return sshtest.ExecResult{Stderr: "docker daemon not responding", Exit: 1}
		}
		return sshtest.ExecResult{Stdout: dockerPS("web", "0.0.0.0:8080->80/tcp")}
	})

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "bind the declared forward", func(s state.Snapshot) bool {
		row, ok := rowNamed(s, "db")
		return ok && row.LocalPort != 0
	})
	before, _ := rowNamed(snap, "db")

	fail = true
	eng.poll(t.Context())

	after := eng.snapshot()
	db, ok := rowNamed(after, "db")
	if !ok {
		t.Fatalf("tunnels = %+v, want the declared forward kept", after.Tunnels)
	}
	if db.LocalPort != before.LocalPort {
		t.Errorf("declared forward moved from %d to %d across a failed docker poll", before.LocalPort, db.LocalPort)
	}
	if _, ok := rowNamed(after, "web"); !ok {
		t.Error("the container row was dropped by a failing poll")
	}
	if after.DiscoveryError == "" {
		t.Error("DiscoveryError is empty after the failing poll")
	}
}

func TestEngineWithoutDockerForwardsOnlyTheOtherSources(t *testing.T) {
	cfg := testConfig(t)
	cfg.noDocker = true
	cfg.hostMode = discovery.HostPortsAll
	cfg.static = mustForward(t, "db=3306")
	eng, _ := testEngine(t, cfg, func(cmd string) sshtest.ExecResult {
		if strings.HasPrefix(cmd, "docker ps") {
			t.Errorf("docker was polled under -no-docker: %q", cmd)
			return sshtest.ExecResult{Exit: 127}
		}
		return sshtest.ExecResult{Stdout: ssListening}
	})

	eng.poll(t.Context())

	snap := waitForSnapshot(t, eng, "bind the host and declared rows", func(s state.Snapshot) bool {
		return len(s.Tunnels) == 3 // postgres, docker-proxy's 8080, and db
	})
	if snap.DiscoveryError != "" {
		t.Errorf("DiscoveryError = %q, want none when docker is not being polled", snap.DiscoveryError)
	}
	for _, name := range []string{"postgres", "db"} {
		if row, ok := rowNamed(snap, name); !ok || row.LocalPort == 0 {
			t.Errorf("row %q = %+v, want it bound", name, row)
		}
	}
}

func TestEngineRemembersTheSelectionBetweenRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")

	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsSelect
	cfg.selectionPath = path
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "list the host port", func(s state.Snapshot) bool {
		_, ok := rowNamed(s, "postgres")
		return ok
	})
	pg, _ := rowNamed(snap, "postgres")
	eng.SetEnabled(pg.Key, true)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat selection file: %v", err)
	}
	// The keys name every container and service found on the remote host.
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("selection file mode = %#o, want 0600", perm)
	}

	// A second run starts with the choice already made.
	next := testConfig(t)
	next.hostMode = discovery.HostPortsSelect
	next.selectionPath = path
	restarted, _ := testEngine(t, next, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	restarted.poll(t.Context())
	waitForSnapshot(t, restarted, "bind the remembered host port", func(s state.Snapshot) bool {
		row, ok := rowNamed(s, "postgres")
		return ok && row.LocalPort != 0
	})
}

func TestEngineIgnoresAnUnreadableSelectionFile(t *testing.T) {
	// A corrupt file is not a reason to refuse to start: the worst case is that
	// the user picks their ports again.
	path := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := loadSelection(path, slog.New(slog.DiscardHandler)); len(got) != 0 {
		t.Errorf("loadSelection = %v, want an empty set", got)
	}
	if got := loadSelection(filepath.Join(t.TempDir(), "missing.json"), slog.New(slog.DiscardHandler)); len(got) != 0 {
		t.Errorf("loadSelection on a first run = %v, want an empty set", got)
	}
}

func mustForward(t *testing.T, specs ...string) []discovery.PortMap {
	t.Helper()
	maps, err := discovery.ParseForwards(specs)
	if err != nil {
		t.Fatalf("ParseForwards(%v): %v", specs, err)
	}
	return maps
}

func TestSelectionFileName(t *testing.T) {
	// The name has to stay recognisable — a user looking in their config
	// directory should see which host each file belongs to.
	for target, want := range map[string]string{
		"deploy@10.0.0.5:22":  "deploy@10.0.0.5_22.json",
		"myserver":            "myserver.json",
		"root@[fe80::1]:2222": "root@_fe80__1__2222.json",
		"":                    "default.json",
		"/../..":              "default.json",
	} {
		if got := selectionFileName(target); got != want {
			t.Errorf("selectionFileName(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestResolveSelectionPath(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	target := &sshconn.Target{User: "deploy", Host: "10.0.0.5", Port: 22}

	if got := resolveSelectionPath(SelectionOff, target, logger); got != "" {
		t.Errorf(`resolveSelectionPath("off") = %q, want nothing remembered`, got)
	}
	if got := resolveSelectionPath("/tmp/mine.json", target, logger); got != "/tmp/mine.json" {
		t.Errorf("an explicit -selection was rewritten to %q", got)
	}

	// The default is one file per target: the keys inside are not
	// host-qualified, so a shared file would apply one host's picks to another.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got := resolveSelectionPath("", target, logger)
	want := filepath.Join(dir, "auto-tunnel", selectionFileName(target.String()))
	if got != want {
		t.Errorf("default selection path = %q, want %q", got, want)
	}
	other := resolveSelectionPath("", &sshconn.Target{User: "deploy", Host: "10.0.0.9", Port: 22}, logger)
	if other == got {
		t.Error("two different hosts resolved to the same selection file")
	}
}

func TestEngineRemembersTheSelectionWithoutAFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsSelect
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "list the host port", func(s state.Snapshot) bool {
		_, ok := rowNamed(s, "postgres")
		return ok
	})

	// Nothing is written until the user actually picks something: a run that
	// never touches the keys must leave no file behind.
	if entries, err := os.ReadDir(filepath.Join(dir, "auto-tunnel")); err == nil && len(entries) > 0 {
		t.Errorf("selection files %v exist before any choice was made", entries)
	}

	pg, _ := rowNamed(snap, "postgres")
	eng.SetEnabled(pg.Key, true)

	st, err := os.Stat(eng.selectionPath)
	if err != nil {
		t.Fatalf("stat the default selection file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("selection file mode = %#o, want 0600", perm)
	}
	if dirSt, err := os.Stat(filepath.Dir(eng.selectionPath)); err != nil {
		t.Fatalf("stat the selection directory: %v", err)
	} else if perm := dirSt.Mode().Perm(); perm != 0o700 {
		t.Errorf("selection directory mode = %#o, want 0700", perm)
	}

	if got := loadSelection(eng.selectionPath, slog.New(slog.DiscardHandler)); !got[pg.Key] {
		t.Errorf("saved selection = %v, want the enabled key", got)
	}
}

func TestEngineSelectionOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg := testConfig(t)
	cfg.hostMode = discovery.HostPortsSelect
	cfg.selectionPath = SelectionOff
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ssListening))

	eng.poll(t.Context())
	snap := waitForSnapshot(t, eng, "list the host port", func(s state.Snapshot) bool {
		_, ok := rowNamed(s, "postgres")
		return ok
	})
	pg, _ := rowNamed(snap, "postgres")

	// The choice still takes effect, it is just not remembered.
	if !eng.SetEnabled(pg.Key, true) {
		t.Fatal("SetEnabled rejected a key under -selection off")
	}
	waitForSnapshot(t, eng, "bind the enabled port", func(s state.Snapshot) bool {
		row, ok := rowNamed(s, "postgres")
		return ok && row.LocalPort != 0
	})
	if eng.selectionPath != "" {
		t.Errorf("selectionPath = %q, want nothing remembered", eng.selectionPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "auto-tunnel")); !os.IsNotExist(err) {
		t.Errorf("a selection directory was created under -selection off: %v", err)
	}
}

func TestEngineRemembersTheDashboardPreferences(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ""))

	// A first run has nothing to remember, and reading that is not an error.
	if got := eng.Prefs(); got.Sort != "" {
		t.Errorf("Prefs on a first run = %+v, want the zero value", got)
	}

	eng.SavePrefs(ui.Prefs{Sort: "remote/url"})

	path := filepath.Join(dir, "auto-tunnel", "view.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the preferences file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("preferences mode = %#o, want 0600", perm)
	}
	if got := eng.Prefs(); got.Sort != "remote/url" {
		t.Errorf("Prefs = %+v, want what was saved", got)
	}

	// The preferences are not per-target: a second host reads the same file.
	other := testConfig(t)
	restarted, _ := testEngine(t, other, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ""))
	if got := restarted.Prefs(); got.Sort != "remote/url" {
		t.Errorf("a second engine read %+v, want the shared preference", got)
	}
}

func TestEngineSelectionOffAlsoStopsRememberingPreferences(t *testing.T) {
	// Somebody who asked for nothing to be remembered did not mean "except the
	// sort order".
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg := testConfig(t)
	cfg.selectionPath = SelectionOff
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ""))

	eng.SavePrefs(ui.Prefs{Sort: "traffic"})

	if eng.prefsPath != "" {
		t.Errorf("prefsPath = %q, want nothing remembered", eng.prefsPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "auto-tunnel")); !os.IsNotExist(err) {
		t.Errorf("a config directory was created under -selection off: %v", err)
	}
	if got := eng.Prefs(); got.Sort != "" {
		t.Errorf("Prefs = %+v, want the zero value", got)
	}
}

func TestEngineIgnoresUnreadablePreferences(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "auto-tunnel"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auto-tunnel", "view.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := testConfig(t)
	eng, _ := testEngine(t, cfg, remoteExec(dockerPS("web", "0.0.0.0:8080->80/tcp"), ""))

	if got := eng.Prefs(); got.Sort != "" {
		t.Errorf("Prefs = %+v, want the zero value for a corrupt file", got)
	}
}
