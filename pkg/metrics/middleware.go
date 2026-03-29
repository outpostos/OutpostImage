package metrics

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"
)

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Hijack implements http.Hijacker, required for CONNECT proxy support.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// Middleware returns an HTTP middleware that records metrics and logs.
// It combines metrics collection with structured logging.
func Middleware(serverName string, metrics *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			rec := &statusRecorder{ResponseWriter: w}

			// Clean path for metrics (remove trailing slashes)
			cleanPath := r.URL.Path
			if cleanPath != "/" {
				cleanPath = path.Clean(cleanPath)
			}
			// For /v2/ endpoints, classify as oci endpoints
			if cleanPath == "/v2" || (len(cleanPath) > 4 && cleanPath[:4] == "/v2/") {
				cleanPath = "/v2"
			}
			// For API v1 endpoints
			if len(cleanPath) > 8 && cleanPath[:8] == "/api/v1/" {
				cleanPath = "/api/v1"
			}

			// Call next handler
			next.ServeHTTP(rec, r)

			// Record metrics
			statusCode := rec.Status()
			metrics.ObserveHTTPRequest(serverName, r.Method, cleanPath, start, statusCode)

			// Log access
			duration := time.Since(start)
			slog.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("server", serverName),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", statusCode),
				slog.String("duration_ms", strconv.FormatInt(duration.Milliseconds(), 10)),
			)
		})
	}
}

// NewMiddleware returns a middleware using the default metrics registry.
func NewMiddleware(serverName string) func(http.Handler) http.Handler {
	return Middleware(serverName, Default)
}
