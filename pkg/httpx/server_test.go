package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestHandleHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	HandleHealthz().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("content-type = %q, want %q", ct, "application/json")
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want %q", body["status"], "ok")
	}
}

func TestHandleReadyz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	HandleReadyz().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ready" {
		t.Fatalf("status = %q, want %q", body["status"], "ready")
	}
}

func TestServe_StartsAndShutdown(t *testing.T) {
	// Pick a random free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	Serve(gctx, g, addr, handler, "test")

	// Wait briefly for the server to start accepting connections.
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("request to server failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Cancel the context to trigger graceful shutdown.
	cancel()
	if err := g.Wait(); err != nil {
		t.Fatalf("errgroup error: %v", err)
	}
}

// TestServe_BindError exercises the error-return path in Serve's ListenAndServe goroutine
// when the address is already in use (ListenAndServe returns an error != ErrServerClosed).
func TestServe_BindError(t *testing.T) {
	// Keep the listener open so the HTTP server cannot bind to the same address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	Serve(gctx, g, addr, handler, "test-bind-error")

	// The server fails to bind immediately; g.Wait() should return a non-nil error.
	if err := g.Wait(); err == nil {
		t.Error("expected error when address is already in use")
	}
}
