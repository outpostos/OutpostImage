package registry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/state"
)

type mockStore struct {
	images []oci.Image
}

func (m *mockStore) Name() string                                                        { return "mock" }
func (m *mockStore) ListImages(_ context.Context) ([]oci.Image, error)                   { return m.images, nil }
func (m *mockStore) Resolve(_ context.Context, _ string) (digest.Digest, error)          { return "", nil }
func (m *mockStore) GetManifest(_ context.Context, _ digest.Digest) ([]byte, string, error) { return nil, "", nil }
func (m *mockStore) GetBlob(_ context.Context, _ digest.Digest) (io.ReadCloser, int64, error) { return nil, 0, nil }
func (m *mockStore) HasBlob(_ context.Context, _ digest.Digest) bool                    { return false }
func (m *mockStore) Subscribe(_ context.Context) (<-chan oci.Event, error) {
	return make(chan oci.Event), nil
}

func setupTestHandler(t *testing.T) (*DigestHandler, *http.ServeMux) {
	t.Helper()

	d1, _ := digest.Parse("sha256:aaaa000000000000000000000000000000000000000000000000000000000000")
	d2, _ := digest.Parse("sha256:bbbb000000000000000000000000000000000000000000000000000000000000")

	store := &mockStore{
		images: []oci.Image{
			{Digest: d1, RepoTags: []string{"nginx:latest"}},
			{Digest: d2, RepoTags: []string{"redis:latest"}},
		},
	}

	tracker := state.NewTracker(store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tracker.Track(ctx)
	time.Sleep(100 * time.Millisecond)

	handler := NewDigestHandler(tracker)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	return handler, mux
}

func TestHandleListDigests(t *testing.T) {
	_, mux := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/digests", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	count := int(body["count"].(float64))
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	digests := body["digests"].([]any)
	if len(digests) != 2 {
		t.Fatalf("got %d digests, want 2", len(digests))
	}
}

func TestHandleGetDigest_Exists(t *testing.T) {
	_, mux := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/digests/sha256:aaaa000000000000000000000000000000000000000000000000000000000000", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["exists"] != true {
		t.Fatalf("exists = %v, want true", body["exists"])
	}
}

func TestHandleGetDigest_NotFound(t *testing.T) {
	_, mux := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/digests/sha256:0000000000000000000000000000000000000000000000000000000000000000", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["exists"] != false {
		t.Fatalf("exists = %v, want false", body["exists"])
	}
}

func TestHandleGetDigest_InvalidFormat(t *testing.T) {
	_, mux := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/digests/invalid!!digest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
