package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/metrics"
	"github.com/outpostos/outpostimage/pkg/oci"
	"github.com/outpostos/outpostimage/pkg/proxy"
	"github.com/outpostos/outpostimage/pkg/routing"
)

// manifestEntry caches a served manifest by its content digest.
type manifestEntry struct {
	data      []byte
	mediaType string
}

// maxManifestFetchSize bounds how much manifest body is buffered from a
// P2P peer or upstream (m-4; aligned with the proxy-side admission limit).
// OCI recommends manifests stay below 4 MiB.
const maxManifestFetchSize = 4 << 20

// Admitter checks manifest digests against a hash whitelist table.
// A nil Admitter on the handler disables admission entirely (existing
// behavior unchanged). Implemented by admission.Checker.
type Admitter interface {
	// Allowed reports whether the manifest digest is in the whitelist.
	Allowed(dgst digest.Digest) bool
	// Version identifies the active whitelist table revision (for alerts).
	Version() string
}

// OCIHandler implements the OCI Distribution Spec v2 endpoints.
type OCIHandler struct {
	store            oci.Store
	router           routing.Router
	upstreamFallback bool
	upstreamRegistry string
	metrics          *metrics.Metrics
	admission        Admitter

	// manifestCache maps content-digest → manifest bytes+mediaType.
	// containerd fetches a manifest by tag, then re-fetches by the
	// Docker-Content-Digest it received, so we must be able to serve
	// the same manifest by its content hash.
	mu            sync.RWMutex
	manifestCache map[digest.Digest]manifestEntry
}

// Option configures OCIHandler behavior.
type Option func(*OCIHandler)

// WithRouter enables local miss routing through P2P peers.
func WithRouter(router routing.Router) Option {
	return func(h *OCIHandler) {
		h.router = router
	}
}

// WithUpstreamFallback controls whether registry misses fall back to upstream.
func WithUpstreamFallback(enabled bool) Option {
	return func(h *OCIHandler) {
		h.upstreamFallback = enabled
	}
}

// WithUpstreamRegistry sets the default upstream registry used when a request does not identify one.
func WithUpstreamRegistry(registry string) Option {
	return func(h *OCIHandler) {
		h.upstreamRegistry = registry
	}
}

// WithMetrics sets the metrics registry used for routed registry reads.
func WithMetrics(m *metrics.Metrics) Option {
	return func(h *OCIHandler) {
		h.metrics = m
	}
}

// WithAdmission enables hash whitelist admission on manifest serving.
// Passing nil leaves admission disabled.
func WithAdmission(a Admitter) Option {
	return func(h *OCIHandler) {
		h.admission = a
	}
}

// NewOCIHandler creates a new OCIHandler backed by the given store.
func NewOCIHandler(store oci.Store, opts ...Option) *OCIHandler {
	h := &OCIHandler{
		store:            store,
		upstreamFallback: true,
		upstreamRegistry: "registry-1.docker.io",
		metrics:          metrics.Default,
		manifestCache:    make(map[digest.Digest]manifestEntry),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterRoutes registers the OCI Distribution API routes on the given mux.
// Since Go's ServeMux doesn't support {wildcard...} in the middle of a path,
// we use a catch-all /v2/ pattern and route manually.
func (h *OCIHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v2/{path...}", h.handleV2)
	mux.HandleFunc("HEAD /v2/{path...}", h.handleV2)
}

// handleV2 dispatches OCI v2 API requests based on path structure.
func (h *OCIHandler) handleV2(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")

	// GET /v2/ → version check (path is empty)
	if path == "" {
		h.handleVersionCheck(w, r)
		return
	}

	// Parse OCI path: {name}/manifests/{reference} or {name}/blobs/{digest}
	// The {name} part can contain slashes (e.g., "library/nginx")
	if i := strings.LastIndex(path, "/manifests/"); i >= 0 {
		name := path[:i]
		reference := path[i+len("/manifests/"):]
		h.handleGetManifest(w, r, name, reference)
		return
	}

	if i := strings.LastIndex(path, "/blobs/"); i >= 0 {
		dgstStr := path[i+len("/blobs/"):]
		if r.Method == http.MethodHead {
			h.handleHeadBlob(w, r, dgstStr)
		} else {
			h.handleGetBlob(w, r, dgstStr)
		}
		return
	}

	writeOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry", path)
}

// handleVersionCheck implements GET /v2/ (OCI version check).
func (h *OCIHandler) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	_ = json.NewEncoder(w).Encode(map[string]any{})
}

// admitManifest applies hash whitelist admission to a manifest digest.
// With no admitter configured it always passes. On denial it writes an
// OCI-spec DENIED error (403) plus a structured alert log and returns false.
func (h *OCIHandler) admitManifest(w http.ResponseWriter, r *http.Request, dgst digest.Digest) bool {
	if h.admission == nil || h.admission.Allowed(dgst) {
		return true
	}
	slog.Warn("admission denied: manifest digest not in whitelist",
		"alert", "admission_denied",
		"digest", dgst.String(),
		"path", r.URL.Path,
		"whitelist_version", h.admission.Version(),
	)
	writeOCIError(w, http.StatusForbidden, "DENIED",
		"manifest digest not in admission whitelist", dgst.String())
	return false
}

// handleGetManifest implements GET /v2/{name}/manifests/{reference}.
func (h *OCIHandler) handleGetManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	ctx := r.Context()
	peerFetch := isPeerFetch(r)

	// If the reference is a digest, check the content-digest cache first.
	// containerd fetches manifests by tag, then re-fetches by the
	// Docker-Content-Digest it received in the first response.
	if strings.HasPrefix(reference, "sha256:") {
		contentDgst, err := digest.Parse(reference)
		if err != nil {
			writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format", reference)
			return
		}

		// Admission by digest happens before any cache/store/P2P/upstream
		// work: a denied digest must never be fetched or served.
		if !h.admitManifest(w, r, contentDgst) {
			return
		}

		h.mu.RLock()
		cached, ok := h.manifestCache[contentDgst]
		h.mu.RUnlock()

		if ok {
			w.Header().Set("Content-Type", cached.mediaType)
			w.Header().Set("Content-Length", strconv.Itoa(len(cached.data)))
			w.Header().Set("Docker-Content-Digest", contentDgst.String())
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			if r.Method != http.MethodHead {
				_, _ = w.Write(cached.data)
			}
			return
		}

		// Fall through: try the store directly with this digest
		data, mediaType, err := h.store.GetManifest(ctx, contentDgst)
		if err != nil {
			if peerFetch {
				h.recordRegistryRead("manifest", "miss")
				writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", contentDgst.String())
				return
			}
			if h.routeManifestMiss(w, r, contentDgst.String()) {
				return
			}
			h.recordRegistryRead("manifest", "miss")
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", contentDgst.String())
			return
		}
		actualDigest := digest.FromBytes(data)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", actualDigest.String())
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}

	// Tag-based lookup
	var dgst digest.Digest
	var err error

	ref := name + ":" + reference
	dgst, err = h.store.Resolve(ctx, ref)
	if err != nil {
		// If the proxy forwarded the original registry host, try with it as prefix.
		origHost := r.Header.Get("X-Original-Registry")
		if origHost != "" {
			host := strings.Split(origHost, ":")[0]
			hostRef := host + "/" + name + ":" + reference
			dgst, err = h.store.Resolve(ctx, hostRef)
		}
		if err != nil {
			if peerFetch {
				h.recordRegistryRead("manifest", "miss")
				writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", ref)
				return
			}
			if h.routeManifestMiss(w, r, ref) {
				return
			}
			h.recordRegistryRead("manifest", "miss")
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", ref)
			return
		}
	}

	data, mediaType, err := h.store.GetManifest(ctx, dgst)
	if err != nil {
		if peerFetch {
			h.recordRegistryRead("manifest", "miss")
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", dgst.String())
			return
		}
		if h.routeManifestMiss(w, r, dgst.String()) {
			return
		}
		h.recordRegistryRead("manifest", "miss")
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", dgst.String())
		return
	}

	// Docker-Content-Digest must be the actual hash of the manifest bytes,
	// not the config digest used as a lookup key. containerd validates this.
	contentDigest := digest.FromBytes(data)

	// Admission is by manifest content digest (what clients pin), not by tag.
	if !h.admitManifest(w, r, contentDigest) {
		return
	}

	// Cache by content digest so containerd can re-fetch by digest.
	h.mu.Lock()
	h.manifestCache[contentDigest] = manifestEntry{data: data, mediaType: mediaType}
	h.mu.Unlock()

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Docker-Content-Digest", contentDigest.String())
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *OCIHandler) routeManifestMiss(w http.ResponseWriter, r *http.Request, detail string) bool {
	origHost := originalRegistryHost(r)
	if h.router != nil {
		resp, peer, ok := proxy.FetchFromPeers(r.Context(), h.router, http.MethodGet, r.URL.RequestURI(), r.Header, origHost)
		if ok {
			defer resp.Body.Close()
			if h.serveFetchedManifest(w, r, resp) {
				h.recordRegistryRead("manifest", "p2p")
				slog.Info("registry manifest served from P2P", "path", r.URL.Path, "peer", peer)
				return true
			}
		}
	}

	if !h.upstreamFallback {
		slog.Debug("registry upstream fallback disabled", "path", r.URL.Path, "detail", detail)
		return false
	}

	resp, err := proxy.FetchFromUpstream(r.Context(), r, http.MethodGet, h.upstreamRegistry)
	if err != nil {
		slog.Debug("registry upstream manifest fetch failed", "path", r.URL.Path, "error", err)
		return false
	}
	defer resp.Body.Close()
	h.recordRegistryRead("manifest", "upstream")
	if resp.StatusCode != http.StatusOK {
		// B-1: with admission enabled, non-200 responses are fail-closed —
		// clients treat 200–399 as success, so a manifest body riding a
		// 206/redirect would bypass the digest check entirely.
		if h.admission != nil {
			h.relayNon200Checked(w, r, resp)
			return true
		}
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, resp.Body)
		}
		return true
	}
	return h.serveFetchedManifest(w, r, resp)
}

// relayNon200Checked handles a non-200 upstream manifest response under
// admission (B-1 hardening):
//   - 2xx/3xx (≠200): answered 502 with no body relayed; only a verified
//     200 may release manifest content, even if its digest is whitelisted.
//   - >=400: status and headers relayed (auth challenges such as
//     Www-Authenticate must survive) but the body is dropped so no
//     manifest content can ride an error status.
func (h *OCIHandler) relayNon200Checked(w http.ResponseWriter, r *http.Request, resp *proxy.PeerFetchResponse) {
	if resp.StatusCode < http.StatusBadRequest {
		slog.Warn("admission blocked non-200 success manifest response",
			"alert", "admission_denied",
			"status", resp.StatusCode,
			"path", r.URL.Path,
			"whitelist_version", h.admission.Version(),
		)
		writeOCIError(w, http.StatusBadGateway, "UNAVAILABLE",
			"upstream returned non-200 success for manifest", resp.StatusCode)
		return
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	// body intentionally dropped
}

func (h *OCIHandler) serveFetchedManifest(w http.ResponseWriter, r *http.Request, resp *proxy.PeerFetchResponse) bool {
	// m-4: bound the buffered manifest read (aligned with the proxy-side
	// admission limit); an oversized manifest is fail-closed, not served.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestFetchSize+1))
	if err != nil {
		slog.Debug("registry manifest fetch read failed", "path", r.URL.Path, "error", err)
		return false
	}
	if len(data) > maxManifestFetchSize {
		slog.Warn("registry manifest fetch exceeds size limit",
			"path", r.URL.Path, "limit", maxManifestFetchSize)
		writeOCIError(w, http.StatusBadGateway, "UNAVAILABLE",
			"fetched manifest exceeds size limit", maxManifestFetchSize)
		return true
	}
	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/vnd.docker.distribution.manifest.v2+json"
	}
	contentDigest := digest.FromBytes(data)

	// Fetched manifests (P2P peer or upstream) are admission-checked by
	// their content digest before being cached or served.
	if !h.admitManifest(w, r, contentDigest) {
		return true
	}

	h.mu.Lock()
	h.manifestCache[contentDigest] = manifestEntry{data: data, mediaType: mediaType}
	h.mu.Unlock()

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Docker-Content-Digest", contentDigest.String())
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

// handleHeadBlob implements HEAD /v2/{name}/blobs/{digest}.
func (h *OCIHandler) handleHeadBlob(w http.ResponseWriter, r *http.Request, dgstStr string) {
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format", dgstStr)
		return
	}

	reader, size, err := h.store.GetBlob(r.Context(), dgst)
	if err != nil {
		if isPeerFetch(r) {
			h.recordRegistryRead("blob", "miss")
			writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry", dgst.String())
			return
		}
		if h.routeBlobMiss(w, r, http.MethodHead, dgst) {
			return
		}
		h.recordRegistryRead("blob", "miss")
		writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry", dgst.String())
		return
	}
	reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
}

// handleGetBlob implements GET /v2/{name}/blobs/{digest} with Range request support.
func (h *OCIHandler) handleGetBlob(w http.ResponseWriter, r *http.Request, dgstStr string) {
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format", dgstStr)
		return
	}

	reader, size, err := h.store.GetBlob(r.Context(), dgst)
	if err != nil {
		if isPeerFetch(r) {
			h.recordRegistryRead("blob", "miss")
			writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry", dgst.String())
			return
		}
		if h.routeBlobMiss(w, r, http.MethodGet, dgst) {
			return
		}
		h.recordRegistryRead("blob", "miss")
		writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry", dgst.String())
		return
	}
	defer reader.Close()

	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Accept-Ranges", "bytes")

	// Check for Range header
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		start, end, ok := parseRangeHeader(rangeHeader, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}

		// Read full content then serve range (simple implementation)
		data, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		rangeLen := end - start + 1
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rangeLen))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
		return
	}

	// Full content response
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	_, _ = io.Copy(w, reader)
}

func (h *OCIHandler) routeBlobMiss(w http.ResponseWriter, r *http.Request, method string, dgst digest.Digest) bool {
	origHost := originalRegistryHost(r)
	if h.router != nil {
		resp, peer, ok := proxy.FetchFromPeers(r.Context(), h.router, method, r.URL.RequestURI(), r.Header, origHost)
		if ok {
			defer resp.Body.Close()
			h.recordRegistryRead("blob", "p2p")
			slog.Info("registry blob served from P2P", "path", r.URL.Path, "peer", peer)
			serveFetchedBlob(w, r, resp, dgst)
			return true
		}
	}

	if !h.upstreamFallback {
		slog.Debug("registry upstream fallback disabled", "path", r.URL.Path, "digest", dgst)
		return false
	}

	resp, err := proxy.FetchFromUpstream(r.Context(), r, method, h.upstreamRegistry)
	if err != nil {
		slog.Debug("registry upstream blob fetch failed", "path", r.URL.Path, "error", err)
		return false
	}
	defer resp.Body.Close()
	h.recordRegistryRead("blob", "upstream")
	serveFetchedBlob(w, r, resp, dgst)
	return true
}

func serveFetchedBlob(w http.ResponseWriter, r *http.Request, resp *proxy.PeerFetchResponse, dgst digest.Digest) {
	copyResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get("Docker-Content-Digest") == "" && dgst != "" {
		w.Header().Set("Docker-Content-Digest", dgst.String())
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if resp.ContentLength >= 0 && w.Header().Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (h *OCIHandler) recordRegistryRead(reqType, source string) {
	if h.metrics != nil {
		h.metrics.RecordRouterRegistryRead(reqType, source)
	}
}

func isPeerFetch(r *http.Request) bool {
	return r.Header.Get(proxy.PeerFetchHeader) == proxy.PeerFetchValue
}

func originalRegistryHost(r *http.Request) string {
	if host := r.Header.Get("X-Original-Registry"); host != "" {
		return host
	}
	if host := r.Header.Get("X-Upstream-Registry"); host != "" {
		return host
	}
	if r.URL.IsAbs() && r.URL.Host != "" {
		return r.URL.Host
	}
	return r.Host
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		switch strings.ToLower(k) {
		case "connection", "proxy-connection", "keep-alive",
			"proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// parseRangeHeader parses a Range header value like "bytes=0-1023" or "bytes=1024-".
// Returns the start, end (inclusive), and whether the range is valid.
func parseRangeHeader(rangeHeader string, totalSize int64) (int64, int64, bool) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeSpec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}

	var start, end int64

	if parts[0] == "" {
		// Suffix range: bytes=-500 (last 500 bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		start = totalSize - suffix
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false
		}

		if parts[1] == "" {
			// Open-ended: bytes=1024-
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return 0, 0, false
			}
		}
	}

	if start >= totalSize {
		return 0, 0, false
	}
	if end >= totalSize {
		end = totalSize - 1
	}

	return start, end, true
}

// writeOCIError writes an OCI Distribution Spec compliant error response.
func writeOCIError(w http.ResponseWriter, status int, code, message string, detail any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{
			{
				"code":    code,
				"message": message,
				"detail":  detail,
			},
		},
	})
}
