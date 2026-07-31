// Command auto-tunnel watches a remote host's Docker containers over SSH and
// forwards their published ports to localhost.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mustiko/auto-tunnel/internal/sshconn"
)

type config struct {
	target      string
	logPath     string
	dialTimeout time.Duration
	verbose     bool
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

	// Milestone 1 behaviour: prove the connection works, then exit. Later
	// milestones replace this with the discovery/tunnel/TUI loop.
	client, err := conn.WaitReady(ctx)
	if err != nil {
		stop()
		<-done
		return err
	}
	out, err := sshconn.RunCommand(ctx, client, "uname -a")
	if err != nil {
		stop()
		<-done
		return err
	}
	fmt.Printf("connected to %s (rtt pending, %s)\n", target, target.Addr())
	fmt.Printf("remote: %s\n", strings.TrimSpace(string(out)))

	stop()
	<-done
	return nil
}

func parseFlags() (*config, error) {
	cfg := &config{}
	fs := flag.NewFlagSet("auto-tunnel", flag.ContinueOnError)
	fs.StringVar(&cfg.logPath, "log", "auto-tunnel.log", "log file path (\"-\" writes to stderr)")
	fs.DurationVar(&cfg.dialTimeout, "connect-timeout", sshconn.DefaultDialTimeout, "SSH connect timeout")
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
	var w io.Writer = f
	return f, slog.New(slog.NewTextHandler(w, opts)), nil
}
