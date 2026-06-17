package proxy

import (
	"bufio"
	"io"
	"net"
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
		_, _ = w.Write([]byte("backend response"))
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
		_, _ = w.Write([]byte("intercepted"))
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
		_, _ = w.Write([]byte("passed through"))
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

func TestHostOnly(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"localhost:5000", "localhost"},
		{"registry.example.com:443", "registry.example.com"},
		{"192.168.1.1:8080", "192.168.1.1"},
		{"localhost", "localhost"},
		{"registry.example.com", "registry.example.com"},
	}
	for _, tt := range tests {
		got := hostOnly(tt.input)
		if got != tt.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWithInsecureRegistries(t *testing.T) {
	p := NewProxy(nil)
	p2 := p.WithInsecureRegistries([]string{"insecure.local:5000"})
	if p2 != p {
		t.Error("WithInsecureRegistries should return self")
	}
	if !p.isInsecureRegistry("insecure.local:5000") {
		t.Error("expected insecure.local:5000 to match")
	}
}

func TestIsInsecureRegistry(t *testing.T) {
	p := NewProxy(nil).WithInsecureRegistries([]string{
		"exact.local:5000",
		"noport.local",
	})

	tests := []struct {
		host string
		want bool
	}{
		{"exact.local:5000", true},
		{"exact.local:9999", false},
		{"noport.local:1234", true},
		{"noport.local:5000", true},
		{"other.local:5000", false},
	}
	for _, tt := range tests {
		got := p.isInsecureRegistry(tt.host)
		if got != tt.want {
			t.Errorf("isInsecureRegistry(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestBufferedResponseWriter(t *testing.T) {
	w := newBufferedResponseWriter()
	w.Header().Set("X-Test", "hello")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte("body content"))

	if w.statusCode != http.StatusCreated {
		t.Errorf("statusCode = %d, want %d", w.statusCode, http.StatusCreated)
	}
	if string(w.body) != "body content" {
		t.Errorf("body = %q, want %q", w.body, "body content")
	}
	if w.header.Get("X-Test") != "hello" {
		t.Error("header not set")
	}
}

func TestWriteHTTPError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		writeHTTPError(server, http.StatusBadGateway, "bad gateway")
		server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestBufferedResponseWriter_WriteTo(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	w := newBufferedResponseWriter()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("test body"))

	go func() {
		w.WriteTo(server)
		server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}
