package routing

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
)

const (
	// MDNSServiceName is the mDNS service type for OutpostImage nodes.
	MDNSServiceName = "_outpostimage._tcp"

	// DefaultMDNSQueryInterval is how often to query for peers.
	DefaultMDNSQueryInterval = 10 * time.Second

	// DefaultMDNSGracePeriod is how long to keep a peer after it stops advertising.
	DefaultMDNSGracePeriod = 30 * time.Second
)

// MDNSConfig holds mDNS configuration.
type MDNSConfig struct {
	// Interface to bind to (empty = all interfaces).
	Interface string
	// Domain for mDNS (default "local").
	Domain string
	// Port this node is serving on.
	Port int
	// NodeID is the unique identifier for this node.
	NodeID string
	// Version is the software version.
	Version string
	// QueryInterval is how often to scan for peers.
	QueryInterval time.Duration
	// GracePeriod is how long to keep a disappeared peer.
	GracePeriod time.Duration
}

// MDNSService handles mDNS broadcasting and discovery.
type MDNSService struct {
	config MDNSConfig
	server *mdns.Server

	mu       sync.RWMutex
	peers    map[string]time.Time // addr → last seen
}

// NewMDNSService creates a new mDNS service.
func NewMDNSService(cfg MDNSConfig) *MDNSService {
	if cfg.Domain == "" {
		cfg.Domain = "local"
	}
	if cfg.QueryInterval == 0 {
		cfg.QueryInterval = DefaultMDNSQueryInterval
	}
	if cfg.GracePeriod == 0 {
		cfg.GracePeriod = DefaultMDNSGracePeriod
	}
	if cfg.NodeID == "" {
		hostname, _ := os.Hostname()
		cfg.NodeID = hostname
	}
	if cfg.Version == "" {
		cfg.Version = "v0.1.0"
	}
	return &MDNSService{
		config: cfg,
		peers:  make(map[string]time.Time),
	}
}

// Advertise starts broadcasting this node via mDNS.
func (s *MDNSService) Advertise() error {
	info := []string{
		fmt.Sprintf("node=%s", s.config.NodeID),
		fmt.Sprintf("version=%s", s.config.Version),
	}

	service, err := mdns.NewMDNSService(
		s.config.NodeID,
		MDNSServiceName,
		s.config.Domain+".",
		"",
		s.config.Port,
		nil, // IPs (nil = auto-detect)
		info,
	)
	if err != nil {
		return fmt.Errorf("create mDNS service: %w", err)
	}

	var iface *net.Interface
	if s.config.Interface != "" {
		iface, err = net.InterfaceByName(s.config.Interface)
		if err != nil {
			return fmt.Errorf("interface %q: %w", s.config.Interface, err)
		}
	}

	server, err := mdns.NewServer(&mdns.Config{
		Zone:  service,
		Iface: iface,
	})
	if err != nil {
		return fmt.Errorf("start mDNS server: %w", err)
	}

	s.server = server
	slog.Info("mDNS advertising started",
		"service", MDNSServiceName,
		"port", s.config.Port,
		"node_id", s.config.NodeID,
	)
	return nil
}

// Discover runs periodic mDNS queries and returns discovered peers.
// It blocks until the context is cancelled.
func (s *MDNSService) Discover(ctx context.Context, pw *PeerWatcher) error {
	ticker := time.NewTicker(s.config.QueryInterval)
	defer ticker.Stop()

	// Initial query
	s.queryAndMerge(pw)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.queryAndMerge(pw)
		}
	}
}

// queryAndMerge performs one mDNS query round and merges results with PeerWatcher.
func (s *MDNSService) queryAndMerge(pw *PeerWatcher) {
	entries := make(chan *mdns.ServiceEntry, 16)

	go func() {
		params := &mdns.QueryParam{
			Service:             MDNSServiceName,
			Domain:              s.config.Domain,
			Timeout:             3 * time.Second,
			Entries:             entries,
			DisableIPv6:         true,
			WantUnicastResponse: false,
		}
		if s.config.Interface != "" {
			if iface, err := net.InterfaceByName(s.config.Interface); err == nil {
				params.Interface = iface
			}
		}
		if err := mdns.Query(params); err != nil {
			slog.Warn("mDNS query failed", "error", err)
		}
	}()

	for entry := range entries {
		addr := fmt.Sprintf("%s:%d", entry.AddrV4.String(), entry.Port)

		s.mu.Lock()
		s.peers[addr] = time.Now()
		s.mu.Unlock()

		slog.Debug("mDNS peer discovered", "addr", addr, "host", entry.Host, "info", entry.InfoFields)
	}

	s.expireAndMerge(pw)
}

// expireAndMerge removes expired peers and merges with PeerWatcher.
func (s *MDNSService) expireAndMerge(pw *PeerWatcher) {
	now := time.Now()
	s.mu.Lock()
	for addr, lastSeen := range s.peers {
		if now.Sub(lastSeen) > s.config.GracePeriod {
			delete(s.peers, addr)
			slog.Info("mDNS peer expired", "addr", addr)
		}
	}
	s.mu.Unlock()

	s.MergePeers(pw)
}

// MergePeers merges mDNS-discovered peers with the PeerWatcher's static peers.
func (s *MDNSService) MergePeers(pw *PeerWatcher) {
	s.mu.RLock()
	mdnsPeers := make([]string, 0, len(s.peers))
	for addr := range s.peers {
		mdnsPeers = append(mdnsPeers, addr)
	}
	s.mu.RUnlock()

	pw.MergeDynamic(mdnsPeers)
}

// DiscoveredPeers returns the currently discovered mDNS peers.
func (s *MDNSService) DiscoveredPeers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make([]string, 0, len(s.peers))
	for addr := range s.peers {
		peers = append(peers, addr)
	}
	return peers
}

// Shutdown stops the mDNS server.
func (s *MDNSService) Shutdown() {
	if s.server != nil {
		_ = s.server.Shutdown()
		slog.Info("mDNS server stopped")
	}
}
