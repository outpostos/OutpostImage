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

	handler := NewOCIHandler(store)
	mux := http.NewServeMux()
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
