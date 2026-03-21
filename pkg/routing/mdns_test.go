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
