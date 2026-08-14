package routing

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// KubernetesConfig holds configuration for Kubernetes-based peer discovery.
type KubernetesConfig struct {
	// ServiceName is the headless Service used for Endpoints discovery.
	ServiceName string
	// Namespace to watch. Defaults to the pod's own namespace.
	Namespace string
	// RegistryPort is the port peers serve the registry on.
	RegistryPort int
	// ResyncInterval is how often to do a full Endpoints list as fallback.
	ResyncInterval time.Duration
}

// KubernetesDiscovery watches a headless Service's Endpoints to discover peers.
type KubernetesDiscovery struct {
	config KubernetesConfig
	client kubernetes.Interface
	selfIP string
}

var kubernetesNewForConfig = func(c *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(c)
}

// NewKubernetesDiscovery creates a KubernetesDiscovery using in-cluster config.
func NewKubernetesDiscovery(cfg KubernetesConfig) (*KubernetesDiscovery, error) {
	if cfg.Namespace == "" {
		ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return nil, fmt.Errorf("cannot detect namespace: %w", err)
		}
		cfg.Namespace = string(ns)
	}
	if cfg.RegistryPort == 0 {
		cfg.RegistryPort = 5000
	}
	if cfg.ResyncInterval == 0 {
		cfg.ResyncInterval = 30 * time.Second
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := kubernetesNewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	selfIP := detectSelfIP()

	return &KubernetesDiscovery{
		config: cfg,
		client: clientset,
		selfIP: selfIP,
	}, nil
}

// NewKubernetesDiscoveryWithClient creates a KubernetesDiscovery with a provided client (for testing).
func NewKubernetesDiscoveryWithClient(cfg KubernetesConfig, client kubernetes.Interface, selfIP string) *KubernetesDiscovery {
	if cfg.RegistryPort == 0 {
		cfg.RegistryPort = 5000
	}
	if cfg.ResyncInterval == 0 {
		cfg.ResyncInterval = 30 * time.Second
	}
	return &KubernetesDiscovery{
		config: cfg,
		client: client,
		selfIP: selfIP,
	}
}

// Discover watches the Endpoints for the configured Service and merges discovered
// peers into the PeerWatcher. It blocks until the context is cancelled.
func (kd *KubernetesDiscovery) Discover(ctx context.Context, pw *PeerWatcher) error {
	return kd.discover(ctx, pw, 5*time.Second)
}

func (kd *KubernetesDiscovery) discover(ctx context.Context, pw *PeerWatcher, retryDelay time.Duration) error {
	slog.Info("kubernetes peer discovery started",
		"service", kd.config.ServiceName,
		"namespace", kd.config.Namespace,
		"self_ip", kd.selfIP,
	)

	for {
		err := kd.watchEndpoints(ctx, pw)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("kubernetes watch error, will retry", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
			}
		}
	}
}

func (kd *KubernetesDiscovery) watchEndpoints(ctx context.Context, pw *PeerWatcher) error {
	// Initial list
	endpoints, err := kd.client.CoreV1().Endpoints(kd.config.Namespace).Get(ctx, kd.config.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get endpoints: %w", err)
	}
	peers := kd.extractPeers(endpoints)
	pw.MergeDynamic(peers)
	slog.Info("kubernetes peers loaded", "count", len(peers), "peers", peers)

	// Watch for changes
	watcher, err := kd.client.CoreV1().Endpoints(kd.config.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fmt.Sprintf("metadata.name=%s", kd.config.ServiceName),
		ResourceVersion: endpoints.ResourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watch endpoints: %w", err)
	}
	defer watcher.Stop()

	resyncTicker := time.NewTicker(kd.config.ResyncInterval)
	defer resyncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type == watch.Error {
				return fmt.Errorf("watch error event")
			}
			ep, ok := event.Object.(*corev1.Endpoints)
			if !ok {
				continue
			}
			peers := kd.extractPeers(ep)
			pw.MergeDynamic(peers)
			slog.Info("kubernetes peers updated", "event", event.Type, "count", len(peers))
		case <-resyncTicker.C:
			ep, err := kd.client.CoreV1().Endpoints(kd.config.Namespace).Get(ctx, kd.config.ServiceName, metav1.GetOptions{})
			if err != nil {
				slog.Warn("kubernetes resync failed", "error", err)
				continue
			}
			peers := kd.extractPeers(ep)
			pw.MergeDynamic(peers)
		}
	}
}

// extractPeers converts Endpoints addresses into peer strings, excluding self.
func (kd *KubernetesDiscovery) extractPeers(ep *corev1.Endpoints) []string {
	var peers []string
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.IP == kd.selfIP {
				continue
			}
			peers = append(peers, fmt.Sprintf("%s:%d", addr.IP, kd.config.RegistryPort))
		}
	}
	return peers
}

// interfaceAddrs is a package-level variable so tests can inject a stub.
var interfaceAddrs = net.InterfaceAddrs

// detectSelfIP returns the first non-loopback IPv4 address of this node.
func detectSelfIP() string {
	// Try POD_IP env first (set by Kubernetes downward API)
	if ip := os.Getenv("POD_IP"); ip != "" {
		return ip
	}
	// Try HOST_IP env (set by Kubernetes downward API for hostNetwork pods)
	if ip := os.Getenv("HOST_IP"); ip != "" {
		return ip
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String()
		}
	}
	return ""
}
