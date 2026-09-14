package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencontainers/go-digest"
)

// fakeAdmitter implements Admitter for tests.
type fakeAdmitter struct {
	allowed map[digest.Digest]bool
}

func (f *fakeAdmitter) Allowed(d digest.Digest) bool { return f.allowed[d] }
func (f *fakeAdmitter) Version() string              { return "test-v1" }

// newAdmissionTestMux builds an OCI handler over a store containing one
// manifest tagged library/app:latest, with the given admitter.
func newAdmissionTestMux(adm Admitter) (*http.ServeMux, digest.Digest, []byte) {
	manifestData := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{}}`)
	manifestDigest := digest.FromBytes(manifestData)

	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{manifestDigest: manifestData},
		blobs:     map[digest.Digest][]byte{manifestDigest: manifestData},
		tags:      map[string]digest.Digest{"library/app:latest": manifestDigest},
	}

	handler := NewOCIHandler(store, WithUpstreamFallback(false), WithAdmission(adm))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux, manifestDigest, manifestData
}

func decodeOCIErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("response is not an OCI error body: %v (%s)", err, body)
	}
	if len(e.Errors) == 0 {
		t.Fatalf("OCI error body has no errors: %s", body)
	}
	return e.Errors[0].Code
}

func TestOCIHandler_AdmissionDeniesUnlistedTag(t *testing.T) {
	mux, _, _ := newAdmissionTestMux(&fakeAdmitter{allowed: map[digest.Digest]bool{}})

	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if code := decodeOCIErrorCode(t, rec.Body.Bytes()); code != "DENIED" {
		t.Fatalf("error code = %q, want %q", code, "DENIED")
	}
}

func TestOCIHandler_AdmissionAllowsListedTag(t *testing.T) {
	manifestData := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{}}`)
	manifestDigest := digest.FromBytes(manifestData)
	mux, _, wantBody := newAdmissionTestMux(&fakeAdmitter{allowed: map[digest.Digest]bool{manifestDigest: true}})

	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != string(wantBody) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
	}
}

// Admission is by digest, not by tag: for the same repository, a listed
// digest is served while an unlisted digest is denied.
func TestOCIHandler_AdmissionByDigestNotByTag(t *testing.T) {
	listedData := []byte(`{"schemaVersion":2,"listed":true}`)
	listedDigest := digest.FromBytes(listedData)
	unlistedData := []byte(`{"schemaVersion":2,"listed":false}`)
	unlistedDigest := digest.FromBytes(unlistedData)

	store := &ociMockStore{
		manifests: map[digest.Digest][]byte{
			listedDigest:   listedData,
			unlistedDigest: unlistedData,
		},
		blobs: map[digest.Digest][]byte{},
		// Same tag name points at the unlisted digest (image was re-tagged).
		tags: map[string]digest.Digest{"library/app:latest": unlistedDigest},
	}
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{listedDigest: true}}
	handler := NewOCIHandler(store, WithUpstreamFallback(false), WithAdmission(adm))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Listed digest → served
	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/"+listedDigest.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("listed digest: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Unlisted digest → DENIED
	req = httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/"+unlistedDigest.String(), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted digest: status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// Same tag resolving to the unlisted digest → DENIED
	req = httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/latest", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tag → unlisted digest: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if code := decodeOCIErrorCode(t, rec.Body.Bytes()); code != "DENIED" {
		t.Fatalf("error code = %q, want %q", code, "DENIED")
	}
}

// Blobs are content-addressed and self-verifying; admission happens at the
// manifest layer only. Blob reads must not be blocked by the whitelist.
func TestOCIHandler_AdmissionDoesNotBlockBlobs(t *testing.T) {
	mux, manifestDigest, _ := newAdmissionTestMux(&fakeAdmitter{allowed: map[digest.Digest]bool{}})

	// GET blob whose digest is NOT in the whitelist → still served
	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/blobs/"+manifestDigest.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET blob: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// HEAD blob → still served
	req = httptest.NewRequest(http.MethodHead, "/v2/library/app/blobs/"+manifestDigest.String(), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD blob: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// A denied digest request must be rejected before any P2P/upstream routing.
func TestOCIHandler_AdmissionDeniesBeforeUpstream(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	handler := NewOCIHandler(store,
		WithUpstreamFallback(true),
		WithUpstreamRegistry(upstream.URL),
		WithAdmission(adm),
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	unknown := digest.FromString("nowhere")
	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/"+unknown.String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if upstreamCalled {
		t.Fatal("denied digest request must not reach upstream")
	}
}

// A tag miss routed to upstream must still be admission-checked on the
// fetched manifest content before it is served to the client.
func TestOCIHandler_AdmissionChecksUpstreamFetchedManifest(t *testing.T) {
	manifestData := []byte(`{"schemaVersion":2,"from":"upstream"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifestData)
	}))
	defer upstream.Close()

	store := newEmptyOCITestStore()
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}} // nothing allowed
	handler := NewOCIHandler(store,
		WithUpstreamFallback(true),
		WithUpstreamRegistry(upstream.URL),
		WithAdmission(adm),
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if code := decodeOCIErrorCode(t, rec.Body.Bytes()); code != "DENIED" {
		t.Fatalf("error code = %q, want %q", code, "DENIED")
	}
}
