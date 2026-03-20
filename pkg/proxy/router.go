package proxy

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/state"
)

// RouteSource indicates where a request was routed.
type RouteSource string

const (
	RouteLocal    RouteSource = "local"
	RouteUpstream RouteSource = "upstream"
)

// RequestRouter examines OCI requests and decides whether to serve them
// locally or forward to upstream. It creates an interceptor function
// suitable for use with the Proxy.
func RequestRouter(tracker *state.Tracker, localAddr string) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path

		// Extract digest from blob requests to check local availability
		if i := strings.LastIndex(path, "/blobs/"); i >= 0 {
			dgstStr := path[i+len("/blobs/"):]
			dgst, err := digest.Parse(dgstStr)
			if err == nil && tracker.HasDigest(dgst) {
				slog.Info("routing to local", "source", RouteLocal, "path", path, "digest", dgst)
				proxyToLocal(w, r, localAddr)
				return true
			}
		}

		// For manifest requests, check if we can resolve the reference
		if strings.Contains(path, "/manifests/") {
			// For now, we check if we have any digests at all (simple heuristic)
			// Full resolution requires extracting name:tag and calling Store.Resolve
			// This will be enhanced when the full routing pipeline is connected
			slog.Info("routing to upstream", "source", RouteUpstream, "path", path)
			return false // Let the proxy forward to upstream
		}

		// Version check /v2/ - always forward to see if upstream is available
		slog.Debug("routing to upstream", "source", RouteUpstream, "path", path)
		return false
	}
}

// proxyToLocal redirects the request to the local OCI registry server.
func proxyToLocal(w http.ResponseWriter, r *http.Request, localAddr string) {
	// Create a request to the local registry
	localURL := "http://" + localAddr + r.URL.Path
	if r.URL.RawQuery != "" {
		localURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, localURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("local proxy failed", "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Stream response body
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}
