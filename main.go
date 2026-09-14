package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"
	"golang.org/x/sync/errgroup"

	"github.com/outpostos/outpostimage/pkg/admission"
	"github.com/outpostos/outpostimage/pkg/httpx"
	"github.com/outpostos/outpostimage/pkg/metrics"
	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/registry"
	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// version is set at build time via -ldflags "-X main.version=v1.0.0".
var version = "dev"

// newCertMgr is the package-level function used to create a CertManager.
// Tests can replace it to inject errors and cover the warning path in run().
var newCertMgr = proxy.NewCertManager

// runFn is the package-level function called by entrypoint to start the service.
// Tests can replace it to inject errors and cover the error return path.
var runFn = run

// argNewParser wraps arg.NewParser so tests can inject a failing factory.
var argNewParser = func(cfg arg.Config, dests ...interface{}) (*arg.Parser, error) {
	return arg.NewParser(cfg, dests...)
}

// RegistryCmd is the main subcommand that runs the OutpostImage service.
type RegistryCmd struct {
	RegistryAddr             string   `arg:"--registry-addr,env:REGISTRY_ADDR" default:":5000" help:"P2P Registry API listen address"`
	ProxyAddr                string   `arg:"--proxy-addr,env:PROXY_ADDR" default:":3128" help:"HTTP Proxy listen address"`
	MetricsAddr              string   `arg:"--metrics-addr,env:METRICS_ADDR" default:":9090" help:"Metrics listen address"`
	DockerRoot               string   `arg:"--docker-root,env:DOCKER_ROOT" default:"/var/lib/docker" help:"Docker storage root directory"`
	ContainerdRoot           string   `arg:"--containerd-root,env:CONTAINERD_ROOT" default:"/var/lib/containerd" help:"Containerd storage root directory"`
	StoreType                string   `arg:"--store-type,env:STORE_TYPE" default:"auto" help:"Store type: auto, docker, containerd"`
	Peers                    []string `arg:"--peers,env:PEERS,separate" help:"Static peer list (host:port)"`
	PeersFile                string   `arg:"--peers-file,env:PEERS_FILE" help:"Path to peers file for hot-reload"`
	MDNSEnabled              bool     `arg:"--mdns-enabled,env:MDNS_ENABLED" default:"true" help:"Enable mDNS peer discovery"`
	MDNSInterface            string   `arg:"--mdns-interface,env:MDNS_INTERFACE" help:"Network interface for mDNS"`
	MDNSDomain               string   `arg:"--mdns-domain,env:MDNS_DOMAIN" default:"local" help:"mDNS domain"`
	K8sDiscovery             bool     `arg:"--k8s-discovery,env:K8S_DISCOVERY" default:"false" help:"Enable Kubernetes Endpoints peer discovery"`
	K8sServiceName           string   `arg:"--k8s-service-name,env:K8S_SERVICE_NAME" default:"outpostimage" help:"Kubernetes Service name for peer discovery"`
	K8sNamespace             string   `arg:"--k8s-namespace,env:K8S_NAMESPACE" help:"Kubernetes namespace (auto-detected if empty)"`
	LogLevel                 string   `arg:"--log-level,env:LOG_LEVEL" default:"INFO" help:"Log level (DEBUG, INFO, WARN, ERROR)"`
	CADir                    string   `arg:"--ca-dir,env:CA_DIR" default:"/etc/outpostimage" help:"Directory for CA certificate and key"`
	InsecureRegistries       []string `arg:"--insecure-registry,env:INSECURE_REGISTRIES,separate" help:"Registries to connect without TLS verification (host:port or host)"`
	RegistryUpstreamFallback bool     `arg:"--registry-upstream-fallback,env:REGISTRY_UPSTREAM_FALLBACK" default:"true" help:"Allow registry endpoint to fall back to upstream after local and P2P miss"`
	AdmissionWhitelist       string   `arg:"--admission-whitelist,env:ADMISSION_WHITELIST" help:"Path to manifest digest whitelist JSON; empty disables admission. The table must list image index digests AND all per-platform child manifest digests for multi-arch images"`
	AdmissionPollInterval    string   `arg:"--admission-poll-interval,env:ADMISSION_POLL_INTERVAL" default:"10s" help:"Whitelist file poll interval for hot reload (Go duration)"`
}

// Args holds the top-level CLI arguments with subcommands.
type Args struct {
	Registry *RegistryCmd `arg:"subcommand:registry" help:"Run the OutpostImage registry service"`
}

func (Args) Description() string {
	return "OutpostImage - P2P OCI image distribution proxy for edge computing"
}

func (Args) Version() string {
	return "outpostimage " + version
}

func main() {
	os.Exit(entrypoint(os.Args[1:], os.Stderr))
}

// entrypoint is the testable core of main: parses args and calls run.
// Returns 0 on success, 1 on any error or missing subcommand.
func entrypoint(args []string, stderr io.Writer) int {
	var a Args
	p, err := argNewParser(arg.Config{Out: stderr}, &a)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := p.Parse(args); err != nil {
		return 1
	}

	if a.Registry == nil {
		p.WriteUsage(stderr)
		fmt.Fprintln(stderr, "\nerror: please specify a subcommand (e.g., 'registry')")
		return 1
	}

	if err := runFn(a.Registry); err != nil {
		slog.Error("service stopped with error", "error", err)
		return 1
	}
	return 0
}

func run(cmd *RegistryCmd) error {
	// Configure structured logging
	level := parseLogLevel(cmd.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	slog.Info("starting outpostimage",
		"registry_addr", cmd.RegistryAddr,
		"proxy_addr", cmd.ProxyAddr,
		"metrics_addr", cmd.MetricsAddr,
		"store_type", cmd.StoreType,
		"docker_root", cmd.DockerRoot,
		"containerd_root", cmd.ContainerdRoot,
		"peers", cmd.Peers,
		"peers_file", cmd.PeersFile,
		"registry_upstream_fallback", cmd.RegistryUpstreamFallback,
		"log_level", cmd.LogLevel,
	)

	// Create root context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	// Hash whitelist admission (T12): configured path must load or we fail
	// fast — admission is never silently skipped. Empty path disables it.
	var admChecker *admission.Checker
	if cmd.AdmissionWhitelist != "" {
		interval, err := time.ParseDuration(cmd.AdmissionPollInterval)
		if err != nil {
			return fmt.Errorf("invalid --admission-poll-interval %q: %w", cmd.AdmissionPollInterval, err)
		}
		if interval <= 0 {
			return fmt.Errorf("invalid --admission-poll-interval %q: must be positive", cmd.AdmissionPollInterval)
		}
		admChecker, err = admission.NewChecker(cmd.AdmissionWhitelist)
		if err != nil {
			return err
		}
		g.Go(func() error {
			return admChecker.Watch(ctx, interval)
		})
		slog.Info("hash whitelist admission enabled",
			"whitelist", cmd.AdmissionWhitelist,
			"version", admChecker.Version(),
			"poll_interval", interval.String(),
		)
	}

	// Initialize store based on --store-type
	store := initStore(ctx, cmd)

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

	// Initialize mDNS peer discovery
	if cmd.MDNSEnabled {
		// Extract port from registry addr for mDNS advertisement
		registryPort := 5000
		if _, portStr, ok := parseAddr(cmd.RegistryAddr); ok {
			_, _ = fmt.Sscanf(portStr, "%d", &registryPort)
		}
		mdnsSvc := routing.NewMDNSService(routing.MDNSConfig{
			Interface: cmd.MDNSInterface,
			Domain:    cmd.MDNSDomain,
			Port:      registryPort,
			Version:   version,
		})
		if err := mdnsSvc.Advertise(); err != nil {
			slog.Warn("mDNS advertise failed", "error", err)
		} else {
			g.Go(func() error {
				defer mdnsSvc.Shutdown()
				return mdnsSvc.Discover(ctx, pw)
			})
		}
	}

	// Initialize Kubernetes peer discovery
	if cmd.K8sDiscovery {
		registryPort := 5000
		if _, portStr, ok := parseAddr(cmd.RegistryAddr); ok {
			_, _ = fmt.Sscanf(portStr, "%d", &registryPort)
		}
		k8sDisc, err := routing.NewKubernetesDiscovery(routing.KubernetesConfig{
			ServiceName:  cmd.K8sServiceName,
			Namespace:    cmd.K8sNamespace,
			RegistryPort: registryPort,
		})
		if err != nil {
			slog.Warn("kubernetes discovery init failed", "error", err)
		} else {
			g.Go(func() error {
				return k8sDisc.Discover(ctx, pw)
			})
		}
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

	ociOpts := []registry.Option{
		registry.WithRouter(router),
		registry.WithUpstreamFallback(cmd.RegistryUpstreamFallback),
		registry.WithMetrics(metrics.Default),
	}
	if admChecker != nil {
		ociOpts = append(ociOpts, registry.WithAdmission(admChecker))
	}
	ociHandler := registry.NewOCIHandler(store, ociOpts...)
	ociHandler.RegisterRoutes(registryMux)

	digestHandler := registry.NewDigestHandler(tracker)
	digestHandler.RegisterRoutes(registryMux)

	registryHandler := metrics.NewMiddleware("registry")(registryMux)
	httpx.Serve(ctx, g, cmd.RegistryAddr, registryHandler, "registry")

	// Initialize CA for HTTPS MITM
	caDir := cmd.CADir
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		slog.Warn("failed to create CA directory", "dir", caDir, "error", err)
	}
	var certMgr *proxy.CertManager
	certMgr, err := newCertMgr(caDir)
	if err != nil {
		slog.Warn("cert manager init failed (HTTPS interception disabled)", "error", err)
	} else {
		slog.Info("CA certificate loaded", "path", filepath.Join(caDir, "ca.crt"))
	}

	// HTTP Proxy server - intercepts OCI registry traffic (HTTP + HTTPS MITM)
	interceptor := proxy.RequestRouter(tracker, cmd.RegistryAddr, router)
	if admChecker != nil {
		interceptor = proxy.AdmissionInterceptor(admChecker, interceptor)
	}
	p := proxy.NewProxy(interceptor, certMgr).WithInsecureRegistries(cmd.InsecureRegistries)
	proxyHandler := metrics.NewMiddleware("proxy")(p)
	httpx.Serve(ctx, g, cmd.ProxyAddr, proxyHandler, "proxy")

	// Metrics server (:9090) - Prometheus metrics + health check
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", httpx.HandleHealthz())
	metricsMux.Handle("GET /metrics", metrics.Handler())
	metricsHandler := metrics.NewMiddleware("metrics")(metricsMux)
	httpx.Serve(ctx, g, cmd.MetricsAddr, metricsHandler, "metrics")

	return g.Wait()
}

func initStore(ctx context.Context, cmd *RegistryCmd) oci.Store {
	switch cmd.StoreType {
	case "docker":
		ds := oci.NewDockerStore(cmd.DockerRoot)
		if err := ds.Init(ctx); err != nil {
			slog.Warn("docker store init failed (will retry via tracker)", "error", err)
		}
		return ds
	case "containerd":
		cs := oci.NewContainerdStore(cmd.ContainerdRoot)
		if err := cs.Init(ctx); err != nil {
			slog.Warn("containerd store init failed (will retry via tracker)", "error", err)
		}
		return cs
	default: // "auto"
		stores := oci.DetectStores(ctx, cmd.DockerRoot, cmd.ContainerdRoot)
		if len(stores) == 0 {
			slog.Warn("no container runtime stores detected, using empty docker store")
			ds := oci.NewDockerStore(cmd.DockerRoot)
			_ = ds.Init(ctx)
			return ds
		}
		if len(stores) == 1 {
			return stores[0]
		}
		slog.Info("multiple stores detected, using MultiStore", "count", len(stores))
		return oci.NewMultiStore(stores...)
	}
}

// parseAddr splits an address like ":5000" or "0.0.0.0:5000" into host and port.
func parseAddr(addr string) (string, string, bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return addr, "", false
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
