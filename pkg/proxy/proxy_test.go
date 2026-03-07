package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsOCIRequest(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v2/", true},
		{"/v2", true},
		{"/v2/library/nginx/manifests/latest", true},
		{"/v2/library/nginx/blobs/sha256:abc", true},
		{"/healthz", false},
		{"/api/v1/digests", false},
		{"/", false},
	}
	for _, tt := range tests {
		if got := isOCIRequest(tt.path); got != tt.want {
			t.Errorf("isOCIRequest(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProxy_HTTPForward(t *testing.T) {
	// Set up a backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "true")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	proxy := NewProxy(nil)

	// Create a proxy request (absolute URL form)
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/test", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body, _ := io.ReadAll(rec.Body)
	if string(body) != "backend response" {
		t.Fatalf("body = %q, want %q", body, "backend response")
	}

	if rec.Header().Get("X-Backend") != "true" {
		t.Fatal("missing X-Backend header from backend")
	}
}

func TestProxy_OCIInterceptor(t *testing.T) {
	intercepted := false
	proxy := NewProxy(func(w http.ResponseWriter, r *http.Request) bool {
		intercepted = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("intercepted"))
		return true
	})

	req := httptest.NewRequest(http.MethodGet, "http://registry.example.com/v2/", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if !intercepted {
		t.Fatal("expected OCI request to be intercepted")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxy_NonOCINotIntercepted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("passed through"))
	}))
	defer backend.Close()

	intercepted := false
	proxy := NewProxy(func(w http.ResponseWriter, r *http.Request) bool {
		intercepted = true
		return true
	})

	req := httptest.NewRequest(http.MethodGet, backend.URL+"/healthz", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if intercepted {
		t.Fatal("non-OCI request should not be intercepted")
	}
}

func TestCopyHeaders_SkipsHopByHop(t *testing.T) {
	src := http.Header{
		"Content-Type": {"application/json"},
		"Connection":   {"keep-alive"},
		"X-Custom":     {"value"},
	}
	dst := http.Header{}

	copyHeaders(dst, src)

	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be copied")
	}
	if dst.Get("Connection") != "" {
		t.Error("Connection should not be copied (hop-by-hop)")
	}
	if dst.Get("X-Custom") != "value" {
		t.Error("X-Custom should be copied")
	}
}
