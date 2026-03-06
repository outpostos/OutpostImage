package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoggingMiddleware(t *testing.T) {
	// Capture slog output
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	slog.SetDefault(logger)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/test/path", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("failed to parse log entry: %v\nraw: %s", err, buf.String())
	}

	if logEntry["method"] != "POST" {
		t.Errorf("method = %v, want POST", logEntry["method"])
	}
	if logEntry["path"] != "/test/path" {
		t.Errorf("path = %v, want /test/path", logEntry["path"])
	}
	// JSON numbers are float64
	if status, ok := logEntry["status"].(float64); !ok || int(status) != 201 {
		t.Errorf("status = %v, want 201", logEntry["status"])
	}
	if _, ok := logEntry["duration_ms"]; !ok {
		t.Error("missing duration_ms field")
	}
}

func TestLoggingMiddleware_DefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	slog.SetDefault(logger)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("failed to parse log: %v", err)
	}

	if status, ok := logEntry["status"].(float64); !ok || int(status) != 200 {
		t.Errorf("status = %v, want 200", logEntry["status"])
	}
}
