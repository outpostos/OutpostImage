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
			_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
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
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": false})
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
	peers := r.HealthyPeers()
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

	peers := r.HealthyPeers()
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

func TestStaticRouter_PeerLatency_Default(t *testing.T) {
	pw := NewPeerWatcher([]string{"peer:5000"})
	r := NewStaticRouter(pw)

	// Before any health check, latency should be 0
	lat := r.PeerLatency("peer:5000")
	if lat != 0 {
		t.Errorf("expected 0 latency for unknown peer, got %f", lat)
	}
}

func TestStaticRouter_PeerLatency_AfterHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	r.checkAllPeers(context.Background())

	lat := r.PeerLatency(addr)
	if lat < 0 {
		t.Errorf("expected non-negative latency after health check, got %f", lat)
	}
}

// TestStaticRouter_Lookup_PeerDigestReturns404 exercises the non-200 path in
// queryPeerDigest — a 404 response counts as "does not have digest".
func TestStaticRouter_Lookup_PeerDigestReturns404(t *testing.T) {
	dgst := digest.FromString("test-404-digest")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if bal.Len() != 0 {
		t.Errorf("expected 0 holders when peer returns 404, got %d", bal.Len())
	}
}

// TestStaticRouter_Lookup_WithLatency exercises the latency-aware balancer path in
// buildBalancer — requires the peer to have an EWMA latency entry from a prior health check.
func TestStaticRouter_Lookup_WithLatency(t *testing.T) {
	dgst := digest.FromString("test-latency-digest")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/digests/") {
			_ = json.NewEncoder(w).Encode(map[string]bool{"exists": true})
			return
		}
		w.WriteHeader(http.StatusOK) // healthz
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	// Run a health check to populate the latency map.
	r.checkAllPeers(context.Background())

	// Lookup should now use NewLatencyBalancer since r.latency[addr] is set.
	bal, err := r.Lookup(context.Background(), dgst)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Len() != 1 {
		t.Errorf("expected 1 holder with latency balancer, got %d", bal.Len())
	}
}

// TestStaticRouter_CheckPeerHealth_ConnectionRefused exercises the client.Do error
// path in checkPeerHealth when the peer is unreachable.
func TestStaticRouter_CheckPeerHealth_ConnectionRefused(t *testing.T) {
	pw := NewPeerWatcher([]string{"127.0.0.1:1"}) // port 1 is always unreachable
	r := NewStaticRouter(pw)

	r.checkAllPeers(context.Background())

	r.mu.RLock()
	failures := r.failures["127.0.0.1:1"]
	r.mu.RUnlock()

	if failures != 1 {
		t.Errorf("expected 1 failure for unreachable peer, got %d", failures)
	}
}

func TestStaticRouter_RunHealthCheck_CancelledContext(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := r.RunHealthCheck(ctx, 100*time.Millisecond)
	if err != nil {
		t.Errorf("expected nil error on cancelled context, got %v", err)
	}
}

func TestStaticRouter_RunHealthCheck_RunsChecks(t *testing.T) {
	checkCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	// Run with 50ms interval; should fire the initial check + at least 1 ticker check
	_ = r.RunHealthCheck(ctx, 80*time.Millisecond)

	if checkCount < 2 {
		t.Errorf("expected at least 2 health checks, got %d", checkCount)
	}
}

// TestStaticRouter_CheckPeerHealth_InvalidURL exercises the http.NewRequestWithContext
// error path in checkPeerHealth when the peer address is malformed.
func TestStaticRouter_CheckPeerHealth_InvalidURL(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)

	// "[::1]" is syntactically valid but ":badport" forces a parse error.
	ok, rtt := r.checkPeerHealth(context.Background(), "bad host]invalid")
	if ok {
		t.Error("expected false for invalid URL")
	}
	if rtt != 0 {
		t.Errorf("expected rtt=0 for invalid URL, got %f", rtt)
	}
}

// TestStaticRouter_QueryPeerDigest_InvalidURL exercises the http.NewRequestWithContext
// error path in queryPeerDigest when the peer address is malformed.
func TestStaticRouter_QueryPeerDigest_InvalidURL(t *testing.T) {
	pw := NewPeerWatcher(nil)
	r := NewStaticRouter(pw)

	dgst := digest.FromString("test")
	has := r.queryPeerDigest(context.Background(), "bad host]invalid", dgst)
	if has {
		t.Error("expected false for invalid URL")
	}
}

// TestStaticRouter_HealthyPeers_UnhealthyExpiredBackoff verifies that a peer
// marked unhealthy but with an expired backoff is included in HealthyPeers.
// Once the backoff timer expires, the peer is given another chance regardless
// of its last health state.
func TestStaticRouter_HealthyPeers_UnhealthyExpiredBackoff(t *testing.T) {
	pw := NewPeerWatcher([]string{"peer:5000"})
	r := NewStaticRouter(pw)

	r.mu.Lock()
	r.health["peer:5000"] = false
	r.backoff["peer:5000"] = time.Now().Add(-1 * time.Second) // expired
	r.mu.Unlock()

	peers := r.HealthyPeers()
	found := false
	for _, p := range peers {
		if p == "peer:5000" {
			found = true
		}
	}
	if !found {
		t.Error("peer with expired backoff and health=false should be included")
	}
}

// TestStaticRouter_Lookup_ContextCancelled exercises the ctx.Done() branch inside
// the Lookup result-collection loop.
func TestStaticRouter_Lookup_ContextCancelled(t *testing.T) {
	// Use a slow server so the goroutine doesn't return before ctx is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	pw := NewPeerWatcher([]string{addr})
	r := NewStaticRouter(pw)

	dgst := digest.FromString("test-ctx-cancel")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	bal, err := r.Lookup(ctx, dgst)
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	// The balancer may be empty (digest query timed out) — that is the expected path.
	_ = bal
}
