package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
)

// OCIHandler implements the OCI Distribution Spec v2 endpoints.
type OCIHandler struct {
	store oci.Store
}

// NewOCIHandler creates a new OCIHandler backed by the given store.
func NewOCIHandler(store oci.Store) *OCIHandler {
	return &OCIHandler{store: store}
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
	json.NewEncoder(w).Encode(map[string]any{})
}

// handleGetManifest implements GET /v2/{name}/manifests/{reference}.
func (h *OCIHandler) handleGetManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	ctx := r.Context()

	var dgst digest.Digest
	var err error

	if strings.HasPrefix(reference, "sha256:") {
		dgst, err = digest.Parse(reference)
		if err != nil {
			writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format", reference)
			return
		}
	} else {
		ref := name + ":" + reference
		dgst, err = h.store.Resolve(ctx, ref)
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

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.Write(data)
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
	w.WriteHeader(http.StatusOK)
}

// handleGetBlob implements GET /v2/{name}/blobs/{digest}.
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

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	io.Copy(w, reader)
}

// writeOCIError writes an OCI Distribution Spec compliant error response.
func writeOCIError(w http.ResponseWriter, status int, code, message string, detail any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{
			{
				"code":    code,
				"message": message,
				"detail":  detail,
			},
		},
	})
}
