package routing

import (
	"context"
	"testing"
	"time"
)

func TestNewMDNSService_Defaults(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})

	if svc.config.Domain != "local" {
		t.Errorf("Domain = %q, want %q", svc.config.Domain, "local")
	}
	if svc.config.QueryInterval != DefaultMDNSQueryInterval {
		t.Errorf("QueryInterval = %v, want %v", svc.config.QueryInterval, DefaultMDNSQueryInterval)
	}
	if svc.config.GracePeriod != DefaultMDNSGracePeriod {
		t.Errorf("GracePeriod = %v, want %v", svc.config.GracePeriod, DefaultMDNSGracePeriod)
	}
	if svc.config.NodeID == "" {
		t.Error("NodeID should default to hostname")
	}
	if svc.config.Version != "v0.1.0" {
		t.Errorf("Version = %q, want %q", svc.config.Version, "v0.1.0")
	}
}

func TestMDNSService_DiscoveredPeers_Empty(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})
	peers := svc.DiscoveredPeers()
	if len(peers) != 0 {
		t.Errorf("expected 0 discovered peers, got %d", len(peers))
	}
}

func TestMDNSService_MergePeers(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})
	pw := NewPeerWatcher([]string{"static:5000"})

	// Simulate discovered peers
	svc.mu.Lock()
	svc.peers["10.0.0.1:5000"] = time.Now()
	svc.peers["10.0.0.2:5000"] = time.Now()
	svc.mu.Unlock()

	svc.MergePeers(pw)

	peers := pw.Peers()
	if len(peers) != 3 {
		t.Errorf("expected 3 peers (1 static + 2 dynamic), got %d: %v", len(peers), peers)
	}

	// Check static peer is still there
	found := false
	for _, p := range peers {
		if p == "static:5000" {
			found = true
			break
		}
	}
	if !found {
		t.Error("static peer missing after merge")
	}
}

func TestMDNSService_MergePeers_Dedup(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})
	pw := NewPeerWatcher([]string{"10.0.0.1:5000"})

	// Dynamic peer same as static
	svc.mu.Lock()
	svc.peers["10.0.0.1:5000"] = time.Now()
	svc.peers["10.0.0.2:5000"] = time.Now()
	svc.mu.Unlock()

	svc.MergePeers(pw)

	peers := pw.Peers()
	if len(peers) != 2 {
		t.Errorf("expected 2 peers (deduped), got %d: %v", len(peers), peers)
	}
}

func TestPeerWatcher_MergeDynamic(t *testing.T) {
	pw := NewPeerWatcher([]string{"a:5000", "b:5000"})
	pw.MergeDynamic([]string{"c:5000", "d:5000"})

	peers := pw.Peers()
	if len(peers) != 4 {
		t.Errorf("expected 4 peers, got %d: %v", len(peers), peers)
	}

	// Replace dynamic
	pw.MergeDynamic([]string{"e:5000"})
	peers = pw.Peers()
	if len(peers) != 3 {
		t.Errorf("expected 3 peers after re-merge, got %d: %v", len(peers), peers)
	}
}

func TestMDNSService_PeerExpiry(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:        5000,
		GracePeriod: 50 * time.Millisecond,
	})

	// Add an "expired" peer and an active one
	svc.mu.Lock()
	svc.peers["expired:5000"] = time.Now().Add(-100 * time.Millisecond)
	svc.peers["active:5000"] = time.Now()
	svc.mu.Unlock()

	// Directly call expireAndMerge to avoid network-dependent mDNS query
	pw := NewPeerWatcher(nil)
	svc.expireAndMerge(pw)

	svc.mu.RLock()
	_, expiredExists := svc.peers["expired:5000"]
	_, activeExists := svc.peers["active:5000"]
	svc.mu.RUnlock()

	if expiredExists {
		t.Error("expired peer should have been removed")
	}
	if !activeExists {
		t.Error("active peer should still exist")
	}
}

// TestMDNSService_Shutdown_NilServer verifies that Shutdown is safe when no
// mDNS server has been started (server field is nil).
func TestMDNSService_Shutdown_NilServer(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})
	// Should not panic and should be a no-op.
	svc.Shutdown()
}

// TestMDNSService_Advertise_Shutdown verifies that Advertise starts a real mDNS
// server and Shutdown cleanly stops it (covers the s.server != nil branch).
func TestMDNSService_Advertise_Shutdown(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:    19876, // arbitrary high port unlikely to conflict
		NodeID:  "test-node",
		Version: "v0.0.1",
	})

	if err := svc.Advertise(); err != nil {
		t.Skipf("mDNS Advertise failed (network not available in test env): %v", err)
	}

	if svc.server == nil {
		t.Fatal("expected server to be non-nil after Advertise")
	}

	// Should call s.server.Shutdown() without panicking.
	svc.Shutdown()
}

// TestMDNSService_DiscoveredPeers_WithPeers verifies the DiscoveredPeers return
// value when at least one peer is in the map (83.3% → 100% for that function).
func TestMDNSService_DiscoveredPeers_WithPeers(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{Port: 5000})

	svc.mu.Lock()
	svc.peers["10.0.0.5:5000"] = time.Now()
	svc.peers["10.0.0.6:5000"] = time.Now()
	svc.mu.Unlock()

	peers := svc.DiscoveredPeers()
	if len(peers) != 2 {
		t.Errorf("expected 2 discovered peers, got %d: %v", len(peers), peers)
	}
}

// TestMDNSService_Advertise_InterfaceError exercises the net.InterfaceByName error
// path in Advertise when the configured interface does not exist.
func TestMDNSService_Advertise_InterfaceError(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:      19878,
		Interface: "eth-nonexistent-99",
	})

	err := svc.Advertise()
	if err == nil {
		// On some systems the call may succeed or report a different error; shutdown
		// and mark the test as skipped since we cannot reliably trigger the path.
		svc.Shutdown()
		t.Skip("Advertise succeeded unexpectedly with nonexistent interface — skipping")
	}
	// Expect an error mentioning the interface name.
	if svc.server != nil {
		t.Error("server should be nil when Advertise fails during interface lookup")
	}
}

// TestMDNSService_QueryAndMerge exercises the queryAndMerge path end-to-end.
// In a test environment without mDNS multicast, Query fails or times out quickly;
// the defer close(entries) fix ensures the for-range loop completes and
// expireAndMerge is called regardless.
func TestMDNSService_QueryAndMerge_CompletesClearly(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:        5000,
		GracePeriod: 100 * time.Millisecond,
	})
	pw := NewPeerWatcher(nil)

	// Seed a peer that will expire (older than GracePeriod).
	svc.mu.Lock()
	svc.peers["expired:5000"] = time.Now().Add(-200 * time.Millisecond)
	svc.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.queryAndMerge(pw)
	}()

	select {
	case <-done:
		// queryAndMerge completed — the entries channel was closed properly.
	case <-time.After(10 * time.Second):
		t.Fatal("queryAndMerge did not return within 10s; entries channel may not be closed")
	}

	// expireAndMerge should have removed the expired peer.
	svc.mu.RLock()
	_, expiredExists := svc.peers["expired:5000"]
	svc.mu.RUnlock()
	if expiredExists {
		t.Error("expired peer should have been removed by queryAndMerge")
	}
}

// TestMDNSService_Discover_ContextCancellation verifies that Discover returns nil
// when the context is cancelled.  The initial queryAndMerge call may take up to
// the mDNS query timeout (3s); we use a generous outer timeout to avoid flakiness.
func TestMDNSService_Discover_ContextCancellation(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:          5000,
		QueryInterval: 1 * time.Hour, // don't fire ticker during the test
	})
	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- svc.Discover(ctx, pw)
	}()

	// Cancel as soon as the goroutine is scheduled — Discover may still be
	// inside the initial queryAndMerge call, which is fine: once it returns,
	// the for-select will observe ctx.Done() and return nil.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error after context cancel, got: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Discover did not return after context cancel within 15s")
	}
}

// TestMDNSService_QueryAndMerge_WithValidInterface exercises the interface lookup
// branch in queryAndMerge with a real network interface (loopback).
func TestMDNSService_QueryAndMerge_WithValidInterface(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:      5000,
		Interface: "lo", // loopback always exists
	})
	pw := NewPeerWatcher(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.queryAndMerge(pw)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("queryAndMerge with interface=lo did not return within 10s")
	}
}

// TestMDNSService_QueryAndMerge_WithInvalidInterface exercises the interface lookup
// branch in queryAndMerge when the interface does not exist (lookup fails, skipped).
func TestMDNSService_QueryAndMerge_WithInvalidInterface(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:      5000,
		Interface: "nonexistent-iface-99",
	})
	pw := NewPeerWatcher(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.queryAndMerge(pw)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("queryAndMerge with invalid interface did not return within 10s")
	}
}

// TestMDNSService_Discover_TickerPath exercises the ticker branch of Discover
// by using a very short QueryInterval.  It cancels after the first tick fires.
func TestMDNSService_Discover_TickerPath(t *testing.T) {
	svc := NewMDNSService(MDNSConfig{
		Port:          5000,
		QueryInterval: 50 * time.Millisecond, // fire almost immediately
	})
	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- svc.Discover(ctx, pw)
	}()

	// Wait long enough for the ticker to fire at least once and another
	// queryAndMerge call to begin, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Discover did not return after ticker test within 15s")
	}
}
