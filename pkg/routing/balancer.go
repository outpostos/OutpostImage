package routing

import (
	"sync"
)

// Balancer distributes requests across a set of peers using round-robin.
type Balancer struct {
	mu    sync.Mutex
	peers []string
	idx   int
}

// NewBalancer creates a new Balancer with the given peers.
func NewBalancer(peers []string) *Balancer {
	return &Balancer{
		peers: peers,
	}
}

// Next returns the next peer in round-robin order.
// Returns empty string and false if no peers are available.
func (b *Balancer) Next() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.peers) == 0 {
		return "", false
	}
	peer := b.peers[b.idx%len(b.peers)]
	b.idx++
	return peer, true
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
