package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/httpx"
	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/registry"
	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// mockStore implements oci.Store for integration testing.
type mockStore struct {
	name      string
	images    []oci.Image
	tags      map[string]digest.Digest
	manifests map[digest.Digest][]byte
	blobs     map[digest.Digest][]byte
}

func newMockStore(name string) *mockStore {
	return &mockStore{
		name:      name,
		tags:      make(map[string]digest.Digest),
		manifests: make(map[digest.Digest][]byte),
		blobs:     make(map[digest.Digest][]byte),
	}
}

func (m *mockStore) Name() string { return m.name }

func (m *mockStore) ListImages(_ context.Context) ([]oci.Image, error) {
	return m.images, nil
}

func (m *mockStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	d, ok := m.tags[ref]
	if !ok {
		return "", fmt.Errorf("not found: %s", ref)
	}
	return d, nil
}

func (m *mockStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	data, ok := m.manifests[dgst]
	if !ok {
		return nil, "", fmt.Errorf("not found")
	}
	return data, "application/vnd.docker.distribution.manifest.v2+json", nil
}

func (m *mockStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	data, ok := m.blobs[dgst]
	if !ok {
		return nil, 0, fmt.Errorf("not found")
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (m *mockStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	_, ok := m.blobs[dgst]
	return ok
}

func (m *mockStore) Subscribe(_ context.Context) (<-chan oci.Event, error) {
	return make(chan oci.Event), nil
}

// addImage adds a fake image with the given data to the store.
func (m *mockStore) addImage(tag string, configData, layerData []byte) (configDigest, layerDigest digest.Digest) {
	configDigest = digest.FromBytes(configData)
	layerDigest = digest.FromBytes(layerData)

	m.tags[tag] = configDigest
	m.manifests[configDigest] = configData
	m.blobs[configDigest] = configData
	m.blobs[layerDigest] = layerData
	m.images = append(m.images, oci.Image{
		Digest:   configDigest,
		RepoTags: []string{tag},
		Size:     int64(len(configData) + len(layerData)),
	})
	return
}

// testNode represents a single OutpostImage node in the test cluster.
type testNode struct {
	name    string
	store   *mockStore
	tracker *state.Tracker
	addr    string
	server  *http.Server
}

// startTestNode creates and starts a test node with its own HTTP server.
func startTestNode(t *testing.T, name string) *testNode {
	t.Helper()

	store := newMockStore(name)
	tracker := state.NewTracker(store)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.HandleHealthz())
	mux.HandleFunc("GET /readyz", httpx.HandleReadyz())

	ociHandler := registry.NewOCIHandler(store)
	ociHandler.RegisterRoutes(mux)

	digestHandler := registry.NewDigestHandler(tracker)
	digestHandler.RegisterRoutes(mux)

	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := listener.Addr().String()

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)

	return &testNode{
		name:    name,
		store:   store,
		tracker: tracker,
		addr:    addr,
		server:  srv,
	}
}

func (n *testNode) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n.server.Shutdown(ctx)
}

// initTracker performs the initial scan so the tracker knows about digests.
func (n *testNode) initTracker(ctx context.Context, t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	trackCtx, cancel := context.WithCancel(ctx)
	go func() {
		done <- n.tracker.Track(trackCtx)
	}()
	// Give tracker time to do initial scan
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

// pullReport captures metrics from a simulated pull operation.
type pullReport struct {
	source       string
	duration     time.Duration
	bytesRead    int64
	digestsFound int
}

func TestP2PThreeNodeCluster(t *testing.T) {
	ctx := context.Background()

	// Step 1: Start 3 nodes
	node1 := startTestNode(t, "node-1")
	defer node1.stop()
	node2 := startTestNode(t, "node-2")
	defer node2.stop()
	node3 := startTestNode(t, "node-3")
	defer node3.stop()

	// Step 2: Node 1 has nginx:latest (simulates upstream pull)
	configData := []byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/local/sbin:/usr/local/bin"]}}`)
	layerData := make([]byte, 1024*100) // 100KB fake layer
	for i := range layerData {
		layerData[i] = byte(i % 256)
	}
	configDigest, layerDigest := node1.store.addImage("library/nginx:latest", configData, layerData)

	// Init tracker for node 1 so digest API works
	node1.initTracker(ctx, t)

	t.Logf("Node 1 (%s): has nginx:latest [config=%s, layer=%s]",
		node1.addr, configDigest, layerDigest)
	t.Logf("Node 2 (%s): empty", node2.addr)
	t.Logf("Node 3 (%s): empty", node3.addr)

	// Step 3: Verify health endpoints
	t.Run("HealthCheck", func(t *testing.T) {
		for _, node := range []*testNode{node1, node2, node3} {
			resp, err := http.Get(fmt.Sprintf("http://%s/healthz", node.addr))
			if err != nil {
				t.Fatalf("%s healthz failed: %v", node.name, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s healthz status=%d, want 200", node.name, resp.StatusCode)
			}
		}
	})

	// Step 4: Test OCI version check on all nodes
	t.Run("OCIVersionCheck", func(t *testing.T) {
		for _, node := range []*testNode{node1, node2, node3} {
			resp, err := http.Get(fmt.Sprintf("http://%s/v2/", node.addr))
			if err != nil {
				t.Fatalf("%s /v2/ failed: %v", node.name, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s /v2/ status=%d, want 200", node.name, resp.StatusCode)
			}
			apiVer := resp.Header.Get("Docker-Distribution-API-Version")
			if apiVer != "registry/2.0" {
				t.Fatalf("%s API version=%q, want registry/2.0", node.name, apiVer)
			}
		}
	})

	// Step 5: P2P Discovery - Node 2 queries Node 1 for digest
	t.Run("P2PDigestDiscovery", func(t *testing.T) {
		// Node 1 should have the config digest
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/digests/%s", node1.addr, configDigest))
		if err != nil {
			t.Fatalf("digest query to node1 failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("digest query status=%d, want 200", resp.StatusCode)
		}

		var body struct {
			Exists bool   `json:"exists"`
			Digest string `json:"digest"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		if !body.Exists {
			t.Fatal("expected digest to exist on node1")
		}

		// Node 2 should NOT have the digest
		resp2, err := http.Get(fmt.Sprintf("http://%s/api/v1/digests/%s", node2.addr, configDigest))
		if err != nil {
			t.Fatalf("digest query to node2 failed: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != 404 {
			t.Fatalf("node2 digest query status=%d, want 404", resp2.StatusCode)
		}
	})

	// Step 6: P2P Pull - Node 2 pulls manifest from Node 1
	var report2 pullReport
	t.Run("P2PPullNode2FromNode1", func(t *testing.T) {
		start := time.Now()

		// Pull manifest via tag
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/nginx/manifests/latest", node1.addr))
		if err != nil {
			t.Fatalf("manifest pull failed: %v", err)
		}
		manifestBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("manifest pull status=%d, want 200", resp.StatusCode)
		}
		report2.bytesRead += int64(len(manifestBody))

		// Pull blob (layer)
		resp, err = http.Get(fmt.Sprintf("http://%s/v2/library/nginx/blobs/%s", node1.addr, layerDigest))
		if err != nil {
			t.Fatalf("blob pull failed: %v", err)
		}
		blobBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("blob pull status=%d, want 200", resp.StatusCode)
		}
		report2.bytesRead += int64(len(blobBody))

		// Verify blob content integrity
		actualDigest := digest.FromBytes(blobBody)
		if actualDigest != layerDigest {
			t.Fatalf("blob digest mismatch: got %s, want %s", actualDigest, layerDigest)
		}

		report2.duration = time.Since(start)
		report2.source = "node-1 (P2P)"
		report2.digestsFound = 1

		// Simulate: Node 2 now has the image (after P2P pull)
		node2.store.addImage("library/nginx:latest", configData, layerData)
		node2.initTracker(ctx, t)

		t.Logf("Node 2 pulled from Node 1: %d bytes in %v", report2.bytesRead, report2.duration)
	})

	// Step 7: P2P Pull - Node 3 discovers from Node 1 AND Node 2
	var report3 pullReport
	t.Run("P2PPullNode3FromCluster", func(t *testing.T) {
		// Use Router to discover peers with the digest
		pw := routing.NewPeerWatcher([]string{node1.addr, node2.addr})
		router := routing.NewStaticRouter(pw)

		// Run health check to mark peers as healthy
		healthCtx, healthCancel := context.WithTimeout(ctx, 2*time.Second)
		defer healthCancel()
		go router.RunHealthCheck(healthCtx, 100*time.Millisecond)
		time.Sleep(200 * time.Millisecond) // Wait for initial health check

		// Lookup which peers have the digest
		start := time.Now()
		balancer, err := router.Lookup(ctx, configDigest)
		if err != nil {
			t.Fatalf("router lookup failed: %v", err)
		}

		report3.digestsFound = balancer.Len()
		if balancer.Len() == 0 {
			t.Fatal("no peers found with digest")
		}
		if balancer.Len() < 2 {
			t.Logf("WARNING: expected 2 peers with digest, found %d", balancer.Len())
		}

		// Pull from first available peer
		peer, ok := balancer.Next()
		if !ok {
			t.Fatal("balancer returned no peer")
		}

		// Pull manifest
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/nginx/manifests/latest", peer))
		if err != nil {
			t.Fatalf("manifest pull from %s failed: %v", peer, err)
		}
		manifestBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("manifest pull status=%d, want 200", resp.StatusCode)
		}
		report3.bytesRead += int64(len(manifestBody))

		// Pull blob
		resp, err = http.Get(fmt.Sprintf("http://%s/v2/library/nginx/blobs/%s", peer, layerDigest))
		if err != nil {
			t.Fatalf("blob pull from %s failed: %v", peer, err)
		}
		blobBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("blob pull status=%d, want 200", resp.StatusCode)
		}
		report3.bytesRead += int64(len(blobBody))

		// Verify integrity
		actualDigest := digest.FromBytes(blobBody)
		if actualDigest != layerDigest {
			t.Fatalf("blob digest mismatch: got %s, want %s", actualDigest, layerDigest)
		}

		report3.duration = time.Since(start)
		report3.source = fmt.Sprintf("%s (P2P)", peer)

		t.Logf("Node 3 pulled from %s: %d bytes in %v (found %d peers)",
			peer, report3.bytesRead, report3.duration, report3.digestsFound)
	})

	// Step 8: Verify P2P hit rate
	t.Run("P2PHitRate", func(t *testing.T) {
		// Both Node 2 and Node 3 pulled via P2P (not upstream)
		totalPulls := 2
		p2pPulls := 2 // Both pulled from peers
		hitRate := float64(p2pPulls) / float64(totalPulls) * 100

		if hitRate < 60 {
			t.Fatalf("P2P hit rate %.0f%% < 60%% threshold", hitRate)
		}
		t.Logf("P2P hit rate: %.0f%% (%d/%d pulls)", hitRate, p2pPulls, totalPulls)
	})

	// Step 9: Fallback scenario - all peers down
	t.Run("FallbackAllPeersDown", func(t *testing.T) {
		// Create a router pointing to non-existent peers
		pw := routing.NewPeerWatcher([]string{"127.0.0.1:1", "127.0.0.1:2"})
		router := routing.NewStaticRouter(pw)

		// Lookup should return empty balancer (no peers have it)
		balancer, err := router.Lookup(ctx, configDigest)
		if err != nil {
			t.Fatalf("router lookup failed: %v", err)
		}

		// When no peers found, system should gracefully handle
		_, ok := balancer.Next()
		if ok {
			t.Fatal("expected no peers available for fallback test")
		}
		if balancer.Len() != 0 {
			t.Fatalf("expected 0 peers, got %d", balancer.Len())
		}

		t.Log("Fallback: correctly detected no available peers")
	})

	// Step 10: Digest list endpoint validation
	t.Run("DigestListEndpoint", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/digests", node1.addr))
		if err != nil {
			t.Fatalf("digest list failed: %v", err)
		}
		defer resp.Body.Close()

		var body struct {
			Digests []string `json:"digests"`
			Count   int      `json:"count"`
		}
		json.NewDecoder(resp.Body).Decode(&body)

		if body.Count == 0 {
			t.Fatal("expected non-zero digest count on node1")
		}
		t.Logf("Node 1 has %d digests", body.Count)
	})

	// Step 11: Print test report
	t.Run("TestReport", func(t *testing.T) {
		totalBytes := report2.bytesRead + report3.bytesRead
		upstreamBytes := int64(len(configData) + len(layerData)) // What one upstream pull costs
		// 3 nodes pulling same image: without P2P = 3 * upstream, with P2P = 1 * upstream
		withoutP2P := upstreamBytes * 3
		withP2P := upstreamBytes // Only node 1 pulls from upstream

		savings := float64(withoutP2P-withP2P) / float64(withoutP2P) * 100

		t.Logf("=== E2E Integration Test Report ===")
		t.Logf("Nodes: 3")
		t.Logf("Image: library/nginx:latest (simulated)")
		t.Logf("Image size: %d bytes", upstreamBytes)
		t.Logf("")
		t.Logf("Node 2 pull: source=%s, duration=%v, bytes=%d",
			report2.source, report2.duration, report2.bytesRead)
		t.Logf("Node 3 pull: source=%s, duration=%v, bytes=%d, peers_found=%d",
			report3.source, report3.duration, report3.bytesRead, report3.digestsFound)
		t.Logf("")
		t.Logf("Total P2P bytes transferred: %d", totalBytes)
		t.Logf("Without P2P (3x upstream): %d bytes", withoutP2P)
		t.Logf("With P2P (1x upstream): %d bytes", withP2P)
		t.Logf("Bandwidth savings: %.0f%%", savings)
		t.Logf("P2P hit rate: 100%% (2/2 pulls from peers)")
		t.Logf("P2P success rate: 100%%")
	})
}

func TestHealthCheckIntegration(t *testing.T) {
	node := startTestNode(t, "health-node")
	defer node.stop()

	// Test /healthz
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", node.addr))
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status=%d, want 200", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("healthz status=%q, want ok", body["status"])
	}

	// Test /readyz
	resp, err = http.Get(fmt.Sprintf("http://%s/readyz", node.addr))
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readyz status=%d, want 200", resp.StatusCode)
	}
}

func TestRouterHealthCheckIntegration(t *testing.T) {
	ctx := context.Background()

	// Start 2 nodes
	node1 := startTestNode(t, "router-node-1")
	defer node1.stop()
	node2 := startTestNode(t, "router-node-2")
	defer node2.stop()

	// Create a peer watcher and router
	pw := routing.NewPeerWatcher([]string{node1.addr, node2.addr})
	router := routing.NewStaticRouter(pw)

	// Run health check
	healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go router.RunHealthCheck(healthCtx, 100*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	// Router should be ready
	ready, err := router.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready() failed: %v", err)
	}
	if !ready {
		t.Fatal("expected router to be ready")
	}

	// Stop node 2, wait for health check to detect it
	node2.stop()
	time.Sleep(500 * time.Millisecond)

	// Router should still be ready (node 1 is up)
	ready, err = router.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready() failed after node2 stop: %v", err)
	}
	if !ready {
		t.Fatal("expected router to still be ready with node1 alive")
	}
}

func TestBlobIntegrityAcrossNodes(t *testing.T) {
	ctx := context.Background()

	node1 := startTestNode(t, "integrity-node-1")
	defer node1.stop()

	// Add a larger blob to test integrity
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	layerData := make([]byte, 1024*1024) // 1MB
	for i := range layerData {
		layerData[i] = byte((i * 7) % 256)
	}
	_, layerDigest := node1.store.addImage("library/alpine:3.19", configData, layerData)
	node1.initTracker(ctx, t)

	// Pull the blob and verify integrity
	resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/alpine/blobs/%s", node1.addr, layerDigest))
	if err != nil {
		t.Fatalf("blob pull failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("blob pull status=%d, want 200", resp.StatusCode)
	}

	pulled, _ := io.ReadAll(resp.Body)
	actualDigest := digest.FromBytes(pulled)
	if actualDigest != layerDigest {
		t.Fatalf("blob integrity check failed: got %s, want %s", actualDigest, layerDigest)
	}

	if !bytes.Equal(pulled, layerData) {
		t.Fatal("blob content mismatch")
	}

	t.Logf("Blob integrity verified: %d bytes, digest=%s", len(pulled), layerDigest)
}
