package main

import (
	"errors"
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
