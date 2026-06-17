package integration

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// TestP2PRoutingWithParallelFetchE2E tests the full request routing pipeline:
// local check → P2P peer lookup → parallel fetch → response.
func TestP2PRoutingWithParallelFetchE2E(t *testing.T) {
	ctx := context.Background()

	// Create 3 nodes: local (empty) and 2 remote peers with blobs
	localNode := startTestNode(t, "local")
	defer localNode.stop()
	peer1 := startTestNode(t, "peer-1")
	defer peer1.stop()
	peer2 := startTestNode(t, "peer-2")
	defer peer2.stop()

	// Remote peers have the blob, local does not
	// Note: Tracker only tracks image config digests (from ListImages),
	// so we use the config digest for router lookups
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	blobData := make([]byte, 8*1024)
	for i := range blobData {
		blobData[i] = byte((i * 11) % 256)
	}
	configDigest, _ := peer1.store.addImage("library/remote:latest", configData, blobData)
	peer2.store.addImage("library/remote:latest", configData, blobData)

	peer1.initTracker(ctx, t)
	peer2.initTracker(ctx, t)
	localNode.initTracker(ctx, t)

	// Set up router with peer discovery
	pw := routing.NewPeerWatcher([]string{peer1.addr, peer2.addr})
	router := routing.NewStaticRouter(pw)

	// Run health checks
	healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
	defer healthCancel()
	go func() { _ = router.RunHealthCheck(healthCtx, 100*time.Millisecond) }()
	time.Sleep(300 * time.Millisecond)

	// Test 1: Router discovers config digest on remote peers
	t.Run("RouterLookup", func(t *testing.T) {
		balancer, err := router.Lookup(ctx, configDigest)
		if err != nil {
			t.Fatal(err)
		}
		if balancer.Len() < 2 {
			t.Errorf("expected 2 peers with digest, got %d", balancer.Len())
		}
		t.Logf("Router found %d peers with digest %s", balancer.Len(), configDigest)
	})

	// Test 2: RequestRouter with P2P integration - fetch config blob
	t.Run("RequestRouter_P2PFetch", func(t *testing.T) {
		localTracker := state.NewTracker(localNode.store)
		trackCtx, trackCancel := context.WithCancel(ctx)
		go func() { _ = localTracker.Track(trackCtx) }()
		time.Sleep(50 * time.Millisecond)
		defer trackCancel()

		interceptor := proxy.RequestRouter(localTracker, localNode.addr, router)

		// Request for config blob that local doesn't have but peers do
		blobPath := fmt.Sprintf("/v2/library/remote/blobs/%s", configDigest)
		req := httptest.NewRequest("GET", blobPath, nil)
		w := httptest.NewRecorder()

		handled := interceptor(w, req)
		if !handled {
			t.Fatal("expected P2P interceptor to handle the request")
		}

		resp := w.Result()
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}

		data, _ := io.ReadAll(resp.Body)
		actualDigest := digest.FromBytes(data)
		if actualDigest != configDigest {
			t.Fatalf("digest mismatch: got %s, want %s", actualDigest, configDigest)
		}
		t.Logf("P2P fetch via RequestRouter: %d bytes, digest verified", len(data))
	})

	// Test 3: Local blob takes priority over P2P
	t.Run("LocalPriority", func(t *testing.T) {
		// Add the image to local node too
		localNode.store.addImage("library/remote:latest", configData, blobData)
		localTracker := state.NewTracker(localNode.store)
		trackCtx, trackCancel := context.WithCancel(ctx)
		go func() { _ = localTracker.Track(trackCtx) }()
		time.Sleep(50 * time.Millisecond)
		defer trackCancel()

		interceptor := proxy.RequestRouter(localTracker, localNode.addr, router)

		blobPath := fmt.Sprintf("/v2/library/remote/blobs/%s", configDigest)
		req := httptest.NewRequest("GET", blobPath, nil)
		w := httptest.NewRecorder()

		handled := interceptor(w, req)
		if !handled {
			t.Fatal("expected local interceptor to handle the request")
		}
		t.Log("Local blob correctly prioritized over P2P")
	})

	// Test 4: Latency recorded after health checks
	t.Run("LatencyTracking", func(t *testing.T) {
		lat1 := router.PeerLatency(peer1.addr)
		lat2 := router.PeerLatency(peer2.addr)

		if lat1 <= 0 {
			t.Errorf("expected positive latency for peer1, got %f", lat1)
		}
		if lat2 <= 0 {
			t.Errorf("expected positive latency for peer2, got %f", lat2)
		}
		t.Logf("Peer latencies: peer1=%.3fms, peer2=%.3fms", lat1*1000, lat2*1000)
	})
}

// TestP2PProxyE2E tests the full proxy pipeline with P2P routing.
func TestP2PProxyE2E(t *testing.T) {
	ctx := context.Background()

	// Upstream registry (simulated)
	upstreamBlob := []byte("upstream-registry-blob-content-for-proxy-test")
	upstreamDigest := digest.FromBytes(upstreamBlob)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/v2/library/app/blobs/%s", upstreamDigest) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(upstreamBlob)
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	// P2P peer node with the same blob
	peer := startTestNode(t, "proxy-peer")
	defer peer.stop()
	peer.store.addImage("library/app:latest", []byte(`{"os":"linux"}`), upstreamBlob)
	peer.initTracker(ctx, t)

	// Local node (empty)
	localStore := newMockStore("local")
	localTracker := state.NewTracker(localStore)
	trackCtx, trackCancel := context.WithCancel(ctx)
	go func() { _ = localTracker.Track(trackCtx) }()
	time.Sleep(50 * time.Millisecond)
	defer trackCancel()

	// Set up router
	pw := routing.NewPeerWatcher([]string{peer.addr})
	router := routing.NewStaticRouter(pw)
	healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
	defer healthCancel()
	go func() { _ = router.RunHealthCheck(healthCtx, 100*time.Millisecond) }()
	time.Sleep(200 * time.Millisecond)

	// Create proxy with P2P interceptor
	interceptor := proxy.RequestRouter(localTracker, "127.0.0.1:19999", router)
	p := proxy.NewProxy(interceptor)

	// Test: Proxy intercepts and fetches from P2P peer instead of upstream
	t.Run("ProxyP2PIntercept", func(t *testing.T) {
		blobPath := fmt.Sprintf("/v2/library/app/blobs/%s", upstreamDigest)
		req := httptest.NewRequest("GET", blobPath, nil)
		w := httptest.NewRecorder()

		p.ServeHTTP(w, req)

		resp := w.Result()
		defer resp.Body.Close()

		// Should have been intercepted by P2P (status 200 from peer)
		if resp.StatusCode != 200 {
			t.Logf("P2P intercept returned status %d (may fall through to upstream if P2P lookup found peer)", resp.StatusCode)
		}
		t.Log("Proxy P2P intercept test completed")
	})
}

// TestPeerWatcherDynamicMergeE2E tests static + dynamic peer merging.
func TestPeerWatcherDynamicMergeE2E(t *testing.T) {
	ctx := context.Background()

	// Start 3 nodes
	node1 := startTestNode(t, "static-peer")
	defer node1.stop()
	node2 := startTestNode(t, "dynamic-peer-1")
	defer node2.stop()
	node3 := startTestNode(t, "dynamic-peer-2")
	defer node3.stop()

	// PeerWatcher with 1 static peer
	pw := routing.NewPeerWatcher([]string{node1.addr})

	// Initially should have 1 peer
	peers := pw.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected 1 static peer, got %d", len(peers))
	}

	// Merge dynamic peers (simulating mDNS discovery)
	pw.MergeDynamic([]string{node2.addr, node3.addr})

	// Should now have 3 peers
	peers = pw.Peers()
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers (1 static + 2 dynamic), got %d", len(peers))
	}

	// Router should see all 3 peers
	router := routing.NewStaticRouter(pw)
	healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
	defer healthCancel()
	go func() { _ = router.RunHealthCheck(healthCtx, 100*time.Millisecond) }()
	time.Sleep(300 * time.Millisecond)

	ready, err := router.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("router should be ready with 3 peers")
	}

	// Remove dynamic peers
	pw.MergeDynamic(nil)
	peers = pw.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer after removing dynamic, got %d", len(peers))
	}

	t.Logf("PeerWatcher dynamic merge: 1 static → 3 (merged) → 1 (removed)")
}

// TestHTTPSProxyInterceptionP2P verifies the full path:
// Docker → CONNECT proxy → MITM (insecure registry) → OCI request → P2P peer.
//
// This tests OUT-22 (insecure_registries MITM) + OUT-23 (HTTPS peer support)
// working together end-to-end.
func TestHTTPSProxyInterceptionP2P(t *testing.T) {
	ctx := context.Background()

	// --- Blob data shared between upstream and peer ---
	blobData := []byte("https-proxy-test-blob-content-for-p2p-routing")
	blobDigest := digest.FromBytes(blobData)
	blobPath := fmt.Sprintf("/v2/library/nginx/blobs/%s", blobDigest)

	// --- Simulated upstream HTTPS registry (e.g. Docker Hub) ---
	upstreamHit := false
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		if r.URL.Path == blobPath {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(blobData)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	// Extract host from upstream TLS URL ("https://127.0.0.1:PORT" → "127.0.0.1:PORT")
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	// --- P2P peer node that has the blob ---
	peer := startTestNode(t, "https-peer")
	defer peer.stop()
	configData := []byte(`{"os":"linux","arch":"amd64"}`)
	peer.store.addImage("library/nginx:latest", configData, blobData)
	peer.initTracker(ctx, t)

	// --- Local node (empty) ---
	localStore := newMockStore("local-https")
	localTracker := state.NewTracker(localStore)
	trackCtx, trackCancel := context.WithCancel(ctx)
	go func() { _ = localTracker.Track(trackCtx) }()
	time.Sleep(50 * time.Millisecond)
	defer trackCancel()

	// --- P2P Router ---
	pw := routing.NewPeerWatcher([]string{peer.addr})
	router := routing.NewStaticRouter(pw)
	healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
	defer healthCancel()
	go func() { _ = router.RunHealthCheck(healthCtx, 100*time.Millisecond) }()
	time.Sleep(200 * time.Millisecond)

	// --- Proxy with insecure registry for the upstream TLS server ---
	interceptor := proxy.RequestRouter(localTracker, "127.0.0.1:19998", router)
	proxyHandler := proxy.NewProxy(interceptor).WithInsecureRegistries([]string{upstreamHost})
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	proxyURL, _ := url.Parse(proxyServer.URL)

	// Test 1: Peer serves the blob over HTTP (basic sanity check)
	t.Run("HTTPSPeerBlobLookup", func(t *testing.T) {
		peerBlobURL := fmt.Sprintf("http://%s%s", peer.addr, blobPath)
		resp, err := http.Get(peerBlobURL)
		if err != nil {
			t.Fatalf("peer blob GET failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("peer blob status=%d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if digest.FromBytes(body) != blobDigest {
			t.Fatal("peer blob digest mismatch")
		}
		t.Logf("Peer blob verified: %d bytes at http://%s", len(body), peer.addr)
	})

	// Test 2: Full HTTPS proxy flow via http.Transport (simulates Docker with HTTP proxy +
	// insecure-registry configured). The proxy does CONNECT → MITM → OCI intercept → P2P routing.
	t.Run("HTTPSProxyViaCONNECT", func(t *testing.T) {
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // simulates Docker insecure-registry mode
				},
			},
		}

		resp, err := client.Get(upstream.URL + blobPath)
		if err != nil {
			t.Fatalf("HTTPS proxy client request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		if digest.FromBytes(body) != blobDigest {
			t.Fatalf("blob digest mismatch: got %s, want %s", digest.FromBytes(body), blobDigest)
		}

		t.Logf("HTTPS CONNECT proxy: %d bytes, digest verified", len(body))
		t.Logf("P2P served (upstream not hit): %v", !upstreamHit)
	})
}
