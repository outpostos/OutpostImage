package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
)

// Router defines the P2P routing interface for discovering which peers have specific content.
type Router interface {
	// Ready reports whether the router is initialized and ready to handle requests.
	Ready(ctx context.Context) (bool, error)

	// Lookup finds peers that have the given digest and returns a Balancer to iterate them.
	Lookup(ctx context.Context, dgst digest.Digest) (*Balancer, error)

	// Advertise announces that this node has the given digests.
	Advertise(ctx context.Context, digests []digest.Digest) error

	// Withdraw announces that this node no longer has the given digests.
	Withdraw(ctx context.Context, digests []digest.Digest) error
}

// StaticRouter is a simple Router that uses a static peer list from PeerWatcher.
// It queries peers via their HTTP API to discover content.
type StaticRouter struct {
	pw     *PeerWatcher
	client *http.Client

	mu       sync.RWMutex
	health   map[string]bool      // peer → healthy
	backoff  map[string]time.Time // peer → backoff until
	failures map[string]int      // peer → consecutive failure count
	latency  map[string]float64  // peer → EWMA RTT in seconds
}

// EWMA smoothing factor: higher = more responsive to recent measurements.
const ewmaAlpha = 0.3

// NewStaticRouter creates a new StaticRouter backed by the given PeerWatcher.
func NewStaticRouter(pw *PeerWatcher) *StaticRouter {
	return &StaticRouter{
		pw: pw,
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
		health:   make(map[string]bool),
		backoff:  make(map[string]time.Time),
		failures: make(map[string]int),
		latency:  make(map[string]float64),
	}
}

// Ready reports whether the router has at least one healthy peer.
func (r *StaticRouter) Ready(_ context.Context) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, healthy := range r.health {
		if healthy {
			return true, nil
		}
	}
	// If no health checks have run yet, check if we have peers
	return len(r.pw.Peers()) > 0, nil
}

// Lookup queries all healthy peers for the given digest and returns a Balancer
// with the peers that have it. The balancer is latency-aware if RTT data is available.
func (r *StaticRouter) Lookup(ctx context.Context, dgst digest.Digest) (*Balancer, error) {
	peers := r.HealthyPeers()
	if len(peers) == 0 {
		return NewBalancer(nil), nil
	}

	type result struct {
		peer string
		has  bool
	}

	results := make(chan result, len(peers))
	for _, peer := range peers {
		go func(p string) {
			has := r.queryPeerDigest(ctx, p, dgst)
			results <- result{peer: p, has: has}
		}(peer)
	}

	var holders []string
	for i := 0; i < len(peers); i++ {
		select {
		case res := <-results:
			if res.has {
				holders = append(holders, res.peer)
			}
		case <-ctx.Done():
			return r.buildBalancer(holders), nil
		}
	}

	return r.buildBalancer(holders), nil
}

// buildBalancer creates a latency-aware balancer from the given peers.
func (r *StaticRouter) buildBalancer(peers []string) *Balancer {
	r.mu.RLock()
	latencies := make(map[string]float64, len(peers))
	hasLatency := false
	for _, p := range peers {
		if rtt, ok := r.latency[p]; ok {
			latencies[p] = rtt
			hasLatency = true
		}
	}
	r.mu.RUnlock()

	if hasLatency {
		return NewLatencyBalancer(peers, latencies)
	}
	return NewBalancer(peers)
}

// PeerLatency returns the EWMA RTT for a peer, or 0 if not available.
func (r *StaticRouter) PeerLatency(peer string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.latency[peer]
}

// Advertise is a no-op for StaticRouter (peers discover content via query).
func (r *StaticRouter) Advertise(_ context.Context, _ []digest.Digest) error {
	return nil
}

// Withdraw is a no-op for StaticRouter.
func (r *StaticRouter) Withdraw(_ context.Context, _ []digest.Digest) error {
	return nil
}

// RunHealthCheck periodically checks all peers' health.
// It blocks until the context is cancelled.
func (r *StaticRouter) RunHealthCheck(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial check
	r.checkAllPeers(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.checkAllPeers(ctx)
		}
	}
}

func (r *StaticRouter) checkAllPeers(ctx context.Context) {
	peers := r.pw.Peers()
	for _, peer := range peers {
		healthy, rtt := r.checkPeerHealth(ctx, peer)
		r.mu.Lock()
		if healthy {
			r.health[peer] = true
			r.failures[peer] = 0
			delete(r.backoff, peer)
			// Update EWMA latency
			if prev, ok := r.latency[peer]; ok {
				r.latency[peer] = ewmaAlpha*rtt + (1-ewmaAlpha)*prev
			} else {
				r.latency[peer] = rtt
			}
		} else {
			r.failures[peer]++
			if r.failures[peer] >= 3 {
				r.health[peer] = false
				r.backoff[peer] = time.Now().Add(r.backoffDuration(r.failures[peer]))
				slog.Warn("peer marked unhealthy", "peer", peer, "failures", r.failures[peer])
			}
		}
		r.mu.Unlock()
	}
}

// checkPeerHealth checks a peer's health and returns (healthy, rtt_seconds).
func (r *StaticRouter) checkPeerHealth(ctx context.Context, peer string) (bool, float64) {
	url := fmt.Sprintf("http://%s/healthz", peer)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0
	}
	start := time.Now()
	resp, err := r.client.Do(req)
	rtt := time.Since(start).Seconds()
	if err != nil {
		return false, 0
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, rtt
}

func (r *StaticRouter) queryPeerDigest(ctx context.Context, peer string, dgst digest.Digest) bool {
	url := fmt.Sprintf("http://%s/api/v1/digests/%s", peer, dgst.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var body struct {
		Exists bool `json:"exists"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	return body.Exists
}

// HealthyPeers returns all healthy peers, respecting backoff.
func (r *StaticRouter) HealthyPeers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := r.pw.Peers()
	var healthy []string
	now := time.Now()
	for _, p := range peers {
		// If in backoff, skip
		if until, ok := r.backoff[p]; ok && now.Before(until) {
			continue
		}
		// If explicitly marked unhealthy and still in backoff, skip
		if h, ok := r.health[p]; ok && !h {
			if until, ok := r.backoff[p]; ok && now.Before(until) {
				continue
			}
		}
		healthy = append(healthy, p)
	}
	return healthy
}

// backoffDuration calculates exponential backoff: 1min, 2min, 5min, 10min max.
func (r *StaticRouter) backoffDuration(failures int) time.Duration {
	switch {
	case failures <= 3:
		return 1 * time.Minute
	case failures <= 5:
		return 2 * time.Minute
	case failures <= 8:
		return 5 * time.Minute
	default:
		return 10 * time.Minute
	}
}
