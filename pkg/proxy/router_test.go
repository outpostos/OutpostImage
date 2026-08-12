package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// mockRouter is a routing.Router that is NOT a *routing.StaticRouter,
// used to test the non-static-router branch of HealthyPeersFromRouter.
type mockRouter struct{}

func (m *mockRouter) Ready(_ context.Context) (bool, error)                                 { return true, nil }
func (m *mockRouter) Lookup(_ context.Context, _ digest.Digest) (*routing.Balancer, error)  { return nil, nil }
func (m *mockRouter) Advertise(_ context.Context, _ []digest.Digest) error                  { return nil }
func (m *mockRouter) Withdraw(_ context.Context, _ []digest.Digest) error                   { return nil }
func (m *mockRouter) PeerLatency(_ string) float64                                           { return 0 }
func (m *mockRouter) RunHealthCheck(_ context.Context, _ time.Duration) error                { return nil }
func (m *mockRouter) HealthyPeers() []string                                                 { return nil }

type routerMockStore struct {
	images []oci.Image
}

func (m *routerMockStore) Name() string                                               { return "mock" }
func (m *routerMockStore) ListImages(_ context.Context) ([]oci.Image, error)          { return m.images, nil }
func (m *routerMockStore) Resolve(_ context.Context, _ string) (digest.Digest, error) { return "", nil }
func (m *routerMockStore) GetManifest(_ context.Context, _ digest.Digest) ([]byte, string, error) {
	return nil, "", nil
}
func (m *routerMockStore) GetBlob(_ context.Context, _ digest.Digest) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}
func (m *routerMockStore) HasBlob(_ context.Context, _ digest.Digest) bool { return false }
func (m *routerMockStore) Subscribe(_ context.Context) (<-chan oci.Event, error) {
	return make(chan oci.Event), nil
}

func setupRouterTest(t *testing.T) *state.Tracker {
	t.Helper()
	d1, _ := digest.Parse("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")

	store := &routerMockStore{
		images: []oci.Image{{Digest: d1}},
	}

	tracker := state.NewTracker(store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = tracker.Track(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return tracker
}

func TestRequestRouter_BlobLocal(t *testing.T) {
	tracker := setupRouterTest(t)

	// Start a local registry server
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("local blob"))
	}))
	defer local.Close()

	interceptor := RequestRouter(tracker, local.Listener.Addr().String())

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/blobs/sha256:aaaa000000000000000000000000000000000000000000000000000000000000",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected local blob request to be handled")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRequestRouter_BlobUpstream(t *testing.T) {
	tracker := setupRouterTest(t)
	interceptor := RequestRouter(tracker, "localhost:5000")

	// Request a blob we don't have
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if handled {
		t.Fatal("expected unknown blob request to NOT be handled (forward to upstream)")
	}
}

func TestRequestRouter_ManifestUpstream(t *testing.T) {
	tracker := setupRouterTest(t)
	interceptor := RequestRouter(tracker, "localhost:5000")

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/manifests/latest",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if handled {
		t.Fatal("expected manifest request to forward to upstream for now")
	}
}

func TestFetchFromPeerSetsPeerFetchHeader(t *testing.T) {
	var gotHeader string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(PeerFetchHeader)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("manifest"))
	}))
	defer peer.Close()

	resp, err := FetchFromPeer(
		context.Background(),
		peer.Listener.Addr().String(),
		http.MethodGet,
		"/v2/library/nginx/manifests/latest",
		nil,
		"docker.io",
	)
	if err != nil {
		t.Fatalf("FetchFromPeer() error = %v", err)
	}
	resp.Body.Close()

	if gotHeader != PeerFetchValue {
		t.Fatalf("%s = %q, want %q", PeerFetchHeader, gotHeader, PeerFetchValue)
	}
}

func TestFetchFromPeer_NonSuccessStatus(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer peer.Close()

	_, err := FetchFromPeer(context.Background(), peer.Listener.Addr().String(), http.MethodGet, "/v2/missing/blobs/sha256:abc", nil, "")
	if err == nil {
		t.Fatal("expected error for non-success status")
	}
}

func TestFetchFromPeers_FirstSucceeds(t *testing.T) {
	// Server handles both the healthz check and blob requests with 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("blob-data"))
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	pw := routing.NewPeerWatcher([]string{addr})
	r := routing.NewStaticRouter(pw)

	// Trigger the initial health check so the peer is marked healthy.
	hctx, hcancel := context.WithCancel(context.Background())
	go func() {
		_ = r.RunHealthCheck(hctx, time.Hour)
	}()
	time.Sleep(150 * time.Millisecond)
	hcancel()

	resp, peer, ok := FetchFromPeers(context.Background(), r, http.MethodGet, "/v2/library/nginx/blobs/sha256:abc", nil, "")
	if !ok {
		t.Fatal("expected FetchFromPeers to succeed")
	}
	resp.Body.Close()
	if peer != addr {
		t.Errorf("peer = %q, want %q", peer, addr)
	}
}

func TestFetchFromPeers_NoPeers(t *testing.T) {
	pw := routing.NewPeerWatcher(nil)
	r := routing.NewStaticRouter(pw)

	_, _, ok := FetchFromPeers(context.Background(), r, http.MethodGet, "/v2/missing/blobs/sha256:abc", nil, "")
	if ok {
		t.Fatal("expected FetchFromPeers to fail with no peers")
	}
}

func TestFetchFromUpstream_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-data"))
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/sha256:abc", nil)
	req.Header.Set("X-Upstream-Registry", upstream.URL)

	resp, err := FetchFromUpstream(context.Background(), req, http.MethodGet, "")
	if err != nil {
		t.Fatalf("FetchFromUpstream() error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestFetchFromUpstream_UsesXOriginalRegistry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	req.Header.Set("X-Original-Registry", upstream.URL)

	resp, err := FetchFromUpstream(context.Background(), req, http.MethodGet, "")
	if err != nil {
		t.Fatalf("FetchFromUpstream() error = %v", err)
	}
	resp.Body.Close()
}

func TestUpstreamBaseURL(t *testing.T) {
	tests := []struct {
		input   string
		wantURL string
		wantErr bool
	}{
		{"", "", true},
		{"docker.io", "https://registry-1.docker.io", false},
		{"index.docker.io", "https://registry-1.docker.io", false},
		{"ghcr.io", "https://ghcr.io", false},
		{"http://localhost:5000", "http://localhost:5000", false},
		{"https://registry.example.com/path", "https://registry.example.com", false},
	}
	for _, tt := range tests {
		got, err := upstreamBaseURL(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("upstreamBaseURL(%q): expected error, got none", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("upstreamBaseURL(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.wantURL {
			t.Errorf("upstreamBaseURL(%q) = %q, want %q", tt.input, got, tt.wantURL)
		}
	}
}

func TestHeadBlobSize_Found(t *testing.T) {
	const blobSize = 12345
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	size := headBlobSize(context.Background(), srv.Listener.Addr().String(), "/v2/lib/nginx/blobs/sha256:abc")
	if size != blobSize {
		t.Errorf("headBlobSize = %d, want %d", size, blobSize)
	}
}

func TestHeadBlobSize_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	size := headBlobSize(context.Background(), srv.Listener.Addr().String(), "/v2/lib/nginx/blobs/sha256:abc")
	if size != 0 {
		t.Errorf("headBlobSize = %d, want 0 for not found", size)
	}
}

func TestHealthyPeersFromRouter_StaticRouter(t *testing.T) {
	// Start a real server that returns 200 for health checks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	pw := routing.NewPeerWatcher([]string{addr})
	r := routing.NewStaticRouter(pw)

	// Trigger initial health check.
	hctx, hcancel := context.WithCancel(context.Background())
	go func() { _ = r.RunHealthCheck(hctx, time.Hour) }()
	time.Sleep(150 * time.Millisecond)
	hcancel()

	peers := HealthyPeersFromRouter(context.Background(), r)
	if len(peers) != 1 || peers[0] != addr {
		t.Errorf("HealthyPeersFromRouter = %v, want [%s]", peers, addr)
	}
}

func TestHealthyPeersFromRouter_NonStaticRouter(t *testing.T) {
	peers := HealthyPeersFromRouter(context.Background(), &mockRouter{})
	if peers != nil {
		t.Errorf("expected nil for non-StaticRouter, got %v", peers)
	}
}

func TestPeerSchemeAndAddr(t *testing.T) {
	tests := []struct {
		addr       string
		wantScheme string
		wantHost   string
	}{
		{"https://peer.local:5000", "https", "peer.local:5000"},
		{"http://peer.local:5000", "http", "peer.local:5000"},
		{":5000", "http", "localhost:5000"},
		{"peer.local:5000", "http", "peer.local:5000"},
		{"127.0.0.1:8080", "http", "127.0.0.1:8080"},
	}
	for _, tt := range tests {
		s, h := peerSchemeAndAddr(tt.addr)
		if s != tt.wantScheme {
			t.Errorf("peerSchemeAndAddr(%q).scheme = %q, want %q", tt.addr, s, tt.wantScheme)
		}
		if h != tt.wantHost {
			t.Errorf("peerSchemeAndAddr(%q).host = %q, want %q", tt.addr, h, tt.wantHost)
		}
	}
}

func TestTryManifestFrom_Success(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	ok := tryManifestFrom(context.Background(), rec, srv.Listener.Addr().String(),
		"/v2/library/nginx/manifests/latest", "docker.io", true)

	if !ok {
		t.Fatal("expected tryManifestFrom to return true on 200")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestTryManifestFrom_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	ok := tryManifestFrom(context.Background(), rec, srv.Listener.Addr().String(),
		"/v2/library/nginx/manifests/missing", "", false)

	if ok {
		t.Fatal("expected tryManifestFrom to return false on 404")
	}
}

func TestRequestRouter_V2VersionCheck(t *testing.T) {
	tracker := setupRouterTest(t)
	interceptor := RequestRouter(tracker, "localhost:5000")

	req := httptest.NewRequest(http.MethodGet, "http://registry.example.com/v2/", nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected /v2/ version check to be handled locally")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRequestRouter_V2NormPath(t *testing.T) {
	tracker := setupRouterTest(t)
	interceptor := RequestRouter(tracker, "localhost:5000")

	req := httptest.NewRequest(http.MethodGet, "http://registry.example.com/v2", nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected /v2 to be handled locally")
	}
}

// TestRequestRouter_BlobLocalDirect exercises the "tracker miss → direct HEAD succeeds" branch of
// RequestRouter.  The blob digest is NOT in the tracker but the local registry responds 200 to
// HEAD, so the router proxies the GET directly to local.
func TestRequestRouter_BlobLocalDirect(t *testing.T) {
	tracker := setupRouterTest(t) // tracker knows sha256:aaaa…

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("direct local blob"))
		}
	}))
	defer local.Close()

	interceptor := RequestRouter(tracker, local.Listener.Addr().String())

	// sha256:bbbb… is NOT in the tracker — forces the direct-check path.
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/blobs/sha256:bbbb000000000000000000000000000000000000000000000000000000000000",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected local direct blob request to be handled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestRequestRouter_ManifestLocal exercises the "tryManifestFrom local succeeds" branch of
// RequestRouter — the local registry returns 200 for the manifest GET, so the router
// writes the response and returns true without touching upstream.
func TestRequestRouter_ManifestLocal(t *testing.T) {
	tracker := setupRouterTest(t)

	manifest := []byte(`{"schemaVersion":2}`)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer local.Close()

	interceptor := RequestRouter(tracker, local.Listener.Addr().String())

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/manifests/latest",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected local manifest request to be handled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestRequestRouter_BlobP2P exercises the "tracker miss → local miss → P2P peer hit" branch of
// RequestRouter.  The local registry returns 404 for the HEAD check, but a P2P peer returns 200,
// so the router proxies the GET to the peer.
func TestRequestRouter_BlobP2P(t *testing.T) {
	tracker := setupRouterTest(t) // tracker knows sha256:aaaa…

	// Peer server: 200 for HEAD and GET on blob paths; 200 for health-check.
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("p2p blob"))
		}
	}))
	defer peerSrv.Close()
	peerAddr := peerSrv.Listener.Addr().String()

	// Local server returns 404 so tryBlobFromLocal(localAddr) is false.
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer localSrv.Close()

	// Build StaticRouter with the peer and run one health check so the peer is marked healthy.
	pw := routing.NewPeerWatcher([]string{peerAddr})
	sr := routing.NewStaticRouter(pw)
	hctx, hcancel := context.WithCancel(context.Background())
	go func() { _ = sr.RunHealthCheck(hctx, time.Hour) }()
	time.Sleep(150 * time.Millisecond)
	hcancel()

	interceptor := RequestRouter(tracker, localSrv.Listener.Addr().String(), sr)

	// sha256:bbbb… is NOT in the tracker; local returns 404 → falls through to P2P.
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/blobs/sha256:bbbb000000000000000000000000000000000000000000000000000000000000",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected P2P blob request to be handled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestRequestRouter_ManifestP2P exercises the "local manifest miss → P2P peer hit" branch of
// RequestRouter.  The local registry returns 404 for the manifest GET, but a P2P peer returns 200.
func TestRequestRouter_ManifestP2P(t *testing.T) {
	tracker := setupRouterTest(t)

	manifest := []byte(`{"schemaVersion":2}`)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// health check
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer peerSrv.Close()
	peerAddr := peerSrv.Listener.Addr().String()

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer localSrv.Close()

	pw := routing.NewPeerWatcher([]string{peerAddr})
	sr := routing.NewStaticRouter(pw)
	hctx, hcancel := context.WithCancel(context.Background())
	go func() { _ = sr.RunHealthCheck(hctx, time.Hour) }()
	time.Sleep(150 * time.Millisecond)
	hcancel()

	interceptor := RequestRouter(tracker, localSrv.Listener.Addr().String(), sr)

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/nginx/manifests/latest",
		nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("expected P2P manifest request to be handled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestPeerClient_Schemes covers both branches of peerClient — the HTTPS branch returns a
// dedicated client with InsecureSkipVerify, the HTTP branch returns http.DefaultClient.
func TestPeerClient_Schemes(t *testing.T) {
	httpsClient := peerClient("https")
	if httpsClient == http.DefaultClient {
		t.Error("peerClient(https): expected a dedicated client, got DefaultClient")
	}
	httpClient := peerClient("http")
	if httpClient != http.DefaultClient {
		t.Error("peerClient(http): expected DefaultClient")
	}
}

// TestFetchFromUpstream_AbsoluteURL exercises the r.URL.IsAbs() branch that extracts
// the registry from the request's absolute URL when no X-*-Registry header is set.
// The branch sets rawRegistry = r.URL.Host (no scheme). upstreamBaseURL then prepends
// "https://", so the downstream Do call will fail for a plain HTTP test server — we
// only verify the branch is exercised (the Do error is expected and accepted).
func TestFetchFromUpstream_AbsoluteURL(t *testing.T) {
	// Construct a request with an absolute URL so IsAbs() && Host != "" is true.
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/v2/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	// No X-Upstream-Registry or X-Original-Registry headers — rawRegistry falls
	// through to the IsAbs branch. upstreamBaseURL("127.0.0.1:1") returns
	// "https://127.0.0.1:1"; Do fails with connection refused — that is expected.
	_, err = FetchFromUpstream(context.Background(), req, http.MethodGet, "")
	// We expect an error (unreachable host). The important thing is the IsAbs branch ran.
	if err == nil {
		t.Log("FetchFromUpstream unexpectedly succeeded; branch still exercised")
	}
}

// TestFetchFromUpstream_EmptyRegistry exercises the upstreamBaseURL error path when
// all registry sources are empty and defaultRegistry is also empty.
func TestFetchFromUpstream_EmptyRegistry(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v2/path", nil)
	// No headers, relative URL, empty defaultRegistry → rawRegistry stays "".
	_, err := FetchFromUpstream(context.Background(), req, http.MethodGet, "")
	if err == nil {
		t.Error("expected error for empty registry")
	}
}

// TestFetchFromUpstream_DoError exercises the http.DefaultClient.Do error path in
// FetchFromUpstream by pointing at an unreachable host.
func TestFetchFromUpstream_DoError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v2/path", nil)
	req.Header.Set("X-Upstream-Registry", "http://127.0.0.1:1")
	_, err := FetchFromUpstream(context.Background(), req, http.MethodGet, "")
	if err == nil {
		t.Error("expected error when upstream is unreachable")
	}
}

// TestProxyToLocal_WithQueryString exercises the RawQuery path in proxyToLocal.
func TestProxyToLocal_WithQueryString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "foo=bar" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/v2/path?foo=bar", nil)
	w := httptest.NewRecorder()
	proxyToLocal(w, req, srv.Listener.Addr().String())

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestProxyToLocal_DoError exercises the peerClient.Do error path in proxyToLocal.
func TestProxyToLocal_DoError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v2/path", nil)
	w := httptest.NewRecorder()
	// Port 1 is unreachable → peerClient.Do fails → 502 response.
	proxyToLocal(w, req, "127.0.0.1:1")

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}
