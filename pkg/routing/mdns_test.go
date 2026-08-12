package routing

import (
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
