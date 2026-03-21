package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/routing"
)

// TestRangeRequestE2E tests HTTP Range request support on the OCI blob endpoint.
func TestRangeRequestE2E(t *testing.T) {
	ctx := context.Background()

	node := startTestNode(t, "range-node")
	defer node.stop()

	// Add a 10KB blob
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	blobData := make([]byte, 10*1024)
	for i := range blobData {
		blobData[i] = byte(i % 256)
	}
	_, blobDigest := node.store.addImage("library/test:latest", configData, blobData)
	node.initTracker(ctx, t)

	blobURL := fmt.Sprintf("http://%s/v2/library/test/blobs/%s", node.addr, blobDigest)

	// Test 1: HEAD should include Accept-Ranges: bytes
	t.Run("HEAD_AcceptRanges", func(t *testing.T) {
		req, _ := http.NewRequest("HEAD", blobURL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Fatalf("HEAD status=%d, want 200", resp.StatusCode)
		}
		if resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Errorf("Accept-Ranges=%q, want 'bytes'", resp.Header.Get("Accept-Ranges"))
		}
		if resp.ContentLength != int64(len(blobData)) {
			t.Errorf("Content-Length=%d, want %d", resp.ContentLength, len(blobData))
		}
		t.Logf("HEAD: Accept-Ranges=bytes, Content-Length=%d", resp.ContentLength)
	})

	// Test 2: Range request - first 1024 bytes
	t.Run("Range_First1024", func(t *testing.T) {
		req, _ := http.NewRequest("GET", blobURL, nil)
		req.Header.Set("Range", "bytes=0-1023")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 206 {
			t.Fatalf("status=%d, want 206 Partial Content", resp.StatusCode)
		}

		contentRange := resp.Header.Get("Content-Range")
		expected := fmt.Sprintf("bytes 0-1023/%d", len(blobData))
		if contentRange != expected {
			t.Errorf("Content-Range=%q, want %q", contentRange, expected)
		}

		data, _ := io.ReadAll(resp.Body)
		if len(data) != 1024 {
			t.Fatalf("got %d bytes, want 1024", len(data))
		}

		// Verify content matches original
		for i := 0; i < 1024; i++ {
			if data[i] != blobData[i] {
				t.Fatalf("byte mismatch at offset %d", i)
			}
		}
		t.Log("Range 0-1023: 1024 bytes, content verified")
	})

	// Test 3: Range request - middle chunk
	t.Run("Range_MiddleChunk", func(t *testing.T) {
		req, _ := http.NewRequest("GET", blobURL, nil)
		req.Header.Set("Range", "bytes=2048-4095")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 206 {
			t.Fatalf("status=%d, want 206", resp.StatusCode)
		}

		data, _ := io.ReadAll(resp.Body)
		if len(data) != 2048 {
			t.Fatalf("got %d bytes, want 2048", len(data))
		}

		for i := 0; i < len(data); i++ {
			if data[i] != blobData[2048+i] {
				t.Fatalf("byte mismatch at offset %d", 2048+i)
			}
		}
		t.Log("Range 2048-4095: 2048 bytes, content verified")
	})

	// Test 4: Open-ended range (bytes=5000-)
	t.Run("Range_OpenEnd", func(t *testing.T) {
		req, _ := http.NewRequest("GET", blobURL, nil)
		req.Header.Set("Range", "bytes=5000-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 206 {
			t.Fatalf("status=%d, want 206", resp.StatusCode)
		}

		data, _ := io.ReadAll(resp.Body)
		expectedLen := len(blobData) - 5000
		if len(data) != expectedLen {
			t.Fatalf("got %d bytes, want %d", len(data), expectedLen)
		}
		t.Logf("Range 5000-: %d bytes", len(data))
	})

	// Test 5: Suffix range (bytes=-500)
	t.Run("Range_Suffix", func(t *testing.T) {
		req, _ := http.NewRequest("GET", blobURL, nil)
		req.Header.Set("Range", "bytes=-500")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 206 {
			t.Fatalf("status=%d, want 206", resp.StatusCode)
		}

		data, _ := io.ReadAll(resp.Body)
		if len(data) != 500 {
			t.Fatalf("got %d bytes, want 500", len(data))
		}

		// Verify it's the last 500 bytes
		offset := len(blobData) - 500
		for i := 0; i < 500; i++ {
			if data[i] != blobData[offset+i] {
				t.Fatalf("byte mismatch at offset %d", offset+i)
			}
		}
		t.Log("Range -500: 500 bytes (suffix), content verified")
	})

	// Test 6: Invalid range
	t.Run("Range_Invalid", func(t *testing.T) {
		req, _ := http.NewRequest("GET", blobURL, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", len(blobData)+1, len(blobData)+100))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode != 416 {
			t.Errorf("status=%d, want 416 Range Not Satisfiable", resp.StatusCode)
		}
		t.Log("Invalid range: correctly returned 416")
	})

	// Test 7: Full GET still works (no Range header)
	t.Run("FullGET_NoRange", func(t *testing.T) {
		resp, err := http.Get(blobURL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}

		data, _ := io.ReadAll(resp.Body)
		if len(data) != len(blobData) {
			t.Fatalf("got %d bytes, want %d", len(data), len(blobData))
		}

		actualDigest := digest.FromBytes(data)
		if actualDigest != blobDigest {
			t.Fatalf("digest mismatch: got %s, want %s", actualDigest, blobDigest)
		}
		t.Log("Full GET: all bytes, digest verified")
	})
}

// TestParallelBlobFetchE2E tests parallel P2P blob fetching from multiple peers.
func TestParallelBlobFetchE2E(t *testing.T) {
	ctx := context.Background()

	// Create 3 nodes with the same blob
	node1 := startTestNode(t, "parallel-node-1")
	defer node1.stop()
	node2 := startTestNode(t, "parallel-node-2")
	defer node2.stop()
	node3 := startTestNode(t, "parallel-node-3")
	defer node3.stop()

	// Create a 20KB blob (large enough to split into chunks)
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	blobData := make([]byte, 20*1024)
	for i := range blobData {
		blobData[i] = byte((i * 13) % 256) // pseudo-random pattern
	}

	// All nodes have the same blob
	_, blobDigest := node1.store.addImage("library/app:latest", configData, blobData)
	node2.store.addImage("library/app:latest", configData, blobData)
	node3.store.addImage("library/app:latest", configData, blobData)

	node1.initTracker(ctx, t)
	node2.initTracker(ctx, t)
	node3.initTracker(ctx, t)

	blobPath := fmt.Sprintf("/v2/library/app/blobs/%s", blobDigest)

	// Test 1: Parallel fetch from single peer
	t.Run("SinglePeer", func(t *testing.T) {
		balancer := routing.NewBalancer([]string{node1.addr})
		fetcher := proxy.NewParallelFetcher(4*1024, 4) // 4KB chunks

		data, err := fetcher.Fetch(ctx, balancer, blobPath, int64(len(blobData)))
		if err != nil {
			t.Fatal(err)
		}

		if len(data) != len(blobData) {
			t.Fatalf("got %d bytes, want %d", len(data), len(blobData))
		}

		actualDigest := digest.FromBytes(data)
		if actualDigest != blobDigest {
			t.Fatalf("digest mismatch: got %s, want %s", actualDigest, blobDigest)
		}
		t.Logf("Single peer fetch: %d bytes, digest verified", len(data))
	})

	// Test 2: Parallel fetch from multiple peers
	t.Run("MultiplePeers", func(t *testing.T) {
		balancer := routing.NewBalancer([]string{node1.addr, node2.addr, node3.addr})
		fetcher := proxy.NewParallelFetcher(4*1024, 4)

		data, err := fetcher.Fetch(ctx, balancer, blobPath, int64(len(blobData)))
		if err != nil {
			t.Fatal(err)
		}

		if len(data) != len(blobData) {
			t.Fatalf("got %d bytes, want %d", len(data), len(blobData))
		}

		// Verify every byte
		for i := range data {
			if data[i] != blobData[i] {
				t.Fatalf("byte mismatch at offset %d: got %d, want %d", i, data[i], blobData[i])
			}
		}

		actualDigest := digest.FromBytes(data)
		if actualDigest != blobDigest {
			t.Fatalf("digest mismatch: got %s, want %s", actualDigest, blobDigest)
		}
		t.Logf("Multi-peer parallel fetch: %d bytes, all bytes verified, digest correct", len(data))
	})

	// Test 3: Parallel fetch with one peer down
	t.Run("PeerFailover", func(t *testing.T) {
		// Use a dead peer + live peer
		balancer := routing.NewBalancer([]string{"127.0.0.1:1", node1.addr, node2.addr})
		fetcher := proxy.NewParallelFetcher(4*1024, 4)

		data, err := fetcher.Fetch(ctx, balancer, blobPath, int64(len(blobData)))
		if err != nil {
			t.Fatal(err)
		}

		actualDigest := digest.FromBytes(data)
		if actualDigest != blobDigest {
			t.Fatalf("digest mismatch after failover: got %s, want %s", actualDigest, blobDigest)
		}
		t.Log("Peer failover: successfully fetched despite dead peer")
	})

	// Test 4: Small blob (no chunking needed)
	t.Run("SmallBlob_NoChunking", func(t *testing.T) {
		// The configData blob is small (< chunk size)
		configDigest := digest.FromBytes(configData)
		configPath := fmt.Sprintf("/v2/library/app/blobs/%s", configDigest)

		balancer := routing.NewBalancer([]string{node1.addr})
		fetcher := proxy.NewParallelFetcher(proxy.DefaultChunkSize, proxy.DefaultParallelism)

		data, err := fetcher.Fetch(ctx, balancer, configPath, int64(len(configData)))
		if err != nil {
			t.Fatal(err)
		}

		if string(data) != string(configData) {
			t.Fatalf("small blob content mismatch")
		}
		t.Log("Small blob (no chunking): content verified")
	})

	t.Logf("=== Parallel P2P Blob Fetch E2E Report ===")
	t.Logf("Nodes: 3")
	t.Logf("Blob size: %d bytes", len(blobData))
	t.Logf("Chunk size: 4KB")
	t.Logf("Parallelism: 4")
	t.Logf("All tests passed: single peer, multi-peer, failover, small blob")
}

// TestLatencyAwareBalancerE2E tests latency-aware peer selection.
func TestLatencyAwareBalancerE2E(t *testing.T) {
	// Simulate 3 peers with different latencies
	latencies := map[string]float64{
		"fast-peer:5000": 0.001,  // 1ms
		"mid-peer:5000":  0.050,  // 50ms
		"slow-peer:5000": 0.500,  // 500ms
	}

	peers := []string{"fast-peer:5000", "mid-peer:5000", "slow-peer:5000"}

	balancer := routing.NewLatencyBalancer(peers, latencies)

	// Sample 1000 selections and count distribution
	counts := make(map[string]int)
	for i := 0; i < 1000; i++ {
		peer, ok := balancer.Next()
		if !ok {
			t.Fatal("Next() returned false")
		}
		counts[peer]++
	}

	// Fast peer should be selected most often
	t.Run("FastPeerPreferred", func(t *testing.T) {
		if counts["fast-peer:5000"] <= counts["slow-peer:5000"] {
			t.Errorf("fast peer (%d) should be selected more than slow peer (%d)",
				counts["fast-peer:5000"], counts["slow-peer:5000"])
		}
		t.Logf("Selection distribution over 1000 samples:")
		for peer, count := range counts {
			pct := float64(count) / 10.0
			t.Logf("  %s: %d (%.1f%%)", peer, count, pct)
		}
	})

	// All peers should be selected at least sometimes (no starvation)
	t.Run("NoStarvation", func(t *testing.T) {
		for _, peer := range peers {
			if counts[peer] == 0 {
				t.Errorf("peer %s was never selected (starvation)", peer)
			}
		}
	})

	// Test round-robin fallback (no latency data)
	t.Run("RoundRobinFallback", func(t *testing.T) {
		rrBalancer := routing.NewBalancer(peers)
		seen := make(map[string]bool)
		for i := 0; i < len(peers); i++ {
			peer, ok := rrBalancer.Next()
			if !ok {
				t.Fatal("Next() returned false")
			}
			seen[peer] = true
		}
		if len(seen) != len(peers) {
			t.Errorf("round-robin should cycle through all %d peers, saw %d", len(peers), len(seen))
		}
		t.Log("Round-robin fallback: all peers visited in cycle")
	})
}
