package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// fakeAdmitter implements Admitter for proxy tests.
type fakeAdmitter struct {
	allowed map[digest.Digest]bool
}

func (f *fakeAdmitter) Allowed(d digest.Digest) bool { return f.allowed[d] }
func (f *fakeAdmitter) Version() string              { return "test-v1" }

func decodeProxyDeniedBody(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("denied body is not structured JSON: %v (%s)", err, body)
	}
	if len(e.Errors) == 0 {
		t.Fatalf("denied body has no errors: %s", body)
	}
	return e.Errors[0].Code
}

func TestAdmissionInterceptor_DeniesUnlistedDigestManifest(t *testing.T) {
	nextCalled := false
	next := func(w http.ResponseWriter, r *http.Request) bool {
		nextCalled = true
		return false
	}
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	interceptor := AdmissionInterceptor(adm, next)

	unlisted := digest.FromString("unlisted-manifest")
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/"+unlisted.String(), nil)
	rec := httptest.NewRecorder()

	handled := interceptor(rec, req)
	if !handled {
		t.Fatal("denied manifest request must be handled (not forwarded upstream)")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if code := decodeProxyDeniedBody(t, rec.Body.Bytes()); code != "DENIED" {
		t.Fatalf("error code = %q, want %q", code, "DENIED")
	}
	if nextCalled {
		t.Fatal("next interceptor must not run for a denied digest")
	}
}

func TestAdmissionInterceptor_AllowsListedDigestManifest(t *testing.T) {
	manifestData := []byte(`{"schemaVersion":2}`)
	listed := digest.FromBytes(manifestData)

	next := func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifestData)
		return true
	}
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{listed: true}}
	interceptor := AdmissionInterceptor(adm, next)

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/"+listed.String(), nil)
	rec := httptest.NewRecorder()

	if handled := interceptor(rec, req); !handled {
		t.Fatal("allowed manifest should be handled by next")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != string(manifestData) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), manifestData)
	}
}

// Tag-based pull served by next (local/P2P): the response body digest is
// checked; an unlisted manifest is replaced by 403, a listed one passes.
func TestAdmissionInterceptor_TagPullCheckedByContentDigest(t *testing.T) {
	listedData := []byte(`{"schemaVersion":2,"listed":true}`)
	listedDigest := digest.FromBytes(listedData)
	unlistedData := []byte(`{"schemaVersion":2,"listed":false}`)

	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{listedDigest: true}}

	serve := func(data []byte) func(w http.ResponseWriter, r *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return true
		}
	}

	// Same tag, listed digest → served
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()
	if handled := AdmissionInterceptor(adm, serve(listedData))(rec, req); !handled {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("listed: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != string(listedData) {
		t.Fatalf("listed: body = %q, want %q", rec.Body.String(), listedData)
	}

	// Same tag, unlisted digest → 403 DENIED
	req = httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/latest", nil)
	rec = httptest.NewRecorder()
	if handled := AdmissionInterceptor(adm, serve(unlistedData))(rec, req); !handled {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if code := decodeProxyDeniedBody(t, rec.Body.Bytes()); code != "DENIED" {
		t.Fatalf("unlisted: error code = %q, want %q", code, "DENIED")
	}
}

// Tag-based pull not handled by next (local/P2P miss): the interceptor must
// take over the upstream fetch so the manifest is checked before reaching
// the client, instead of letting the proxy stream it through unchecked.
func TestAdmissionInterceptor_TagPullUpstreamChecked(t *testing.T) {
	listedData := []byte(`{"schemaVersion":2,"origin":"upstream-listed"}`)
	listedDigest := digest.FromBytes(listedData)
	unlistedData := []byte(`{"schemaVersion":2,"origin":"upstream-unlisted"}`)

	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{listedDigest: true}}
	next := func(w http.ResponseWriter, r *http.Request) bool { return false } // miss

	for _, tc := range []struct {
		name       string
		data       []byte
		wantStatus int
	}{
		{"listed", listedData, http.StatusOK},
		{"unlisted", unlistedData, http.StatusForbidden},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tc.data)
		}))

		req := httptest.NewRequest(http.MethodGet,
			upstream.URL+"/v2/library/app/manifests/latest", nil)
		rec := httptest.NewRecorder()

		handled := AdmissionInterceptor(adm, next)(rec, req)
		if !handled {
			t.Fatalf("%s: manifest request must be handled by admission interceptor", tc.name)
		}
		if rec.Code != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
		}
		if tc.wantStatus == http.StatusOK && rec.Body.String() != string(tc.data) {
			t.Fatalf("%s: body = %q, want %q", tc.name, rec.Body.String(), tc.data)
		}
		upstream.Close()
	}
}

// Blob requests are not admission-checked (content-addressed, self-verifying):
// they pass through to next untouched, including local misses (return false →
// normal upstream forwarding).
func TestAdmissionInterceptor_BlobsPassThrough(t *testing.T) {
	blobDigest := digest.FromString("some-blob")
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}} // empty whitelist

	nextCalled := false
	nextHandled := false
	next := func(w http.ResponseWriter, r *http.Request) bool {
		nextCalled = true
		if nextHandled {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("blob data"))
			return true
		}
		return false
	}
	interceptor := AdmissionInterceptor(adm, next)

	// Local hit: served by next even though the digest is not whitelisted
	nextHandled = true
	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/blobs/"+blobDigest.String(), nil)
	rec := httptest.NewRecorder()
	if handled := interceptor(rec, req); !handled {
		t.Fatal("blob local hit should be handled by next")
	}
	if !nextCalled || rec.Code != http.StatusOK {
		t.Fatalf("blob local hit: nextCalled=%v status=%d", nextCalled, rec.Code)
	}

	// Local miss: falls through to upstream forwarding (handled=false)
	nextHandled = false
	req = httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/blobs/"+blobDigest.String(), nil)
	rec = httptest.NewRecorder()
	if handled := interceptor(rec, req); handled {
		t.Fatal("blob local miss should fall through to upstream (handled=false)")
	}
}

// The /v2/ version check and other non-manifest OCI paths pass through.
func TestAdmissionInterceptor_VersionCheckPassThrough(t *testing.T) {
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	next := func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusOK)
		return true
	}
	req := httptest.NewRequest(http.MethodGet, "http://registry.example.com/v2/", nil)
	rec := httptest.NewRecorder()
	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("/v2/ should be handled by next")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// B-1: a colluding upstream answering a tag GET with a non-200 success
// status (e.g. 206) and a malicious manifest body must not have that body
// relayed unchecked — clients treat 200–399 as success. Expect fail-closed
// 502 with no body leak.
func TestAdmissionInterceptor_Upstream206BodyNotRelayed(t *testing.T) {
	malicious := []byte(`{"schemaVersion":2,"malicious":true}`)
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	next := func(w http.ResponseWriter, r *http.Request) bool { return false } // miss

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(malicious)
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet,
		upstream.URL+"/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()

	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("manifest request must be handled by admission interceptor")
	}
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 502 or 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "malicious") {
		t.Fatalf("malicious body leaked to client: %s", rec.Body.String())
	}
}

// B-1 companion: even a whitelisted manifest body carried on a non-200
// success status is blocked — only a verified 200 releases manifest content.
func TestAdmissionInterceptor_Upstream206EvenListedBlocked(t *testing.T) {
	data := []byte(`{"schemaVersion":2}`)
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{digest.FromBytes(data): true}}
	next := func(w http.ResponseWriter, r *http.Request) bool { return false }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet,
		upstream.URL+"/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()

	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

// B-1: upstream error responses (>=400) relay status and safe headers only;
// the body is dropped so no manifest content can ride an error status.
func TestAdmissionInterceptor_UpstreamErrorBodyDropped(t *testing.T) {
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	next := func(w http.ResponseWriter, r *http.Request) bool { return false }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="https://auth.example.com/token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"schemaVersion":2,"smuggled":true}`))
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet,
		upstream.URL+"/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()

	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec.Header().Get("Www-Authenticate") == "" {
		t.Error("safe header Www-Authenticate should be relayed")
	}
	if strings.Contains(rec.Body.String(), "smuggled") {
		t.Fatalf("error body must be dropped, got %s", rec.Body.String())
	}
}

// B-1: a next (local/P2P) response with a non-200 success status is
// likewise blocked instead of having its body relayed unchecked.
func TestAdmissionInterceptor_Next206BodyNotRelayed(t *testing.T) {
	malicious := []byte(`{"schemaVersion":2,"malicious":true}`)
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	next := func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(malicious)
		return true
	}

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/latest", nil)
	rec := httptest.NewRecorder()

	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "malicious") {
		t.Fatalf("malicious body leaked to client: %s", rec.Body.String())
	}
}

// M-1: a digest-shaped reference that fails to parse must fail closed
// locally, never fall through to unchecked upstream forwarding.
func TestAdmissionInterceptor_InvalidDigestFailClosed(t *testing.T) {
	adm := &fakeAdmitter{allowed: map[digest.Digest]bool{}}
	nextCalled := false
	next := func(w http.ResponseWriter, r *http.Request) bool {
		nextCalled = true
		return false
	}

	req := httptest.NewRequest(http.MethodGet,
		"http://registry.example.com/v2/library/app/manifests/sha256:zzzz-not-a-digest", nil)
	rec := httptest.NewRecorder()

	if handled := AdmissionInterceptor(adm, next)(rec, req); !handled {
		t.Fatal("invalid digest reference must be handled locally (fail-closed)")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if nextCalled {
		t.Fatal("next must not run for an unparseable digest reference")
	}
}
