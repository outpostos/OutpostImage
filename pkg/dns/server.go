package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// DefaultHijackDomains is the default list of registry domains to hijack.
var DefaultHijackDomains = []string{
	"docker.io",
	"registry-1.docker.io",
	"ghcr.io",
	"registry.k8s.io",
	"quay.io",
}

// ServerConfig holds DNS server configuration.
type ServerConfig struct {
	// Addr is the listen address (default ":5353").
	Addr string
	// HijackDomains is the list of domains to hijack (resolve to local IP).
	HijackDomains []string
	// LocalIP is the IP address to return for hijacked domains.
	// If empty, auto-detected from the first non-loopback interface.
	LocalIP net.IP
	// UpstreamDNS is the upstream DNS server to forward non-hijacked queries.
	// Default: "8.8.8.8:53"
	UpstreamDNS string
	// TTL for hijacked DNS responses (default 30s).
	TTL uint32
}

// Server is an embedded DNS server that hijacks configured registry domains.
type Server struct {
	config    ServerConfig
	hijackSet map[string]struct{} // normalized domain → struct{}
	udpServer *mdns.Server
	tcpServer *mdns.Server
	client    *mdns.Client

	mu      sync.Mutex
	started bool
}

// NewServer creates a new DNS server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":5353"
	}
	if cfg.UpstreamDNS == "" {
		cfg.UpstreamDNS = "8.8.8.8:53"
	}
	if cfg.TTL == 0 {
		cfg.TTL = 30
	}
	if len(cfg.HijackDomains) == 0 {
		cfg.HijackDomains = DefaultHijackDomains
	}
	if cfg.LocalIP == nil {
		ip, err := detectLocalIP()
		if err != nil {
			return nil, fmt.Errorf("detect local IP: %w", err)
		}
		cfg.LocalIP = ip
	}

	// Build hijack set with normalized FQDN
	hijackSet := make(map[string]struct{}, len(cfg.HijackDomains))
	for _, d := range cfg.HijackDomains {
		fqdn := mdns.Fqdn(strings.ToLower(d))
		hijackSet[fqdn] = struct{}{}
	}

	return &Server{
		config:    cfg,
		hijackSet: hijackSet,
		client: &mdns.Client{
			Timeout: 5 * time.Second,
		},
	}, nil
}

// Run starts the DNS server and blocks until context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	handler := mdns.HandlerFunc(s.handleDNS)

	// Start UDP server
	udpServer := &mdns.Server{
		Addr:    s.config.Addr,
		Net:     "udp",
		Handler: handler,
	}

	// Start TCP server
	tcpServer := &mdns.Server{
		Addr:    s.config.Addr,
		Net:     "tcp",
		Handler: handler,
	}

	s.mu.Lock()
	s.udpServer = udpServer
	s.tcpServer = tcpServer
	s.started = true
	s.mu.Unlock()

	errCh := make(chan error, 2)

	go func() {
		slog.Info("DNS server listening (UDP)", "addr", s.config.Addr)
		if err := udpServer.ListenAndServe(); err != nil {
			errCh <- fmt.Errorf("UDP DNS server: %w", err)
		}
	}()

	go func() {
		slog.Info("DNS server listening (TCP)", "addr", s.config.Addr)
		if err := tcpServer.ListenAndServe(); err != nil {
			errCh <- fmt.Errorf("TCP DNS server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		s.Shutdown()
		return nil
	case err := <-errCh:
		s.Shutdown()
		return err
	}
}

// Shutdown stops the DNS server.
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udpServer != nil {
		s.udpServer.Shutdown()
	}
	if s.tcpServer != nil {
		s.tcpServer.Shutdown()
	}
	s.started = false
}

// IsHijacked reports whether the given domain is in the hijack list.
func (s *Server) IsHijacked(domain string) bool {
	fqdn := mdns.Fqdn(strings.ToLower(domain))
	_, ok := s.hijackSet[fqdn]
	return ok
}

// handleDNS processes incoming DNS queries.
func (s *Server) handleDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, q := range r.Question {
		switch q.Qtype {
		case mdns.TypeA:
			if s.IsHijacked(q.Name) {
				slog.Debug("DNS hijack A", "domain", q.Name, "ip", s.config.LocalIP)
				msg.Answer = append(msg.Answer, &mdns.A{
					Hdr: mdns.RR_Header{
						Name:   q.Name,
						Rrtype: mdns.TypeA,
						Class:  mdns.ClassINET,
						Ttl:    s.config.TTL,
					},
					A: s.config.LocalIP.To4(),
				})
				continue
			}
		case mdns.TypeAAAA:
			if s.IsHijacked(q.Name) {
				// Return empty AAAA to force IPv4 usage
				slog.Debug("DNS hijack AAAA (empty)", "domain", q.Name)
				continue
			}
		}

		// Forward to upstream for non-hijacked queries
		s.forwardQuery(q, msg)
	}

	if err := w.WriteMsg(msg); err != nil {
		slog.Error("DNS write response failed", "error", err)
	}
}

// forwardQuery forwards a single question to the upstream DNS server.
func (s *Server) forwardQuery(q mdns.Question, msg *mdns.Msg) {
	fwd := new(mdns.Msg)
	fwd.SetQuestion(q.Name, q.Qtype)
	fwd.RecursionDesired = true

	resp, _, err := s.client.Exchange(fwd, s.config.UpstreamDNS)
	if err != nil {
		slog.Warn("DNS upstream query failed", "domain", q.Name, "type", mdns.TypeToString[q.Qtype], "error", err)
		return
	}

	msg.Answer = append(msg.Answer, resp.Answer...)
	msg.Ns = append(msg.Ns, resp.Ns...)
	msg.Extra = append(msg.Extra, resp.Extra...)
}

// detectLocalIP finds the first non-loopback IPv4 address.
func detectLocalIP() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipNet.IP.IsLoopback() {
			continue
		}
		if ipNet.IP.To4() != nil {
			return ipNet.IP.To4(), nil
		}
	}
	return nil, fmt.Errorf("no non-loopback IPv4 address found")
}
