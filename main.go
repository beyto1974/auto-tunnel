// Command auto-tunnel watches a remote host's Docker containers over SSH and
// forwards their published ports to localhost.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mustiko/auto-tunnel/internal/discovery"
	"github.com/mustiko/auto-tunnel/internal/logbuf"
	"github.com/mustiko/auto-tunnel/internal/sshconn"
	"github.com/mustiko/auto-tunnel/internal/state"
	"github.com/mustiko/auto-tunnel/internal/tunnel"
	"github.com/mustiko/auto-tunnel/internal/ui"
)

// refreshInterval is how often the dashboard is redrawn. It is independent of
// the discovery interval so counters stay lively between polls.
const refreshInterval = 500 * time.Millisecond

type config struct {
	target             string
	logPath            string
	dialTimeout        time.Duration
	interval           time.Duration
	bind               string
	fallbackBase       int
	psCommand          string
	inspectCommand     string
	include            *regexp.Regexp
	exclude            *regexp.Regexp
	includeUnpublished bool
	noTUI              bool
	verbose            bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "auto-tunnel: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	logs := logbuf.New(500)
	logFile, logger, err := newLogger(cfg, logs)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer logFile.Close()
	}

	target, err := sshconn.ResolveTarget(cfg.target)
	if err != nil {
		return err
	}
	logger.Info("resolved target",
		"spec", target.Spec, "user", target.User, "host", target.Host,
		"port", target.Port, "proxy_jump", target.ProxyJump)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn := sshconn.New(target, cfg.dialTimeout, logger)
	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		conn.Run(ctx)
	}()

	fmt.Printf("auto-tunnel: connecting to %s\n", target)
	if _, err := conn.WaitReady(ctx); err != nil {
		stop()
		<-connDone
		return err
	}

	eng := newEngine(cfg, conn, logger)
	engDone := make(chan struct{})
	engCtx, engStop := context.WithCancel(ctx)
	go func() {
		defer close(engDone)
		eng.run(engCtx)
	}()

	if cfg.noTUI {
		err = runPlain(ctx, cfg, eng)
	} else {
		err = runTUI(ctx, eng, logs)
	}

	// Tear down in order: stop polling and close tunnels, then the SSH link.
	engStop()
	<-engDone
	stop()
	<-connDone
	return err
}

// runTUI renders the live dashboard until the user quits or a signal arrives.
func runTUI(ctx context.Context, eng *engine, logs *logbuf.Buffer) error {
	program := tea.NewProgram(ui.New(eng, logs), tea.WithAltScreen(), tea.WithContext(ctx))

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			program.Send(ui.SnapshotMsg(eng.snapshot()))
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	_, err := program.Run()
	<-feedDone
	if err != nil && ctx.Err() != nil {
		// A signal tearing the program down is a normal exit, not a failure.
		return nil
	}
	return err
}

// runPlain prints the tunnel table whenever it changes. It exists for scripting
// and for debugging the layers underneath the TUI.
func runPlain(ctx context.Context, cfg *config, eng *engine) error {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	fmt.Printf("auto-tunnel: connected, polling docker every %s (Ctrl-C to stop)\n", cfg.interval)

	var previous string
	for {
		snap := eng.snapshot()
		if summary := render(cfg, snap); summary != previous {
			fmt.Printf("\n[%s] %s", time.Now().Format("15:04:05"), summary)
			previous = summary
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Println("auto-tunnel: shutting down")
			return nil
		}
	}
}

// render formats the tunnel table. Its output doubles as a change key, so it
// must contain nothing that changes on its own: no timestamp (the caller adds
// one) and no byte counters, or the table would reprint on every refresh.
func render(cfg *config, snap state.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ssh %s · %d container(s) · %d tunnel(s)\n",
		snap.SSH.State, snap.Containers, len(snap.Tunnels))
	if snap.DiscoveryError != "" {
		fmt.Fprintf(&b, "  discovery error: %s\n", snap.DiscoveryError)
	}
	for _, t := range snap.Tunnels {
		local := "-"
		if t.LocalPort != 0 {
			local = net.JoinHostPort(cfg.bind, strconv.Itoa(t.LocalPort))
		}
		note := ""
		switch {
		case t.State == state.TunnelUnsupported:
			note = fmt.Sprintf("  (%s is not forwardable over ssh)", strings.ToUpper(t.Proto))
		case t.State == state.TunnelError:
			note = "  (" + t.LastError + ")"
		case !t.Published:
			note = "  (unpublished, via container IP)"
		}
		fmt.Fprintf(&b, "  %-11s %-24s %-22s -> %s%s\n",
			t.State, truncate(t.Name, 24), local, t.RemoteTarget, note)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func parseFlags(args []string) (*config, error) {
	cfg := &config{}
	var includePattern, excludePattern string

	fs := flag.NewFlagSet("auto-tunnel", flag.ContinueOnError)
	fs.StringVar(&cfg.logPath, "log", "auto-tunnel.log", "log file path (\"-\" writes to stderr)")
	fs.DurationVar(&cfg.dialTimeout, "connect-timeout", sshconn.DefaultDialTimeout, "SSH connect timeout")
	fs.DurationVar(&cfg.interval, "interval", 5*time.Second, "how often to poll the remote docker daemon")
	fs.StringVar(&cfg.bind, "bind", "127.0.0.1", "local address to bind forwarded ports on")
	fs.IntVar(&cfg.fallbackBase, "fallback-base", tunnel.DefaultFallbackBase, "first port tried when the preferred local port is taken")
	fs.StringVar(&cfg.psCommand, "docker-cmd", discovery.DefaultPSCommand, "remote command listing containers as JSON")
	fs.StringVar(&cfg.inspectCommand, "docker-inspect-cmd", discovery.DefaultInspectCommand, "remote command prefix used to resolve container IPs")
	fs.StringVar(&includePattern, "include", "", "only forward containers whose name matches this regexp")
	fs.StringVar(&excludePattern, "exclude", "", "never forward containers whose name matches this regexp")
	fs.BoolVar(&cfg.includeUnpublished, "include-unpublished", false, "also forward EXPOSEd-but-unpublished ports via the container IP")
	fs.BoolVar(&cfg.noTUI, "no-tui", false, "print plain text instead of the live dashboard")
	fs.BoolVar(&cfg.verbose, "verbose", false, "log at debug level")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: auto-tunnel [flags] <host>\n\n")
		fmt.Fprintf(fs.Output(), "  <host> is an ssh_config alias or user@host[:port]\n\n")
		fs.PrintDefaults()
	}
	// flag stops parsing at the first non-flag argument, which would silently
	// ignore everything after the host. Parse repeatedly, peeling off one
	// positional at a time, so `auto-tunnel myhost -no-tui` works too.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	if len(positional) != 1 {
		fs.Usage()
		return nil, fmt.Errorf("expected exactly one host argument, got %d", len(positional))
	}
	cfg.target = positional[0]

	if cfg.interval <= 0 {
		return nil, fmt.Errorf("-interval must be positive, got %s", cfg.interval)
	}
	if includePattern != "" {
		re, err := regexp.Compile(includePattern)
		if err != nil {
			return nil, fmt.Errorf("bad -include regexp: %w", err)
		}
		cfg.include = re
	}
	if excludePattern != "" {
		re, err := regexp.Compile(excludePattern)
		if err != nil {
			return nil, fmt.Errorf("bad -exclude regexp: %w", err)
		}
		cfg.exclude = re
	}
	return cfg, nil
}

// newLogger sends logs to a file by default: the TUI owns the terminal, so
// anything written to stdout would corrupt the rendered frame. Records are also
// mirrored into an in-memory buffer for the dashboard's log pane.
func newLogger(cfg *config, logs *logbuf.Buffer) (*os.File, *slog.Logger, error) {
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	if cfg.logPath == "-" || cfg.logPath == "" {
		if !cfg.noTUI {
			return nil, nil, fmt.Errorf(`-log - writes to stderr, which the dashboard would overwrite; use -no-tui or a log file`)
		}
		return nil, slog.New(logbuf.NewHandler(slog.NewTextHandler(os.Stderr, opts), logs)), nil
	}
	f, err := os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", cfg.logPath, err)
	}
	return f, slog.New(logbuf.NewHandler(slog.NewTextHandler(f, opts), logs)), nil
}
