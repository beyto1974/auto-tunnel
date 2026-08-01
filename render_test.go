package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beyto1974/auto-tunnel/internal/logbuf"
	"github.com/beyto1974/auto-tunnel/internal/sshconn"
	"github.com/beyto1974/auto-tunnel/internal/state"
)

func TestRenderEmptySnapshot(t *testing.T) {
	got := render(&config{bind: "127.0.0.1"}, state.Snapshot{SSH: sshconn.Status{State: sshconn.StateConnecting}})

	if want := "ssh connecting · 0 container(s) · 0 tunnel(s)\n"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

func TestRenderSurfacesDiscoveryErrorsAndPerRowNotes(t *testing.T) {
	cfg := &config{bind: "127.0.0.1"}
	snap := state.Snapshot{
		SSH:            sshconn.Status{State: sshconn.StateConnected},
		Containers:     4,
		DiscoveryError: "docker discovery: permission denied",
		Tunnels: []state.Tunnel{
			{Name: "web", State: state.TunnelListening, LocalPort: 8080, RemoteTarget: "127.0.0.1:80", Published: true},
			{Name: "syslog", State: state.TunnelUnsupported, Proto: "udp", RemoteTarget: "127.0.0.1:514", Published: true},
			{Name: "db", State: state.TunnelError, LastError: "no free local port", RemoteTarget: "127.0.0.1:5432", Published: true},
			{Name: "cache", State: state.TunnelListening, LocalPort: 6379, RemoteTarget: "172.17.0.3:6379"},
		},
	}

	got := render(cfg, snap)

	for _, want := range []string{
		"ssh connected · 4 container(s) · 4 tunnel(s)",
		"discovery error: docker discovery: permission denied",
		"127.0.0.1:8080",
		"(UDP is not forwardable over ssh)",
		"(no free local port)",
		"(unpublished, via container IP)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render output is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderShowsADashWhenNoLocalPortIsBound(t *testing.T) {
	// An ERROR row never got a listener, so there is no host:port to print and
	// the column would otherwise read "127.0.0.1:0".
	got := render(&config{bind: "127.0.0.1"}, state.Snapshot{
		Tunnels: []state.Tunnel{{Name: "db", State: state.TunnelError, RemoteTarget: "127.0.0.1:5432"}},
	})

	if strings.Contains(got, ":0") {
		t.Errorf("render printed a zero port:\n%s", got)
	}
	// The local column is a padded "-", so the arrow follows a run of spaces.
	if !strings.Contains(got, "- ") || !strings.Contains(got, "-> 127.0.0.1:5432") {
		t.Errorf("render output is missing the placeholder for an unbound port:\n%s", got)
	}
}

func TestRenderIgnoresByteCounters(t *testing.T) {
	// runPlain uses render's output as the change key, so a row whose only
	// difference is traffic must render identically or the table reprints on
	// every refresh tick.
	cfg := &config{bind: "127.0.0.1"}
	base := state.Tunnel{Name: "web", State: state.TunnelActive, LocalPort: 8080, RemoteTarget: "127.0.0.1:80", Published: true}

	quiet := render(cfg, state.Snapshot{Tunnels: []state.Tunnel{base}})

	busy := base
	busy.BytesIn, busy.BytesOut, busy.ActiveConns, busy.TotalConns = 4096, 8192, 3, 17
	loud := render(cfg, state.Snapshot{Tunnels: []state.Tunnel{busy}})

	if quiet != loud {
		t.Errorf("render changed with traffic alone:\n%s\nvs\n%s", quiet, loud)
	}
}

func TestRenderTruncatesLongContainerNames(t *testing.T) {
	name := strings.Repeat("x", 40)
	got := render(&config{bind: "127.0.0.1"}, state.Snapshot{
		Tunnels: []state.Tunnel{{Name: name, State: state.TunnelListening, LocalPort: 8080, RemoteTarget: "127.0.0.1:80", Published: true}},
	})

	if strings.Contains(got, name) {
		t.Errorf("render printed the full 40-character name:\n%s", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("render output is missing the truncation marker:\n%s", got)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{name: "shorter than the limit is untouched", s: "web", n: 10, want: "web"},
		{name: "exactly the limit is untouched", s: "web", n: 3, want: "web"},
		{name: "longer gains an ellipsis", s: "webserver", n: 5, want: "webs…"},
		{name: "a one-character limit has no room for the marker", s: "webserver", n: 1, want: "w"},
		{name: "a zero limit yields nothing", s: "webserver", n: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate(tt.s, tt.n); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

func TestNewLoggerRefusesStderrUnderTheDashboard(t *testing.T) {
	// The TUI owns the terminal: logging to stderr behind it would shred the
	// rendered frame, so this has to be a startup error rather than a surprise.
	for _, path := range []string{"-", ""} {
		f, logger, err := newLogger(&config{logPath: path}, logbuf.New(10))
		if err == nil {
			t.Errorf("newLogger(logPath=%q) succeeded, want an error", path)
			continue
		}
		if f != nil || logger != nil {
			t.Errorf("newLogger(logPath=%q) returned a file or logger alongside its error", path)
		}
	}
}

func TestNewLoggerWritesToStderrWithoutTheDashboard(t *testing.T) {
	f, logger, err := newLogger(&config{logPath: "-", noTUI: true}, logbuf.New(10))
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	if f != nil {
		t.Errorf("newLogger returned a file for stderr logging, want nil")
	}
	if logger == nil {
		t.Fatal("newLogger returned no logger")
	}
}

func TestNewLoggerCreatesAPrivateLogFile(t *testing.T) {
	// The log records the remote host, the login user and every container name
	// found there, so a world-readable file on a shared box is a real leak.
	path := filepath.Join(t.TempDir(), "auto-tunnel.log")
	logs := logbuf.New(10)

	f, logger, err := newLogger(&config{logPath: path, verbose: true}, logs)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	defer f.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %#o, want 0600", perm)
	}

	// -verbose has to reach the handler, or debug records are silently dropped.
	logger.Debug("debug record")
	if !logger.Enabled(t.Context(), slog.LevelDebug) {
		t.Error("logger is not debug-enabled under -verbose")
	}
	if got := logs.Lines(); len(got) != 1 {
		t.Errorf("in-memory buffer holds %v, want the debug record", got)
	}
}

func TestNewLoggerReportsAnUnopenableLogPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "auto-tunnel.log")

	if _, _, err := newLogger(&config{logPath: path}, logbuf.New(10)); err == nil {
		t.Fatalf("newLogger(%q) succeeded, want an error", path)
	}
}

func TestResolveVersion(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = "v9.9.9"
	if got := resolveVersion(); got != "v9.9.9" {
		t.Errorf("resolveVersion() = %q, want the ldflag value", got)
	}

	// Without the ldflag the build info is consulted; under `go test` that is
	// either empty or "(devel)", so the fallback is what must come back.
	version = "dev"
	if got := resolveVersion(); got != "dev" {
		t.Errorf("resolveVersion() = %q, want the \"dev\" fallback under go test", got)
	}
}
