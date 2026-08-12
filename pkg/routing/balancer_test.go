package routing

import (
	"testing"
)

func TestBalancer_Empty(t *testing.T) {
	b := NewBalancer(nil)
	peer, ok := b.Next()
	if ok {
		t.Errorf("expected false, got true with peer %q", peer)
	}
	if b.Len() != 0 {
		t.Errorf("expected len 0, got %d", b.Len())
	}
}

func TestBalancer_RoundRobin(t *testing.T) {
	peers := []string{"a:5000", "b:5000", "c:5000"}
	b := NewBalancer(peers)

	expected := []string{"a:5000", "b:5000", "c:5000", "a:5000", "b:5000"}
	for i, want := range expected {
		got, ok := b.Next()
		if !ok {
			t.Fatalf("iteration %d: expected ok=true", i)
		}
		if got != want {
			t.Errorf("iteration %d: expected %q, got %q", i, want, got)
		}
	}
}

func TestBalancer_SinglePeer(t *testing.T) {
	b := NewBalancer([]string{"only:5000"})
	for i := 0; i < 5; i++ {
		peer, ok := b.Next()
		if !ok || peer != "only:5000" {
			t.Errorf("iteration %d: expected only:5000, got %q (ok=%v)", i, peer, ok)
		}
	}
}

func TestBalancer_Peers(t *testing.T) {
	b := NewBalancer([]string{"a:5000", "b:5000"})
	peers := b.Peers()
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	// Modify returned slice should not affect balancer
	peers[0] = "modified"
	original := b.Peers()
	if original[0] != "a:5000" {
		t.Error("modifying returned slice affected balancer")
	}
}

func TestBalancer_Len(t *testing.T) {
	tests := []struct {
		peers []string
		want  int
	}{
		{nil, 0},
		{[]string{}, 0},
		{[]string{"a:5000"}, 1},
		{[]string{"a:5000", "b:5000", "c:5000"}, 3},
	}
	for _, tt := range tests {
		b := NewBalancer(tt.peers)
		if got := b.Len(); got != tt.want {
			t.Errorf("Len(%v) = %d, want %d", tt.peers, got, tt.want)
		}
	}
}

func TestNewLatencyBalancer_Empty(t *testing.T) {
	b := NewLatencyBalancer(nil, nil)
	_, ok := b.Next()
	if ok {
		t.Error("expected false for empty balancer")
	}
}

func TestNewLatencyBalancer_AllKnown(t *testing.T) {
	peers := []string{"fast:5000", "slow:5000"}
	latencies := map[string]float64{
		"fast:5000": 0.01,
		"slow:5000": 1.0,
	}
	b := NewLatencyBalancer(peers, latencies)
	if b.Len() != 2 {
		t.Fatalf("expected 2 peers, got %d", b.Len())
	}

	fastCount := 0
	const iterations = 1000
	for i := 0; i < iterations; i++ {
		p, ok := b.Next()
		if !ok {
			t.Fatal("Next returned false")
		}
		if p == "fast:5000" {
			fastCount++
		}
	}
	// With inverse-latency weights: fast has ~100x weight, expect >>50% for fast
	if fastCount < 700 {
		t.Errorf("expected fast peer to win majority, got %d/%d", fastCount, iterations)
	}
}

func TestNewLatencyBalancer_UnknownPeers(t *testing.T) {
	peers := []string{"a:5000", "b:5000"}
	b := NewLatencyBalancer(peers, map[string]float64{})
	if b.Len() != 2 {
		t.Fatalf("expected 2 peers")
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, ok := b.Next()
		if !ok {
			t.Fatal("Next returned false")
		}
		seen[p] = true
	}
	if !seen["a:5000"] || !seen["b:5000"] {
		t.Errorf("expected both peers to be selected, got %v", seen)
	}
}

func TestNewLatencyBalancer_ZeroLatency(t *testing.T) {
	peers := []string{"a:5000"}
	b := NewLatencyBalancer(peers, map[string]float64{"a:5000": 0})
	p, ok := b.Next()
	if !ok || p != "a:5000" {
		t.Errorf("expected a:5000, got %q ok=%v", p, ok)
	}
}

// TestWeightedNext_Fallback exercises the "floating point edge case" fallback line in
// weightedNext. A balancer with an empty weights slice (non-nil but length 0) causes
// the for-range loop body to never execute, so execution falls through to the final
// return statement — the last peer.
func TestWeightedNext_Fallback(t *testing.T) {
	b := &Balancer{
		peers:   []string{"only:5000"},
		weights: []float64{}, // empty, not nil → loop body never executes → fallback fires
	}
	peer := b.weightedNext()
	if peer != "only:5000" {
		t.Errorf("expected fallback peer only:5000, got %q", peer)
	}
}
