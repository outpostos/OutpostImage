package routing

import (
	"math/rand"
	"sync"
)

// Balancer distributes requests across a set of peers.
// Supports round-robin (default) and latency-aware weighted random selection.
type Balancer struct {
	mu      sync.Mutex
	peers   []string
	weights []float64 // inverse-latency weights (nil = round-robin)
	idx     int
}

// NewBalancer creates a round-robin Balancer with the given peers.
func NewBalancer(peers []string) *Balancer {
	return &Balancer{
		peers: peers,
	}
}

// NewLatencyBalancer creates a latency-aware Balancer.
// Peers with lower latency get higher weight (inverse latency).
// If latency data is missing for a peer, it gets the median weight.
func NewLatencyBalancer(peers []string, latencies map[string]float64) *Balancer {
	if len(peers) == 0 {
		return &Balancer{}
	}

	weights := make([]float64, len(peers))
	totalWeight := 0.0

	for i, p := range peers {
		rtt, ok := latencies[p]
		if !ok || rtt <= 0 {
			rtt = 0.1 // default 100ms for unknown peers
		}
		// Inverse latency as weight: lower latency → higher weight
		w := 1.0 / rtt
		weights[i] = w
		totalWeight += w
	}

	// Normalize weights
	if totalWeight > 0 {
		for i := range weights {
			weights[i] /= totalWeight
		}
	}

	return &Balancer{
		peers:   peers,
		weights: weights,
	}
}

// Next returns the next peer.
// Uses weighted random selection if latency data is available,
// otherwise falls back to round-robin.
func (b *Balancer) Next() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.peers) == 0 {
		return "", false
	}

	if b.weights != nil {
		return b.weightedNext(), true
	}

	// Round-robin fallback
	peer := b.peers[b.idx%len(b.peers)]
	b.idx++
	return peer, true
}

// weightedNext selects a peer using weighted random selection.
// Must be called with lock held.
func (b *Balancer) weightedNext() string {
	r := rand.Float64()
	cumulative := 0.0
	for i, w := range b.weights {
		cumulative += w
		if r <= cumulative {
			return b.peers[i]
		}
	}
	// Fallback to last peer (floating point edge case)
	return b.peers[len(b.peers)-1]
}

// Peers returns a copy of the peer list.
func (b *Balancer) Peers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.peers))
	copy(out, b.peers)
	return out
}

// Len returns the number of peers.
func (b *Balancer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.peers)
}
