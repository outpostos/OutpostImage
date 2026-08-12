package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/routing"
)

// ociMockStore implements oci.Store for OCI handler testing.
type ociMockStore struct {
	manifests map[digest.Digest][]byte
	blobs     map[digest.Digest][]byte
	tags      map[string]digest.Digest
}

func (m *ociMockStore) Name() string { return "mock" }

func (m *ociMockStore) ListImages(_ context.Context) ([]oci.Image, error) {
	return nil, nil
}

func (m *ociMockStore) Resolve(_ context.Context, ref string) (digest.Digest, error) {
	d, ok := m.tags[ref]
	if !ok {
		return "", fmt.Errorf("not found: %s", ref)
	}
	return d, nil
}

func (m *ociMockStore) GetManifest(_ context.Context, dgst digest.Digest) ([]byte, string, error) {
	data, ok := m.manifests[dgst]
	if !ok {
		return nil, "", fmt.Errorf("not found")
	}
	return data, "application/vnd.docker.container.image.v1+json", nil
}

func (m *ociMockStore) GetBlob(_ context.Context, dgst digest.Digest) (io.ReadCloser, int64, error) {
	data, ok := m.blobs[dgst]
	if !ok {
		return nil, 0, fmt.Errorf("not found")
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (m *ociMockStore) HasBlob(_ context.Context, dgst digest.Digest) bool {
	_, ok := m.blobs[dgst]
	return ok
}

func (m *ociMockStore) Subscribe(_ context.Context) (<-chan oci.Event, error) {
	return make(chan oci.Event), nil
}

func newOCITestMux() *http.ServeMux {
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := digest.FromBytes(configData)

	blobData := []byte("fake blob content")
	blobDigest := digest.FromBytes(blobData)

	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{
			configDigest: configData,
		},
		blobs: map[digest.Digest][]byte{
			configDigest: configData,
			blobDigest:   blobData,
		},
		tags: map[string]digest.Digest{
			"library/nginx:latest": configDigest,
		},
	}

	handler := NewOCIHandler(store, WithUpstreamFallback(false), WithMetrics(nil))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux
}

func newOCITestStore() (*ociMockStore, digest.Digest, digest.Digest, []byte, []byte) {
	manifestData := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	manifestDigest := digest.FromBytes(manifestData)
	blobData := []byte("remote blob content")
	blobDigest := digest.FromBytes(blobData)

	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{
			manifestDigest: manifestData,
		},
		blobs: map[digest.Digest][]byte{
			manifestDigest: manifestData,
			blobDigest:     blobData,
		},
		tags: map[string]digest.Digest{
			"library/remote:latest": manifestDigest,
		},
	}

	return store, manifestDigest, blobDigest, manifestData, blobData
}

func newEmptyOCITestStore() *ociMockStore {
	return &ociMockStore{
		manifests: map[digest.Digest][]byte{},
		blobs:     map[digest.Digest][]byte{},
		tags:      map[string]digest.Digest{},
	}
}

func newRegistryTestServer(store oci.Store, opts ...Option) *httptest.Server {
	mux := http.NewServeMux()
	handler := NewOCIHandler(store, opts...)
	handler.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func newRegistryFallbackMux(store oci.Store, opts ...Option) *http.ServeMux {
	mux := http.NewServeMux()
	handler := NewOCIHandler(store, opts...)
	handler.RegisterRoutes(mux)
	return mux
}

func TestHandleVersionCheck(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	apiVersion := rec.Header().Get("Docker-Distribution-API-Version")
	if apiVersion != "registry/2.0" {
		t.Fatalf("Docker-Distribution-API-Version = %q, want %q", apiVersion, "registry/2.0")
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if len(body) != 0 {
		t.Fatalf("expected empty JSON object, got %v", body)
	}
}

func TestHandleGetManifest_ByTag(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		t.Fatal("missing Content-Type header")
	}

	dcd := rec.Header().Get("Docker-Content-Digest")
	if dcd == "" {
		t.Fatal("missing Docker-Content-Digest header")
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestHandleGetManifest_NotFound(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nonexistent/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	errors, ok := body["errors"].([]any)
	if !ok || len(errors) == 0 {
		t.Fatal("expected OCI error response with errors array")
	}
	errObj := errors[0].(map[string]any)
	if errObj["code"] != "MANIFEST_UNKNOWN" {
		t.Fatalf("error code = %v, want MANIFEST_UNKNOWN", errObj["code"])
	}
}

func TestHandleHeadBlob(t *testing.T) {
	mux := newOCITestMux()

	blobData := []byte("fake blob content")
	blobDigest := digest.FromBytes(blobData)

	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/blobs/"+blobDigest.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	cl := rec.Header().Get("Content-Length")
	if cl == "" {
		t.Fatal("missing Content-Length header")
	}

	dcd := rec.Header().Get("Docker-Content-Digest")
	if dcd != blobDigest.String() {
		t.Fatalf("Docker-Content-Digest = %q, want %q", dcd, blobDigest.String())
	}
}

func TestHandleGetBlob(t *testing.T) {
	mux := newOCITestMux()

	blobData := []byte("fake blob content")
	blobDigest := digest.FromBytes(blobData)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/"+blobDigest.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.Bytes()
	if !bytes.Equal(body, blobData) {
		t.Fatalf("body = %q, want %q", body, blobData)
	}
}

func TestHandleGetBlob_NotFound(t *testing.T) {
	mux := newOCITestMux()

	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/"+fakeDigest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	errors := body["errors"].([]any)
	errObj := errors[0].(map[string]any)
	if errObj["code"] != "BLOB_UNKNOWN" {
		t.Fatalf("error code = %v, want BLOB_UNKNOWN", errObj["code"])
	}
}

func TestRegistryFallback_LocalMissP2PHit(t *testing.T) {
	peerStore, manifestDigest, blobDigest, manifestData, blobData := newOCITestStore()
	peer := newRegistryTestServer(peerStore)
	defer peer.Close()

	pw := routing.NewPeerWatcher([]string{peer.Listener.Addr().String()})
	router := routing.NewStaticRouter(pw)
	mux := newRegistryFallbackMux(newEmptyOCITestStore(), WithRouter(router), WithUpstreamFallback(false), WithMetrics(nil))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/remote/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), manifestData) {
		t.Fatalf("manifest body = %q, want %q", rec.Body.Bytes(), manifestData)
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got != manifestDigest.String() {
		t.Fatalf("manifest digest = %q, want %q", got, manifestDigest)
	}

	req = httptest.NewRequest(http.MethodGet, "/v2/library/remote/blobs/"+blobDigest.String(), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), blobData) {
		t.Fatalf("blob body = %q, want %q", rec.Body.Bytes(), blobData)
	}
}

func TestRegistryFallback_P2PMissUpstreamHit(t *testing.T) {
	upstreamStore, manifestDigest, blobDigest, manifestData, blobData := newOCITestStore()
	upstream := newRegistryTestServer(upstreamStore)
	defer upstream.Close()

	pw := routing.NewPeerWatcher([]string{"127.0.0.1:1"})
	router := routing.NewStaticRouter(pw)
	mux := newRegistryFallbackMux(
		newEmptyOCITestStore(),
		WithRouter(router),
		WithUpstreamRegistry(upstream.URL),
		WithMetrics(nil),
	)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/remote/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), manifestData) {
		t.Fatalf("manifest body = %q, want %q", rec.Body.Bytes(), manifestData)
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got != manifestDigest.String() {
		t.Fatalf("manifest digest = %q, want %q", got, manifestDigest)
	}

	req = httptest.NewRequest(http.MethodGet, "/v2/library/remote/blobs/"+blobDigest.String(), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), blobData) {
		t.Fatalf("blob body = %q, want %q", rec.Body.Bytes(), blobData)
	}
}

func TestRegistryFallback_UpstreamDisabledReturns404(t *testing.T) {
	pw := routing.NewPeerWatcher([]string{"127.0.0.1:1"})
	router := routing.NewStaticRouter(pw)
	mux := newRegistryFallbackMux(newEmptyOCITestStore(), WithRouter(router), WithUpstreamFallback(false), WithMetrics(nil))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/remote/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("manifest status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req = httptest.NewRequest(http.MethodGet, "/v2/library/remote/blobs/"+fakeDigest, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("blob status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestRegistryPeerFetchMissDoesNotRoute(t *testing.T) {
	peerHits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer peer.Close()

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer upstream.Close()

	pw := routing.NewPeerWatcher([]string{peer.Listener.Addr().String()})
	router := routing.NewStaticRouter(pw)
	mux := newRegistryFallbackMux(
		newEmptyOCITestStore(),
		WithRouter(router),
		WithUpstreamRegistry(upstream.URL),
		WithMetrics(nil),
	)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/missing/manifests/latest", nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("manifest status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if peerHits != 0 {
		t.Fatalf("peer hits = %d, want 0", peerHits)
	}
	if upstreamHits != 0 {
		t.Fatalf("upstream hits = %d, want 0", upstreamHits)
	}

	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req = httptest.NewRequest(http.MethodGet, "/v2/library/missing/blobs/"+fakeDigest, nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("blob status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if peerHits != 0 {
		t.Fatalf("peer hits after blob = %d, want 0", peerHits)
	}
	if upstreamHits != 0 {
		t.Fatalf("upstream hits after blob = %d, want 0", upstreamHits)
	}
}

func TestRegistryFallback_HEADAndGETConsistent(t *testing.T) {
	peerStore, manifestDigest, blobDigest, _, blobData := newOCITestStore()
	peer := newRegistryTestServer(peerStore)
	defer peer.Close()

	pw := routing.NewPeerWatcher([]string{peer.Listener.Addr().String()})
	router := routing.NewStaticRouter(pw)
	mux := newRegistryFallbackMux(newEmptyOCITestStore(), WithRouter(router), WithUpstreamFallback(false), WithMetrics(nil))

	manifestPath := "/v2/library/remote/manifests/latest"
	headReq := httptest.NewRequest(http.MethodHead, manifestPath, nil)
	headRec := httptest.NewRecorder()
	mux.ServeHTTP(headRec, headReq)

	getReq := httptest.NewRequest(http.MethodGet, manifestPath, nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	if headRec.Code != http.StatusOK || getRec.Code != http.StatusOK {
		t.Fatalf("manifest statuses HEAD=%d GET=%d, want both 200", headRec.Code, getRec.Code)
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD manifest body length = %d, want 0", headRec.Body.Len())
	}
	for _, name := range []string{"Content-Length", "Docker-Content-Digest", "Docker-Distribution-API-Version"} {
		if headRec.Header().Get(name) != getRec.Header().Get(name) {
			t.Fatalf("manifest %s HEAD=%q GET=%q", name, headRec.Header().Get(name), getRec.Header().Get(name))
		}
	}
	if getRec.Header().Get("Docker-Content-Digest") != manifestDigest.String() {
		t.Fatalf("manifest digest = %q, want %q", getRec.Header().Get("Docker-Content-Digest"), manifestDigest)
	}

	blobPath := "/v2/library/remote/blobs/" + blobDigest.String()
	headReq = httptest.NewRequest(http.MethodHead, blobPath, nil)
	headRec = httptest.NewRecorder()
	mux.ServeHTTP(headRec, headReq)

	getReq = httptest.NewRequest(http.MethodGet, blobPath, nil)
	getRec = httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	if headRec.Code != http.StatusOK || getRec.Code != http.StatusOK {
		t.Fatalf("blob statuses HEAD=%d GET=%d, want both 200", headRec.Code, getRec.Code)
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD blob body length = %d, want 0", headRec.Body.Len())
	}
	if !bytes.Equal(getRec.Body.Bytes(), blobData) {
		t.Fatalf("blob body = %q, want %q", getRec.Body.Bytes(), blobData)
	}
	for _, name := range []string{"Content-Length", "Docker-Content-Digest"} {
		if headRec.Header().Get(name) != getRec.Header().Get(name) {
			t.Fatalf("blob %s HEAD=%q GET=%q", name, headRec.Header().Get(name), getRec.Header().Get(name))
		}
	}
}

func TestOCIErrorFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOCIError(rec, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown", "detail")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)

	errors, ok := body["errors"].([]any)
	if !ok {
		t.Fatal("missing errors array")
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}

	errObj := errors[0].(map[string]any)
	if errObj["code"] != "MANIFEST_UNKNOWN" {
		t.Fatalf("code = %v, want MANIFEST_UNKNOWN", errObj["code"])
	}
	if errObj["message"] != "manifest unknown" {
		t.Fatalf("message = %v, want 'manifest unknown'", errObj["message"])
	}
}

// --- handleGetManifest by-digest paths ---

func TestHandleGetManifest_ByDigest_StoreFetch(t *testing.T) {
	mux := newOCITestMux()

	// The mock store has configData at configDigest — fetch by digest directly.
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := digest.FromBytes(configData)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/"+configDigest.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got == "" {
		t.Error("missing Docker-Content-Digest header")
	}
}

func TestHandleGetManifest_ByDigest_CacheHit(t *testing.T) {
	mux := newOCITestMux()

	// Step 1: tag-based GET populates the manifest cache.
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tag GET status = %d", rec.Code)
	}
	contentDigest := rec.Header().Get("Docker-Content-Digest")
	if contentDigest == "" {
		t.Fatal("missing Docker-Content-Digest from tag GET")
	}

	// Step 2: digest-based GET hits the manifest cache.
	req = httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/"+contentDigest, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("digest GET (cache hit) status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Docker-Content-Digest") != contentDigest {
		t.Errorf("digest = %q, want %q", rec.Header().Get("Docker-Content-Digest"), contentDigest)
	}
}

func TestHandleGetManifest_ByDigest_HEAD_CacheHit(t *testing.T) {
	mux := newOCITestMux()

	// Populate cache via GET.
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	contentDigest := rec.Header().Get("Docker-Content-Digest")

	// HEAD by digest (cache hit) should return no body.
	req = httptest.NewRequest(http.MethodHead, "/v2/library/nginx/manifests/"+contentDigest, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD digest (cache hit) status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD should return no body, got %d bytes", rec.Body.Len())
	}
}

func TestHandleGetManifest_ByDigest_Invalid(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/sha256:invalid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGetManifest_ByDigest_NotFound(t *testing.T) {
	mux := newOCITestMux()

	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/"+fakeDigest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- handleGetBlob range/error paths ---

func TestHandleGetBlob_WithRange(t *testing.T) {
	mux := newOCITestMux()

	blobData := []byte("fake blob content")
	blobDigest := digest.FromBytes(blobData)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/"+blobDigest.String(), nil)
	req.Header.Set("Range", "bytes=0-3") // "fake"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if string(rec.Body.Bytes()) != "fake" {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), "fake")
	}
	if rec.Header().Get("Content-Range") == "" {
		t.Error("missing Content-Range header")
	}
}

func TestHandleGetBlob_InvalidRange(t *testing.T) {
	mux := newOCITestMux()

	blobData := []byte("fake blob content") // 17 bytes
	blobDigest := digest.FromBytes(blobData)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/"+blobDigest.String(), nil)
	req.Header.Set("Range", "bytes=100-200") // start >= size → invalid
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rec.Code)
	}
	if rec.Header().Get("Content-Range") == "" {
		t.Error("missing Content-Range header on 416")
	}
}

func TestHandleGetBlob_InvalidDigest(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/sha256:invalid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// --- handleHeadBlob error paths ---

func TestHandleHeadBlob_InvalidDigest(t *testing.T) {
	mux := newOCITestMux()

	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/blobs/sha256:invalid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// --- originalRegistryHost paths ---

func TestOriginalRegistryHost_Headers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	req.Header.Set("X-Original-Registry", "original.registry.io")
	if got := originalRegistryHost(req); got != "original.registry.io" {
		t.Errorf("X-Original-Registry: got %q, want %q", got, "original.registry.io")
	}

	req = httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	req.Header.Set("X-Upstream-Registry", "upstream.registry.io")
	if got := originalRegistryHost(req); got != "upstream.registry.io" {
		t.Errorf("X-Upstream-Registry: got %q, want %q", got, "upstream.registry.io")
	}
}

func TestOriginalRegistryHost_AbsoluteURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://abs.registry.io/v2/library/nginx/manifests/latest", nil)
	// httptest.NewRequest parses the URL; set scheme and host explicitly for IsAbs().
	req.URL.Scheme = "https"
	req.URL.Host = "abs.registry.io"
	if got := originalRegistryHost(req); got != "abs.registry.io" {
		t.Errorf("absolute URL: got %q, want %q", got, "abs.registry.io")
	}
}

func TestOriginalRegistryHost_HostFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	req.Host = "fallback.registry.io"
	if got := originalRegistryHost(req); got != "fallback.registry.io" {
		t.Errorf("Host fallback: got %q, want %q", got, "fallback.registry.io")
	}
}

func TestParseRangeHeader(t *testing.T) {
	tests := []struct {
		header    string
		totalSize int64
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		// Valid ranges
		{"bytes=0-499", 1000, 0, 499, true},
		{"bytes=500-999", 1000, 500, 999, true},
		{"bytes=0-0", 1000, 0, 0, true},
		// Open-ended range
		{"bytes=500-", 1000, 500, 999, true},
		// Suffix range
		{"bytes=-200", 1000, 800, 999, true},
		// End beyond total: clamped
		{"bytes=0-9999", 1000, 0, 999, true},
		// Suffix larger than total: start clamped to 0
		{"bytes=-9999", 100, 0, 99, true},
		// Invalid: no bytes= prefix
		{"chunks=0-499", 1000, 0, 0, false},
		// Invalid: start >= totalSize
		{"bytes=1000-1999", 1000, 0, 0, false},
		// Invalid: bad start
		{"bytes=abc-499", 1000, 0, 0, false},
		// Invalid: end < start
		{"bytes=500-100", 1000, 0, 0, false},
		// Invalid: suffix zero
		{"bytes=-0", 1000, 0, 0, false},
		// Invalid: suffix negative
		{"bytes=-abc", 1000, 0, 0, false},
	}
	for _, tt := range tests {
		start, end, ok := parseRangeHeader(tt.header, tt.totalSize)
		if ok != tt.wantOK {
			t.Errorf("parseRangeHeader(%q, %d) ok=%v, want %v", tt.header, tt.totalSize, ok, tt.wantOK)
			continue
		}
		if ok && (start != tt.wantStart || end != tt.wantEnd) {
			t.Errorf("parseRangeHeader(%q, %d) = (%d, %d), want (%d, %d)",
				tt.header, tt.totalSize, start, end, tt.wantStart, tt.wantEnd)
		}
	}
}

// --- handleHeadBlob miss paths ---

// TestHandleHeadBlob_UnknownBlob exercises the routeBlobMiss path in handleHeadBlob — a valid
// digest that is not in the store with upstreamFallback disabled → 404.
func TestHandleHeadBlob_UnknownBlob(t *testing.T) {
	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	unknownDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/blobs/"+unknownDigest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleHeadBlob_PeerFetch_Miss exercises the isPeerFetch branch in handleHeadBlob —
// with X-Peer-Fetch set and blob not found, a 404 is returned without routing to upstream.
func TestHandleHeadBlob_PeerFetch_Miss(t *testing.T) {
	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	unknownDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/blobs/"+unknownDigest, nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// --- handleGetManifest peerFetch miss paths ---

// TestHandleGetManifest_PeerFetch_TagMiss exercises the peerFetch path when a tag lookup fails —
// returns 404 without routing to upstream.
func TestHandleGetManifest_PeerFetch_TagMiss(t *testing.T) {
	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/missing", nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleGetManifest_PeerFetch_DigestMiss exercises the peerFetch path when a digest is not
// cached and not in the store — returns 404 without routing to upstream.
func TestHandleGetManifest_PeerFetch_DigestMiss(t *testing.T) {
	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/"+fakeDigest, nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleGetManifest_PeerFetch_GetManifestError exercises the peerFetch path when Resolve
// succeeds but GetManifest fails — returns 404 without routing to upstream.
func TestHandleGetManifest_PeerFetch_GetManifestError(t *testing.T) {
	brokenDigest := digest.FromString("broken")
	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{}, // tag points to digest but manifest data missing
		blobs:     map[digest.Digest][]byte{},
		tags:      map[string]digest.Digest{"library/test:v1": brokenDigest},
	}
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/test/manifests/v1", nil)
	req.Header.Set(proxy.PeerFetchHeader, proxy.PeerFetchValue)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleGetManifest_OrigHostPrefix exercises the X-Original-Registry host prefix
// fallback in handleGetManifest — first Resolve fails, second succeeds with host prefix.
func TestHandleGetManifest_OrigHostPrefix(t *testing.T) {
	manifestData := []byte(`{"schemaVersion":2}`)
	manifestDigest := digest.FromBytes(manifestData)
	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{manifestDigest: manifestData},
		blobs:     map[digest.Digest][]byte{},
		tags: map[string]digest.Digest{
			"ghcr.io/library/test:v1": manifestDigest, // only the prefixed form is stored
		},
	}
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(false))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/test/manifests/v1", nil)
	req.Header.Set("X-Original-Registry", "ghcr.io")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// --- routeManifestMiss upstream paths ---

// TestRouteManifestMiss_UpstreamNonOK exercises the non-200 upstream response path in
// routeManifestMiss — the response is forwarded to the client as-is.
func TestRouteManifestMiss_UpstreamNonOK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(true), WithUpstreamRegistry(upstream.URL))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestRouteManifestMiss_UpstreamOK exercises the GET path in serveFetchedManifest — manifest
// not in store, upstreamFallback=true, upstream returns 200 → body written to client.
func TestRouteManifestMiss_UpstreamOK(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(true), WithUpstreamRegistry(upstream.URL))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if string(rec.Body.Bytes()) != string(manifest) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), manifest)
	}
}

// TestRouteBlobMiss_UpstreamGet exercises serveFetchedBlob with a GET request — blob not in
// store, upstreamFallback=true, upstream serves the blob → body written to client.
func TestRouteBlobMiss_UpstreamGet(t *testing.T) {
	blobData := []byte("upstream blob content")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blobData)))
		// Set a hop-by-hop header to exercise the copyResponseHeaders continue path.
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(blobData)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(true), WithUpstreamRegistry(upstream.URL))

	unknownDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/"+unknownDigest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if string(rec.Body.Bytes()) != string(blobData) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), blobData)
	}
}

// TestRouteBlobMiss_UpstreamHead exercises the HEAD path in serveFetchedBlob — blob not in
// store, upstreamFallback=true, upstream returns 200 for HEAD → no body written.
func TestRouteBlobMiss_UpstreamHead(t *testing.T) {
	blobData := []byte("upstream blob content")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blobData)))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(true), WithUpstreamRegistry(upstream.URL))

	unknownDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/blobs/"+unknownDigest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD should have no body, got %d bytes", rec.Body.Len())
	}
}

// failReadBody is an io.ReadCloser whose Read always returns an error.
type failReadBody struct{}

func (f *failReadBody) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (f *failReadBody) Close() error               { return nil }

// TestServeFetchedManifest_ReadError covers the io.ReadAll error path in serveFetchedManifest
// by passing a response body that always fails on read.
func TestServeFetchedManifest_ReadError(t *testing.T) {
	store := newEmptyOCITestStore()
	h := NewOCIHandler(store)

	resp := &proxy.PeerFetchResponse{
		Body:   &failReadBody{},
		Header: make(http.Header),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)

	if h.serveFetchedManifest(w, r, resp) {
		t.Error("expected false when body read fails")
	}
}

// TestServeFetchedManifest_HEAD exercises the HEAD branch in serveFetchedManifest — when the
// upstream serves a 200 for a HEAD manifest request, no body is written.
func TestServeFetchedManifest_HEAD(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	mux := newRegistryFallbackMux(store, WithUpstreamFallback(true), WithUpstreamRegistry(upstream.URL))

	req := httptest.NewRequest(http.MethodHead, "/v2/library/nginx/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response should have no body, got %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("Docker-Content-Digest") == "" {
		t.Error("missing Docker-Content-Digest header")
	}
}
