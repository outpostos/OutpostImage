package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

func TestStaticRouter_Ready_NoPeers(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)
	ok, err := r.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected not ready with no peers")
	}
}

func TestStaticRouter_Ready_WithPeers(t *testing.T) {
	pw := NewPeerWatcher([]string{"peer1:5000"})
	r := NewStaticRouter(pw)
	ok, err := r.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected ready with peers")
	}
}

func TestStaticRouter_Ready_HealthyPeer(t *testing.T) {
	pw := NewPeerWatcher([]string{"peer1:5000"})
	r := NewStaticRouter(pw)
	r.mu.Lock()
	r.health["peer1:5000"] = true
	r.mu.Unlock()

	ok, err := r.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected ready with healthy peer")
	}
}

func TestStaticRouter_Lookup_NoPeers(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)
	dgst := digest.FromString("test")
	bal, err := r.Lookup(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Len() != 0 {
		t.Errorf("expected 0 holders, got %d", bal.Len())
	}
}

func TestStaticRouter_Lookup_PeerHasDigest(t *testing.T) {
	dgst := digest.FromString("test")

	// Start a mock peer server that responds "exists: true"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/digests/") {
			json.NewEncoder(w).Encode(map[string]bool{"exists": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	bal, err := r.Lookup(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Len() != 1 {
		t.Errorf("expected 1 holder, got %d", bal.Len())
	}
	peer, ok := bal.Next()
	if !ok || peer != addr {
		t.Errorf("expected peer %s, got %s", addr, peer)
	}
}

func TestStaticRouter_Lookup_PeerDoesNotHaveDigest(t *testing.T) {
	dgst := digest.FromString("missing")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"exists": false})
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	bal, err := r.Lookup(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Len() != 0 {
		t.Errorf("expected 0 holders, got %d", bal.Len())
	}
}

func TestStaticRouter_Advertise(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)
	err := r.Advertise(context.Background(), nil)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestStaticRouter_Withdraw(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)
	err := r.Withdraw(context.Background(), nil)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestStaticRouter_HealthCheck(t *testing.T) {
	// Healthy peer
	srvHealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srvHealthy.Close()

	// Unhealthy peer
	srvUnhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srvUnhealthy.Close()

	addrHealthy := strings.TrimPrefix(srvHealthy.URL, "http://")
	addrUnhealthy := strings.TrimPrefix(srvUnhealthy.URL, "http://")

	pw := NewPeerWatcher([]string{addrHealthy, addrUnhealthy})
	r := NewStaticRouter(pw)

	r.checkAllPeers(context.Background())

	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.health[addrHealthy] {
		t.Errorf("expected healthy peer %s to be marked healthy", addrHealthy)
	}
	// Unhealthy peer needs 3 consecutive failures to be marked
	if r.failures[addrUnhealthy] != 1 {
		t.Errorf("expected 1 failure for unhealthy peer, got %d", r.failures[addrUnhealthy])
	}
}

func TestStaticRouter_HealthCheck_Backoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	// Simulate 3 failures to trigger backoff
	for i := 0; i < 3; i++ {
		r.checkAllPeers(context.Background())
	}

	r.mu.RLock()
	healthy := r.health[addr]
	_, hasBackoff := r.backoff[addr]
	r.mu.RUnlock()

	if healthy {
		t.Error("expected peer to be marked unhealthy after 3 failures")
	}
	if !hasBackoff {
		t.Error("expected backoff to be set")
	}

	// Verify peer is excluded from healthy peers
	peers := r.healthyPeers()
	for _, p := range peers {
		if p == addr {
			t.Error("expected unhealthy peer to be excluded from healthy peers")
		}
	}
}

func TestStaticRouter_BackoffDuration(t *testing.T) {
	r := NewStaticRouter(NewPeerWatcher(nil))
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{1, 1 * time.Minute},
		{3, 1 * time.Minute},
		{4, 2 * time.Minute},
		{5, 2 * time.Minute},
		{6, 5 * time.Minute},
		{8, 5 * time.Minute},
		{9, 10 * time.Minute},
		{100, 10 * time.Minute},
	}
	for _, tt := range tests {
		got := r.backoffDuration(tt.failures)
		if got != tt.want {
			t.Errorf("backoffDuration(%d) = %v, want %v", tt.failures, got, tt.want)
		}
	}
}

func TestStaticRouter_HealthyPeers_Recovery(t *testing.T) {
	pw := NewPeerWatcher([]string{"peer:5000"})
	r := NewStaticRouter(pw)

	// Mark peer unhealthy with past backoff
	r.mu.Lock()
	r.health["peer:5000"] = false
	r.backoff["peer:5000"] = time.Now().Add(-1 * time.Minute) // expired
	r.mu.Unlock()

	peers := r.healthyPeers()
	found := false
	for _, p := range peers {
		if p == "peer:5000" {
			found = true
		}
	}
	if !found {
		t.Error("expected peer with expired backoff to be included in healthy peers")
	}
}
