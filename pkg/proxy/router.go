package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// RouteSource indicates where a request was routed.
type RouteSource string

const (
	RouteLocal    RouteSource = "local"
	RouteP2P     RouteSource = "p2p"
	RouteUpstream RouteSource = "upstream"
)

// RequestRouter examines OCI requests and decides whether to serve them
// locally, from P2P peers, or forward to upstream.
// If router is provided, it enables P2P blob fetch from remote peers.
func RequestRouter(tracker *state.Tracker, localAddr string, router ...routing.Router) func(w http.ResponseWriter, r *http.Request) bool {
	var p2pRouter routing.Router
	var fetcher *ParallelFetcher
	if len(router) > 0 && router[0] != nil {
		p2pRouter = router[0]
		fetcher = NewParallelFetcher(DefaultChunkSize, DefaultParallelism)
	}

	return func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path

		// Extract digest from blob requests to check local availability
		if i := strings.LastIndex(path, "/blobs/"); i >= 0 {
			dgstStr := path[i+len("/blobs/"):]
			dgst, err := digest.Parse(dgstStr)
			if err == nil {
				// Check local first
				if tracker.HasDigest(dgst) {
					slog.Info("routing to local", "source", RouteLocal, "path", path, "digest", dgst)
					proxyToLocal(w, r, localAddr)
					return true
				}

				// Try P2P peers
				if p2pRouter != nil && fetcher != nil {
					balancer, err := p2pRouter.Lookup(r.Context(), dgst)
					if err == nil && balancer.Len() > 0 {
						slog.Info("routing to P2P peers", "source", RouteP2P, "path", path,
							"digest", dgst, "peers", balancer.Len())
						// Get blob size from first peer via HEAD
						peer, _ := balancer.Next()
						size := headBlobSize(r.Context(), peer, path)
						if size > 0 {
							if err := fetcher.StreamFetch(r.Context(), w, balancer, path, size); err != nil {
								slog.Error("P2P fetch failed, falling back to upstream", "error", err)
								return false
							}
							return true
						}
					}
				}
			}
		}

		// For manifest requests, check if we can resolve the reference
		if strings.Contains(path, "/manifests/") {
			slog.Info("routing to upstream", "source", RouteUpstream, "path", path)
			return false
		}

		// Version check /v2/ - always forward to see if upstream is available
		slog.Debug("routing to upstream", "source", RouteUpstream, "path", path)
		return false
	}
}

// headBlobSize sends a HEAD request to a peer to get the blob size.
func headBlobSize(ctx context.Context, peer, path string) int64 {
	url := fmt.Sprintf("http://%s%s", peer, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	size := resp.ContentLength
	if size <= 0 {
		return 0
	}
	return size
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
