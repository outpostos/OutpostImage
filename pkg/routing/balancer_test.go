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
