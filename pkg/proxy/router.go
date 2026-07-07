package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/routing"
	"github.com/outpostos/outpostimage/pkg/state"
)

// insecurePeerClient is used for HTTPS peer connections on trusted internal networks
// where peers may use self-signed certificates (common in edge deployments).
var insecurePeerClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

// peerSchemeAndAddr normalizes a peer address and returns (scheme, host:port).
// Supports: "host:port", ":port" (→ localhost:port), "http://...", "https://...".
func peerSchemeAndAddr(addr string) (scheme, hostport string) {
	switch {
	case strings.HasPrefix(addr, "https://"):
		return "https", addr[8:]
	case strings.HasPrefix(addr, "http://"):
		return "http", addr[7:]
	default:
		if strings.HasPrefix(addr, ":") {
			return "http", "localhost" + addr
		}
		return "http", addr
	}
}

// peerClient returns the HTTP client to use for a given scheme.
func peerClient(scheme string) *http.Client {
	if scheme == "https" {
		return insecurePeerClient
	}
	return http.DefaultClient
}

// RouteSource indicates where a request was routed.
type RouteSource string

const (
	RouteLocal    RouteSource = "local"
	RouteP2P      RouteSource = "p2p"
	RouteUpstream RouteSource = "upstream"
)

// PeerFetchResponse is a successful response fetched from a registry peer or upstream.
type PeerFetchResponse struct {
	StatusCode    int
	Header        http.Header
	Body          io.ReadCloser
	ContentLength int64
}

// FetchFromPeers tries healthy registry peers in router order and returns the first successful response.
func FetchFromPeers(ctx context.Context, r routing.Router, method, path string, headers http.Header, origHost string) (*PeerFetchResponse, string, bool) {
	for _, peer := range HealthyPeersFromRouter(ctx, r) {
		resp, err := FetchFromPeer(ctx, peer, method, path, headers, origHost)
		if err == nil {
			return resp, peer, true
		}
		slog.Debug("peer fetch failed", "peer", peer, "method", method, "path", path, "error", err)
	}
	return nil, "", false
}

// FetchFromPeer fetches an OCI path from a single registry peer.
func FetchFromPeer(ctx context.Context, addr, method, path string, headers http.Header, origHost string) (*PeerFetchResponse, error) {
	scheme, hostport := peerSchemeAndAddr(addr)
	targetURL := scheme + "://" + hostport + path
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, headers)
	if origHost != "" {
		req.Header.Set("X-Original-Registry", origHost)
	}

	resp, err := peerClient(scheme).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("non-success status %d", resp.StatusCode)
	}

	return &PeerFetchResponse{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header.Clone(),
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
	}, nil
}

// FetchFromUpstream fetches an OCI path from the original registry.
// The registry is read from X-Upstream-Registry or X-Original-Registry, then falls back to defaultRegistry.
func FetchFromUpstream(ctx context.Context, r *http.Request, method, defaultRegistry string) (*PeerFetchResponse, error) {
	rawRegistry := r.Header.Get("X-Upstream-Registry")
	if rawRegistry == "" {
		rawRegistry = r.Header.Get("X-Original-Registry")
	}
	if rawRegistry == "" && r.URL.IsAbs() && r.URL.Host != "" {
		rawRegistry = r.URL.Host
	}
	if rawRegistry == "" {
		rawRegistry = defaultRegistry
	}

	base, err := upstreamBaseURL(rawRegistry)
	if err != nil {
		return nil, err
	}
	targetURL := strings.TrimRight(base, "/") + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("X-Upstream-Registry")
	req.Header.Del("X-Original-Registry")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	return &PeerFetchResponse{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header.Clone(),
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
	}, nil
}

func upstreamBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty upstream registry")
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if u.Host == "" {
			return "", fmt.Errorf("invalid upstream registry %q", raw)
		}
		u.Path = ""
		u.RawQuery = ""
		u.Fragment = ""
		return u.String(), nil
	}

	if raw == "docker.io" || raw == "index.docker.io" {
		raw = "registry-1.docker.io"
	}
	return "https://" + raw, nil
}

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
				// Check local first via tracker (fast path for config blobs)
				if tracker.HasDigest(dgst) {
					slog.Info("routing blob to local (tracker)", "source", RouteLocal, "path", path)
					proxyToLocal(w, r, localAddr)
					return true
				}

				// Tracker only indexes config digests; layer blobs need a direct check
				if tryBlobFromLocal(r.Context(), localAddr, path) {
					slog.Info("routing blob to local (direct)", "source", RouteLocal, "path", path)
					proxyToLocal(w, r, localAddr)
					return true
				}

				// Try P2P peers
				if p2pRouter != nil {
					peers := HealthyPeersFromRouter(r.Context(), p2pRouter)
					for _, peer := range peers {
						if tryBlobFromLocal(r.Context(), peer, path) {
							slog.Info("routing blob to P2P", "source", RouteP2P, "path", path, "peer", peer)
							proxyToLocal(w, r, peer)
							return true
						}
					}
				}

				// Legacy P2P parallel fetcher path
				if p2pRouter != nil && fetcher != nil {
					balancer, err := p2pRouter.Lookup(r.Context(), dgst)
					if err == nil && balancer.Len() > 0 {
						slog.Info("routing to P2P peers (parallel)", "source", RouteP2P, "path", path,
							"digest", dgst, "peers", balancer.Len())
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

		// For manifest requests, try local then P2P before falling back to upstream
		if strings.Contains(path, "/manifests/") {
			// Extract original registry host for tag resolution
			origHost := r.URL.Host
			if origHost == "" {
				origHost = r.Host
			}

			// Try local registry first
			if tryManifestFrom(r.Context(), w, localAddr, path, origHost) {
				slog.Info("manifest served from local", "source", RouteLocal, "path", path)
				return true
			}

			// Try P2P peers
			if p2pRouter != nil {
				peers := HealthyPeersFromRouter(r.Context(), p2pRouter)
				for _, peer := range peers {
					if tryManifestFrom(r.Context(), w, peer, path, origHost) {
						slog.Info("manifest served from P2P", "source", RouteP2P, "path", path, "peer", peer)
						return true
					}
				}
			}

			slog.Info("routing manifest to upstream", "source", RouteUpstream, "path", path)
			return false
		}

		// Version check /v2/ - respond locally so MITM proxy clients don't
		// need to reach the upstream registry for the handshake.
		if path == "/v2/" || path == "/v2" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			slog.Info("v2 version check served locally", "source", RouteLocal, "path", path)
			return true
		}

		slog.Debug("routing to upstream", "source", RouteUpstream, "path", path)
		return false
	}
}

// headBlobSize sends a HEAD request to a peer to get the blob size.
func headBlobSize(ctx context.Context, peer, path string) int64 {
	scheme, hostport := peerSchemeAndAddr(peer)
	url := fmt.Sprintf("%s://%s%s", scheme, hostport, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0
	}
	resp, err := peerClient(scheme).Do(req)
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

// tryBlobFromLocal sends a HEAD request to check if a blob exists at the given addr.
// addr may include a scheme prefix ("https://host:port") for HTTPS peers; defaults to HTTP.
func tryBlobFromLocal(ctx context.Context, addr, path string) bool {
	scheme, hostport := peerSchemeAndAddr(addr)
	url := scheme + "://" + hostport + path
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := peerClient(scheme).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// tryManifestFrom attempts to fetch a manifest from a registry peer (or local).
// It makes a GET request; if the response is 200, it writes to w and returns true.
// On any failure or non-200, it returns false without touching w.
// addr may include a scheme prefix ("https://host:port") for HTTPS peers; defaults to HTTP.
// origHost is the original registry host (e.g., "ghcr.io") for tag resolution.
func tryManifestFrom(ctx context.Context, w http.ResponseWriter, addr, path string, origHost string) bool {
	scheme, hostport := peerSchemeAndAddr(addr)
	url := scheme + "://" + hostport + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("tryManifestFrom: request creation failed", "addr", addr, "error", err)
		return false
	}
	// Pass original registry host so the OCI handler can resolve tags
	// stored with registry prefix (e.g., "ghcr.io/test/debian:latest")
	if origHost != "" {
		req.Header.Set("X-Original-Registry", origHost)
	}
	resp, err := peerClient(scheme).Do(req)
	if err != nil {
		slog.Debug("tryManifestFrom: request failed", "addr", addr, "error", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Debug("tryManifestFrom: non-200", "addr", addr, "status", resp.StatusCode)
		return false
	}

	// Success — copy the response to the caller
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// HealthyPeersFromRouter extracts healthy peers from a Router.
// For StaticRouter, it directly accesses the healthy peer list.
func HealthyPeersFromRouter(ctx context.Context, r routing.Router) []string {
	if sr, ok := r.(*routing.StaticRouter); ok {
		return sr.HealthyPeers()
	}
	return nil
}

// proxyToLocal redirects the request to the local OCI registry server.
// localAddr may include a scheme prefix ("https://host:port") for HTTPS; defaults to HTTP.
func proxyToLocal(w http.ResponseWriter, r *http.Request, localAddr string) {
	scheme, hostport := peerSchemeAndAddr(localAddr)
	localURL := scheme + "://" + hostport + r.URL.Path
	if r.URL.RawQuery != "" {
		localURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, localURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)

	resp, err := peerClient(scheme).Do(req)
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
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}
