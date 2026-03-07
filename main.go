package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"
	"golang.org/x/sync/errgroup"

	"github.com/outpostos/outpostimage/pkg/httpx"
	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/registry"
	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
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
	CADir        string   `arg:"--ca-dir,env:CA_DIR" default:"/etc/outpostimage" help:"Directory for CA certificate and key"`
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

	// Initialize Docker store
	store := oci.NewDockerStore(cmd.DockerRoot)
	if err := store.Init(ctx); err != nil {
		slog.Warn("docker store init failed (will retry via tracker)", "error", err)
	}

	// Initialize State Tracker - maintains in-memory digest index
	tracker := state.NewTracker(store)
	g.Go(func() error {
		return tracker.Track(ctx)
	})

	// Initialize PeerWatcher with CLI peers
	pw := routing.NewPeerWatcher(cmd.Peers)
	if cmd.PeersFile != "" {
		g.Go(func() error {
			return pw.WatchFile(ctx, cmd.PeersFile)
		})
	}

	// Initialize P2P Router with health checking
	router := routing.NewStaticRouter(pw)
	g.Go(func() error {
		return router.RunHealthCheck(ctx, 30*time.Second)
	})

	// Registry server (:5000) - OCI Distribution API + digest query + health checks
	registryMux := http.NewServeMux()
	registryMux.HandleFunc("GET /healthz", httpx.HandleHealthz())
	registryMux.HandleFunc("GET /readyz", httpx.HandleReadyz())

	ociHandler := registry.NewOCIHandler(store)
	ociHandler.RegisterRoutes(registryMux)

	digestHandler := registry.NewDigestHandler(tracker)
	digestHandler.RegisterRoutes(registryMux)

	httpx.Serve(ctx, g, cmd.RegistryAddr, httpx.LoggingMiddleware(registryMux), "registry")

	// HTTP Proxy server (:3128) - intercepts OCI registry traffic
	interceptor := proxy.RequestRouter(tracker, cmd.RegistryAddr)
	p := proxy.NewProxy(interceptor)
	httpx.Serve(ctx, g, cmd.ProxyAddr, httpx.LoggingMiddleware(p), "proxy")

	// Initialize CA for HTTPS MITM (used when CONNECT tunneling is needed)
	caDir := cmd.CADir
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		slog.Warn("failed to create CA directory", "dir", caDir, "error", err)
	}
	certMgr, err := proxy.NewCertManager(caDir)
	if err != nil {
		slog.Warn("cert manager init failed (HTTPS interception disabled)", "error", err)
	} else {
		slog.Info("CA certificate loaded", "path", filepath.Join(caDir, "ca.crt"))
		_ = certMgr // TODO: wire into CONNECT handler for HTTPS MITM
	}

	// Metrics server (:9090) - Prometheus metrics + health check
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", httpx.HandleHealthz())
	// TODO(E6): Register /metrics handler (prometheus)
	httpx.Serve(ctx, g, cmd.MetricsAddr, httpx.LoggingMiddleware(metricsMux), "metrics")

	_ = router // Router is used by RunHealthCheck goroutine; future: pass to proxy for P2P lookup

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
