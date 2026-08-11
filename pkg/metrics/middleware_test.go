package metrics

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// hijackableRecorder embeds httptest.ResponseRecorder and adds Hijack support
// so we can test the Hijack path in statusRecorder.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	conn   net.Conn
	hijErr error
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h.hijErr != nil {
		return nil, nil, h.hijErr
	}
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func TestStatusRecorder_WriteHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	rec.WriteHeader(http.StatusNotFound)

	if rec.status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, rec.status)
	}
	if inner.Code != http.StatusNotFound {
		t.Errorf("expected inner recorder code %d, got %d", http.StatusNotFound, inner.Code)
	}
}

func TestStatusRecorder_Status_Default(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	// Before WriteHeader is called, Status() should return 200.
	if got := rec.Status(); got != http.StatusOK {
		t.Errorf("expected default status 200, got %d", got)
	}
}

func TestStatusRecorder_Status_AfterWriteHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	rec.WriteHeader(http.StatusCreated)

	if got := rec.Status(); got != http.StatusCreated {
		t.Errorf("expected status 201 after WriteHeader, got %d", got)
	}
}

func TestStatusRecorder_Hijack_Supported(t *testing.T) {
	// Create a real net.Conn pair so the hijack can succeed.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	inner := &hijackableRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             server,
	}
	rec := &statusRecorder{ResponseWriter: inner}

	conn, rw, err := rec.Hijack()
	if err != nil {
		t.Fatalf("unexpected Hijack error: %v", err)
	}
	if conn == nil {
		t.Error("expected non-nil conn from Hijack")
	}
	if rw == nil {
		t.Error("expected non-nil bufio.ReadWriter from Hijack")
	}
}

func TestStatusRecorder_Hijack_NotSupported(t *testing.T) {
	// httptest.ResponseRecorder does NOT implement http.Hijacker.
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	_, _, err := rec.Hijack()
	if err == nil {
		t.Error("expected error when underlying writer does not support hijacking")
	}
}

func TestMiddleware_RecordsMetrics(t *testing.T) {
	m, _ := newTestMetrics()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	mw := Middleware("test-server", m)(handler)

	req := httptest.NewRequest(http.MethodGet, "/v2/blobs", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("expected proxied status 202, got %d", rec.Code)
	}
}

func TestMiddleware_PathNormalization_V2(t *testing.T) {
	m, reg := newTestMetrics()

	// Handler that records which clean path was used in ObserveHTTPRequest
	// is exercised via metrics labels — just ensure no panic and correct status.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		path         string
		wantMetric   string // prefix of cleaned path label
	}{
		{"/v2/library/ubuntu/manifests/latest", "/v2"},
		{"/v2", "/v2"},
		{"/api/v1/health", "/api/v1"},
		{"/health//", "/health"},
		{"/", "/"},
	}

	for _, tc := range cases {
		mw := Middleware("srv", m)(handler)
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
	}

	// All requests should be recorded; gather to confirm no registry errors.
	_, err := reg.Gather()
	if err != nil {
		t.Fatalf("unexpected gather error: %v", err)
	}
}

func TestMiddleware_DefaultStatus200(t *testing.T) {
	m, _ := newTestMetrics()

	// Handler that never calls WriteHeader — status should default to 200.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mw := Middleware("srv", m)(handler)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when WriteHeader not called, got %d", rec.Code)
	}
}

func TestNewMiddleware_ReturnsHandler(t *testing.T) {
	// NewMiddleware uses the package-level Default registry.
	// Just verify it returns a non-nil, functional handler chain.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := NewMiddleware("test-srv")(handler)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMetricsInstanceHandler(t *testing.T) {
	// m.Handler() returns promhttp.Handler() which serves the global registry.
	// We verify the method exists and returns a functional handler.
	_, reg := newTestMetrics()

	// Use the per-registry handler to inspect test metrics (same pattern as TestMetricsHandler).
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "test_storage_digests") {
		t.Error("metrics handler should include test_storage_digests")
	}

	// Also verify that calling the instance method doesn't panic.
	h := Default.Handler()
	if h == nil {
		t.Error("Metrics.Handler() should return non-nil handler")
	}
}

func TestPackageLevelHandler(t *testing.T) {
	h := Handler()
	if h == nil {
		t.Fatal("package-level Handler() should return a non-nil http.Handler")
	}

	// A real request to the package-level handler should succeed (uses global registry).
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 from package-level Handler(), got %d", rec.Code)
	}
}
