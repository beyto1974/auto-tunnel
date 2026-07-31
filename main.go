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

	"github.com/mustiko/auto-tunnel/internal/discovery"
	"github.com/mustiko/auto-tunnel/internal/sshconn"
	"github.com/mustiko/auto-tunnel/internal/state"
	"github.com/mustiko/auto-tunnel/internal/tunnel"
)

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
	verbose            bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "auto-tunnel: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	logFile, logger, err := newLogger(cfg)
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.Run(ctx)
	}()

	fmt.Printf("auto-tunnel: connecting to %s\n", target)
	if _, err := conn.WaitReady(ctx); err != nil {
		stop()
		<-done
		return err
	}
	fmt.Printf("auto-tunnel: connected, polling docker every %s (Ctrl-C to stop)\n", cfg.interval)

	watchErr := watch(ctx, cfg, conn, logger)
	stop()
	<-done
	return watchErr
}

// watch polls the remote Docker daemon, keeps the tunnel set in sync with what
// it finds, and prints the table whenever it changes.
func watch(ctx context.Context, cfg *config, conn *sshconn.Conn, logger *slog.Logger) error {
	disc := discovery.New(discovery.Options{
		PSCommand:          cfg.psCommand,
		InspectCommand:     cfg.inspectCommand,
		Include:            cfg.include,
		Exclude:            cfg.exclude,
		IncludeUnpublished: cfg.includeUnpublished,
		Log:                logger,
	})
	alloc := tunnel.NewAllocator(cfg.bind, cfg.fallbackBase, tunnel.DefaultFallbackSize)
	manager := tunnel.NewManager(tunnel.SSHDialer(conn), alloc, logger)
	defer manager.Close()

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	var previous, lastErr string
	for {
		client := conn.Client()
		if client == nil {
			if lastErr != "ssh-down" {
				fmt.Println("auto-tunnel: ssh connection down, local ports stay bound while it reconnects")
				lastErr = "ssh-down"
			}
		} else {
			result, err := disc.Discover(ctx, client)
			switch {
			case err != nil && ctx.Err() == nil:
				if msg := err.Error(); msg != lastErr {
					fmt.Fprintf(os.Stderr, "auto-tunnel: %v\n", err)
					logger.Warn("discovery failed", "err", err)
					lastErr = msg
				}
			case err == nil:
				lastErr = ""
				for _, w := range result.Warnings {
					logger.Warn("discovery warning", "detail", w)
				}
				manager.Reconcile(ctx, result.Maps)
			}
		}

		if summary := render(cfg, manager.Tunnels()); summary != previous {
			fmt.Print(summary)
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

// render formats the tunnel table. Its output doubles as a change key: identical
// text means nothing worth reporting moved. Byte counters are deliberately left
// out so idle traffic does not redraw the table on every poll.
func render(cfg *config, tunnels []state.Tunnel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%d tunnel(s)\n", len(tunnels))
	for _, t := range tunnels {
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

func parseFlags() (*config, error) {
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
	fs.BoolVar(&cfg.verbose, "verbose", false, "log at debug level")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: auto-tunnel [flags] <host>\n\n")
		fmt.Fprintf(fs.Output(), "  <host> is an ssh_config alias or user@host[:port]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return nil, fmt.Errorf("expected exactly one host argument, got %d", fs.NArg())
	}
	cfg.target = fs.Arg(0)

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

// newLogger sends logs to a file by default: once the TUI owns the terminal,
// anything written to stdout corrupts the rendered frame.
func newLogger(cfg *config) (*os.File, *slog.Logger, error) {
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	if cfg.logPath == "-" || cfg.logPath == "" {
		return nil, slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	}
	f, err := os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", cfg.logPath, err)
	}
	return f, slog.New(slog.NewTextHandler(f, opts)), nil
}
