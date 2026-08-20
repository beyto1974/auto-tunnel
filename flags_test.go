package main

import (
	"errors"
	"github.com/beyto1974/auto-tunnel/internal/discovery"
	"testing"
	"time"
)

func TestIsLoopback(t *testing.T) {
	// Anything that answers false gets a startup warning, because binding there
	// republishes the remote's services to the network with no authentication.
	tests := map[string]bool{
		"":              true,
		"127.0.0.1":     true,
		"127.0.0.53":    true,
		"::1":           true,
		"localhost":     true,
		"0.0.0.0":       false,
		"::":            false,
		"192.168.1.10":  false,
		"my-laptop.lan": false,
	}
	for bind, want := range tests {
		if got := isLoopback(bind); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", bind, got, want)
		}
	}
}

func TestParseFlagsAcceptsFlagsAfterTheHost(t *testing.T) {
	// The natural way to type it: host first, flags after. Go's flag package
	// stops at the first positional, so this has to be handled explicitly.
	cfg, err := parseFlags([]string{"deploy@example-host:2222", "-no-tui", "-interval", "2s", "-verbose"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.target != "deploy@example-host:2222" {
		t.Errorf("target = %q", cfg.target)
	}
	if !cfg.noTUI {
		t.Error("-no-tui after the host was ignored")
	}
	if !cfg.verbose {
		t.Error("-verbose after the host was ignored")
	}
	if cfg.interval != 2*time.Second {
		t.Errorf("interval = %s, want 2s", cfg.interval)
	}
}

func TestParseFlagsAcceptsFlagsBeforeTheHost(t *testing.T) {
	cfg, err := parseFlags([]string{"-bind", "0.0.0.0", "-interval", "1s", "myserver"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.target != "myserver" {
		t.Errorf("target = %q", cfg.target)
	}
	if cfg.bind != "0.0.0.0" {
		t.Errorf("bind = %q", cfg.bind)
	}
}

func TestParseFlagsInterleaved(t *testing.T) {
	// A flag whose value looks like a positional ("-log -") must not be
	// mistaken for the host.
	cfg, err := parseFlags([]string{"-no-tui", "myserver", "-log", "-"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.target != "myserver" {
		t.Errorf("target = %q, want myserver", cfg.target)
	}
	if cfg.logPath != "-" {
		t.Errorf("logPath = %q, want -", cfg.logPath)
	}
}

func TestParseFlagsRejectsWrongArgumentCount(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-no-tui"},
		{"hostA", "hostB"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v) succeeded, want an error", args)
		}
	}
}

func TestParseFlagsVersionNeedsNoHost(t *testing.T) {
	// -version has to short-circuit before the host argument is enforced,
	// otherwise `auto-tunnel -version` fails asking for a host.
	if _, err := parseFlags([]string{"-version"}); !errors.Is(err, errVersionRequested) {
		t.Errorf("parseFlags(-version) = %v, want errVersionRequested", err)
	}
	if v := resolveVersion(); v == "" {
		t.Error("resolveVersion returned an empty string")
	}
}

func TestParseFlagsValidatesPatternsAndInterval(t *testing.T) {
	if _, err := parseFlags([]string{"myserver", "-include", "("}); err == nil {
		t.Error("bad -include regexp was accepted")
	}
	if _, err := parseFlags([]string{"myserver", "-interval", "0s"}); err == nil {
		t.Error("zero -interval was accepted")
	}
}

func TestParseFlagsCollectsRepeatedForwards(t *testing.T) {
	cfg, err := parseFlags([]string{"myserver", "-forward", "5432", "-forward", "api=8000-8002@10.0.0.5"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if len(cfg.static) != 4 { // one port plus a three-port range
		t.Fatalf("static = %+v, want four declared forwards", cfg.static)
	}
	if got := cfg.static[0].Target(); got != "127.0.0.1:5432" {
		t.Errorf("first forward targets %q, want the remote loopback", got)
	}
	if got := cfg.static[3].Target(); got != "10.0.0.5:8002" {
		t.Errorf("last forward targets %q, want the declared host and port", got)
	}
}

func TestParseFlagsRejectsABadForward(t *testing.T) {
	// Silently dropping the row would leave the user hunting for a tunnel that
	// was never going to appear.
	if _, err := parseFlags([]string{"myserver", "-forward", "5432", "-forward", "http"}); err == nil {
		t.Error("a bad -forward spec was accepted")
	}
}

func TestParseFlagsHostPortModes(t *testing.T) {
	cfg, err := parseFlags([]string{"myserver", "-host-ports", "select"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.hostMode != discovery.HostPortsSelect {
		t.Errorf("hostMode = %q, want select", cfg.hostMode)
	}

	// Listed but not forwarded by default: the candidates are visible without a
	// flag, and nothing is bound until the user picks it.
	def, err := parseFlags([]string{"myserver"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if def.hostMode != discovery.HostPortsSelect {
		t.Errorf("default hostMode = %q, want select", def.hostMode)
	}
	if !def.hostMode.Offered() {
		t.Error("the default mode forwards host sockets on sight")
	}

	if _, err := parseFlags([]string{"myserver", "-host-ports", "yes"}); err == nil {
		t.Error("an unknown -host-ports mode was accepted")
	}
	if _, err := parseFlags([]string{"myserver", "-host-exclude", "("}); err == nil {
		t.Error("a bad -host-exclude regexp was accepted")
	}
}

func TestParseFlagsRejectsNoDockerWithNothingElse(t *testing.T) {
	// Host ports are on by default, so -no-docker alone still has something to
	// show; only switching that off as well leaves an empty dashboard forever.
	if _, err := parseFlags([]string{"myserver", "-no-docker"}); err != nil {
		t.Errorf("-no-docker on its own: %v", err)
	}
	if _, err := parseFlags([]string{"myserver", "-no-docker", "-host-ports", "off"}); err == nil {
		t.Error("-no-docker with no other source was accepted; the dashboard would always be empty")
	}
	if _, err := parseFlags([]string{"myserver", "-no-docker", "-forward", "5432"}); err != nil {
		t.Errorf("-no-docker with a declared forward: %v", err)
	}
	if _, err := parseFlags([]string{"myserver", "-no-docker", "-host-ports", "all"}); err != nil {
		t.Errorf("-no-docker with host ports: %v", err)
	}
}
