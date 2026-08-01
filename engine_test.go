package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/sshtest"
	"github.com/beyto1974/auto-tunnel/internal/state"
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
