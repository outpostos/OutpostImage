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

	"github.com/outpostos/outpostimage/pkg/routing"
)

// RegistryCmd is the main subcommand that runs the OutpostImage service.
type RegistryCmd struct {
	RegistryAddr string   `arg:"--registry-addr,env:REGISTRY_ADDR" default:":5000" help:"P2P Registry API listen address"`
	ProxyAddr    string   `arg:"--proxy-addr,env:PROXY_ADDR" default:":3128" help:"HTTP Proxy listen address"`
	MetricsAddr  string   `arg:"--metrics-addr,env:METRICS_ADDR" default:":9090" help:"Metrics listen address"`
	DockerRoot   string   `arg:"--docker-root,env:DOCKER_ROOT" default:"/var/lib/docker" help:"Docker storage root directory"`
	Peers        []string `arg:"--peers,env:PEERS,separate" help:"Static peer list (host:port)"`
	PeersFile    string   `arg:"--peers-file,env:PEERS_FILE" help:"Path to peers file for hot-reload"`
	LogLevel     string   `arg:"--log-level,env:LOG_LEVEL" default:"INFO" help:"Log level (DEBUG, INFO, WARN, ERROR)"`
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
		"docker_root", cmd.DockerRoot,
		"peers", cmd.Peers,
		"peers_file", cmd.PeersFile,
		"log_level", cmd.LogLevel,
	)

	// Create root context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	// Initialize PeerWatcher with CLI peers
	pw := routing.NewPeerWatcher(cmd.Peers)

	// If peers-file is specified, watch it for hot-reload
	if cmd.PeersFile != "" {
		g.Go(func() error {
			return pw.WatchFile(ctx, cmd.PeersFile)
		})
	}

	// TODO(E2): Initialize Store (containerd/docker adapter)
	// TODO(E5): Initialize Router (StaticRouter, use pw)
	// TODO(E2): g.Go: state.Track(ctx, store, router)
	// TODO(E3): g.Go: registry server (cmd.RegistryAddr)
	// TODO(E4): g.Go: proxy server (cmd.ProxyAddr)
	// TODO(E6): g.Go: metrics server (cmd.MetricsAddr)
	_ = pw // TODO: pass to Router when implemented

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
