package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/outpostos/outpostimage/pkg/routing"
)

// createTestBlobServer creates a test server that serves a blob with Range support.
func createTestBlobServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		// Parse Range: bytes=start-end
		if !strings.HasPrefix(rangeHeader, "bytes=") {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		spec := strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)

		if start >= int64(len(data)) || end >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}

		chunk := data[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	}))
}

func TestParallelFetcher_FetchSingle(t *testing.T) {
	data := []byte("hello world blob content")
	srv := createTestBlobServer(t, data)
	defer srv.Close()

	// Extract host:port from test server
	addr := strings.TrimPrefix(srv.URL, "http://")
	balancer := routing.NewBalancer([]string{addr})

	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	result, err := pf.Fetch(context.Background(), balancer, "/v2/test/blobs/sha256:abc", int64(len(data)))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if string(result) != string(data) {
		t.Errorf("Fetch() = %q, want %q", string(result), string(data))
	}
}

func TestParallelFetcher_FetchParallel(t *testing.T) {
	// Create a 10KB blob that will be split into chunks
	data := make([]byte, 10*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	srv := createTestBlobServer(t, data)
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	balancer := routing.NewBalancer([]string{addr})

	// Use small chunk size to force parallel fetch
	pf := NewParallelFetcher(2*1024, 4) // 2KB chunks, 4 parallel
	result, err := pf.Fetch(context.Background(), balancer, "/v2/test/blobs/sha256:abc", int64(len(data)))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if len(result) != len(data) {
		t.Fatalf("Fetch() returned %d bytes, want %d", len(result), len(data))
	}

	for i := range data {
		if result[i] != data[i] {
			t.Fatalf("byte mismatch at offset %d: got %d, want %d", i, result[i], data[i])
		}
	}
}

func TestParallelFetcher_MultiplePeers(t *testing.T) {
	data := make([]byte, 8*1024) // 8KB
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Create multiple identical servers
	srv1 := createTestBlobServer(t, data)
	defer srv1.Close()
	srv2 := createTestBlobServer(t, data)
	defer srv2.Close()

	addr1 := strings.TrimPrefix(srv1.URL, "http://")
	addr2 := strings.TrimPrefix(srv2.URL, "http://")
	balancer := routing.NewBalancer([]string{addr1, addr2})

	pf := NewParallelFetcher(2*1024, 4)
	result, err := pf.Fetch(context.Background(), balancer, "/v2/test/blobs/sha256:abc", int64(len(data)))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if len(result) != len(data) {
		t.Fatalf("Fetch() returned %d bytes, want %d", len(result), len(data))
	}
}

func TestParallelFetcher_NoPeers(t *testing.T) {
	balancer := routing.NewBalancer(nil)
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)

	_, err := pf.Fetch(context.Background(), balancer, "/v2/test/blobs/sha256:abc", 1024)
	if err == nil {
		t.Error("Fetch() should fail with no peers")
	}
}

func TestParallelFetcher_CalculateChunks(t *testing.T) {
	pf := NewParallelFetcher(1024, 4) // 1KB chunks

	chunks := pf.calculateChunks(3000)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	// First chunk: 0-1023
	if chunks[0].start != 0 || chunks[0].end != 1023 {
		t.Errorf("chunk 0: %d-%d, want 0-1023", chunks[0].start, chunks[0].end)
	}
	// Second chunk: 1024-2047
	if chunks[1].start != 1024 || chunks[1].end != 2047 {
		t.Errorf("chunk 1: %d-%d, want 1024-2047", chunks[1].start, chunks[1].end)
	}
	// Third chunk: 2048-2999
	if chunks[2].start != 2048 || chunks[2].end != 2999 {
		t.Errorf("chunk 2: %d-%d, want 2048-2999", chunks[2].start, chunks[2].end)
	}
}

// TestNewParallelFetcher_Defaults verifies that zero values for chunkSize and parallelism
// are replaced with DefaultChunkSize and DefaultParallelism respectively.
func TestNewParallelFetcher_Defaults(t *testing.T) {
	pf := NewParallelFetcher(0, 0)
	if pf.chunkSize != DefaultChunkSize {
		t.Errorf("chunkSize = %d, want %d (DefaultChunkSize)", pf.chunkSize, DefaultChunkSize)
	}
	if pf.parallelism != DefaultParallelism {
		t.Errorf("parallelism = %d, want %d (DefaultParallelism)", pf.parallelism, DefaultParallelism)
	}
}

// TestFetchSingle_NonOKStatus covers the "unexpected status" error return in fetchSingle.
func TestFetchSingle_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	peer := strings.TrimPrefix(srv.URL, "http://")
	_, err := pf.fetchSingle(context.Background(), peer, "/v2/test/blobs/sha256:abc")
	if err == nil {
		t.Error("fetchSingle: expected error for non-200 status")
	}
}

// TestFetchChunk_NonOKStatus covers the "unexpected status" error return in fetchChunk
// when the server returns neither 200 nor 206.
func TestFetchChunk_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	peer := strings.TrimPrefix(srv.URL, "http://")
	_, err := pf.fetchChunk(context.Background(), peer, "/v2/test/blobs/sha256:abc", 0, 100)
	if err == nil {
		t.Error("fetchChunk: expected error for non-206/200 status")
	}
}

func TestParallelFetcher_StreamFetch(t *testing.T) {
	data := []byte("stream test blob")
	srv := createTestBlobServer(t, data)
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	balancer := routing.NewBalancer([]string{addr})

	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	w := httptest.NewRecorder()

	err := pf.StreamFetch(context.Background(), w, balancer, "/v2/test/blobs/sha256:abc", int64(len(data)))
	if err != nil {
		t.Fatalf("StreamFetch() error = %v", err)
	}

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != string(data) {
		t.Errorf("body = %q, want %q", w.Body.String(), string(data))
	}
}

// TestFetchSingle_InvalidURL exercises the http.NewRequestWithContext error path
// in fetchSingle when the peer address is malformed.
func TestFetchSingle_InvalidURL(t *testing.T) {
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	_, err := pf.fetchSingle(context.Background(), "bad host]", "/path")
	if err == nil {
		t.Error("expected error for invalid peer URL")
	}
}

// TestFetchChunk_InvalidURL exercises the http.NewRequestWithContext error path
// in fetchChunk when the peer address is malformed.
func TestFetchChunk_InvalidURL(t *testing.T) {
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	_, err := pf.fetchChunk(context.Background(), "bad host]", "/path", 0, 99)
	if err == nil {
		t.Error("expected error for invalid peer URL")
	}
}

// TestFetchSingle_ConnectionRefused exercises the http.Client.Do error path in
// fetchSingle when the peer is unreachable (connection refused).
func TestFetchSingle_ConnectionRefused(t *testing.T) {
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	// Port 1 is always refused.
	_, err := pf.fetchSingle(context.Background(), "127.0.0.1:1", "/v2/test/blobs/sha256:abc")
	if err == nil {
		t.Error("expected error for connection refused in fetchSingle")
	}
}

// TestFetchChunk_ConnectionRefused exercises the http.Client.Do error path in
// fetchChunk when the peer is unreachable (connection refused).
func TestFetchChunk_ConnectionRefused(t *testing.T) {
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	// Port 1 is always refused.
	_, err := pf.fetchChunk(context.Background(), "127.0.0.1:1", "/v2/test/blobs/sha256:abc", 0, 99)
	if err == nil {
		t.Error("expected error for connection refused in fetchChunk")
	}
}

// TestParallelFetcher_AllChunksFail exercises the chunk-failure accumulation and
// reassembly error path: when every chunk attempt fails, Fetch returns an error.
// Uses 2 fail servers so len(peers)>1 forces the chunked code path.
func TestParallelFetcher_AllChunksFail(t *testing.T) {
	// Both servers always return 500 so fetchChunk fails for every chunk attempt.
	makeFail := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	}
	srv1 := makeFail()
	defer srv1.Close()
	srv2 := makeFail()
	defer srv2.Close()

	addr1 := strings.TrimPrefix(srv1.URL, "http://")
	addr2 := strings.TrimPrefix(srv2.URL, "http://")

	// chunkSize=1 → 10 chunks; 2 peers → chunked path (not fetchSingle).
	pf := NewParallelFetcher(1, 4)
	balancer := routing.NewBalancer([]string{addr1, addr2})

	_, err := pf.Fetch(context.Background(), balancer, "/v2/test/blobs/sha256:abc", 10)
	if err == nil {
		t.Error("expected error when all chunk fetches fail")
	}
}

// TestStreamFetch_FetchError exercises the StreamFetch error path when Fetch fails.
func TestStreamFetch_FetchError(t *testing.T) {
	// Use a balancer with no peers so Fetch returns "no peers available".
	balancer := routing.NewBalancer(nil)
	pf := NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	w := httptest.NewRecorder()

	err := pf.StreamFetch(context.Background(), w, balancer, "/v2/test/blobs/sha256:abc", 100)
	if err == nil {
		t.Error("expected error when Fetch fails")
	}
}
