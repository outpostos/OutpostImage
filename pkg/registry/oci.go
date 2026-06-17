package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
)

// manifestEntry caches a served manifest by its content digest.
type manifestEntry struct {
	data      []byte
	mediaType string
}

// OCIHandler implements the OCI Distribution Spec v2 endpoints.
type OCIHandler struct {
	store oci.Store

	// manifestCache maps content-digest → manifest bytes+mediaType.
	// containerd fetches a manifest by tag, then re-fetches by the
	// Docker-Content-Digest it received, so we must be able to serve
	// the same manifest by its content hash.
	mu            sync.RWMutex
	manifestCache map[digest.Digest]manifestEntry
}

// NewOCIHandler creates a new OCIHandler backed by the given store.
func NewOCIHandler(store oci.Store) *OCIHandler {
	return &OCIHandler{
		store:         store,
		manifestCache: make(map[digest.Digest]manifestEntry),
	}
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

// handleGetManifest implements GET /v2/{name}/manifests/{reference}.
func (h *OCIHandler) handleGetManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	ctx := r.Context()

	// If the reference is a digest, check the content-digest cache first.
	// containerd fetches manifests by tag, then re-fetches by the
	// Docker-Content-Digest it received in the first response.
	if strings.HasPrefix(reference, "sha256:") {
		contentDgst, err := digest.Parse(reference)
		if err != nil {
			writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format", reference)
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
			_, _ = w.Write(cached.data)
			return
		}

		// Fall through: try the store directly with this digest
		data, mediaType, err := h.store.GetManifest(ctx, contentDgst)
		if err != nil {
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", contentDgst.String())
			return
		}
		actualDigest := digest.FromBytes(data)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Docker-Content-Digest", actualDigest.String())
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		_, _ = w.Write(data)
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
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", ref)
			return
		}
	}

	data, mediaType, err := h.store.GetManifest(ctx, dgst)
	if err != nil {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown to registry", dgst.String())
		return
	}

	// Docker-Content-Digest must be the actual hash of the manifest bytes,
	// not the config digest used as a lookup key. containerd validates this.
	contentDigest := digest.FromBytes(data)

	// Cache by content digest so containerd can re-fetch by digest.
	h.mu.Lock()
	h.manifestCache[contentDigest] = manifestEntry{data: data, mediaType: mediaType}
	h.mu.Unlock()

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Docker-Content-Digest", contentDigest.String())
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	_, _ = w.Write(data)
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
