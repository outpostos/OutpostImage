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
	"github.com/outpostos/outpostimage/pkg/state"
)

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
