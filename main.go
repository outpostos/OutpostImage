package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexflint/go-arg"
	"golang.org/x/sync/errgroup"
)

// RegistryCmd is the main subcommand that runs the OutpostImage service.
type RegistryCmd struct {
	RegistryAddr string `arg:"--registry-addr,env:REGISTRY_ADDR" default:":5000" help:"P2P Registry API listen address"`
	ProxyAddr    string `arg:"--proxy-addr,env:PROXY_ADDR" default:":3128" help:"HTTP Proxy listen address"`
	MetricsAddr  string `arg:"--metrics-addr,env:METRICS_ADDR" default:":9090" help:"Metrics listen address"`
	LogLevel     string `arg:"--log-level,env:LOG_LEVEL" default:"INFO" help:"Log level (DEBUG, INFO, WARN, ERROR)"`
}

// Args holds the top-level CLI arguments with subcommands.
type Args struct {
	Registry *RegistryCmd `arg:"subcommand:registry" help:"Run the OutpostImage registry service"`
}

func (Args) Description() string {
	return "OutpostImage - P2P OCI image distribution proxy for edge computing"
}

func (Args) Version() string {
	return "outpostimage v0.1.0"
}

func main() {
	var args Args
	p := arg.MustParse(&args)

	if args.Registry == nil {
		p.WriteUsage(os.Stderr)
		fmt.Fprintln(os.Stderr, "\nerror: please specify a subcommand (e.g., 'registry')")
		os.Exit(1)
	}

	if err := run(args.Registry); err != nil {
		slog.Error("service stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(cmd *RegistryCmd) error {
	// Configure structured logging
	level := parseLogLevel(cmd.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	slog.Info("starting outpostimage",
		"registry_addr", cmd.RegistryAddr,
		"proxy_addr", cmd.ProxyAddr,
		"metrics_addr", cmd.MetricsAddr,
		"log_level", cmd.LogLevel,
	)

	// Create root context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	// TODO(E2): Initialize Store (containerd/docker adapter)
	// TODO(E5): Initialize Router (StaticRouter, load peers)
	// TODO(E2): g.Go: state.Track(ctx, store, router)
	// TODO(E3): g.Go: registry server (cmd.RegistryAddr)
	// TODO(E4): g.Go: proxy server (cmd.ProxyAddr)
	// TODO(E6): g.Go: metrics server (cmd.MetricsAddr)

	// Placeholder: wait for context cancellation
	g.Go(func() error {
		<-ctx.Done()
		slog.Info("shutting down")
		return nil
	})

	return g.Wait()
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
