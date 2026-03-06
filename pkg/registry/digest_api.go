package registry

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/state"
)

// DigestHandler provides HTTP handlers for the digest query API.
type DigestHandler struct {
	tracker *state.Tracker
}

// NewDigestHandler creates a new DigestHandler backed by the given tracker.
func NewDigestHandler(tracker *state.Tracker) *DigestHandler {
	return &DigestHandler{tracker: tracker}
}

// RegisterRoutes registers the digest API routes on the given mux.
func (h *DigestHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/digests", h.handleListDigests)
	mux.HandleFunc("GET /api/v1/digests/{digest...}", h.handleGetDigest)
}

// handleListDigests returns all digests known to this node.
func (h *DigestHandler) handleListDigests(w http.ResponseWriter, r *http.Request) {
	digests := h.tracker.AllDigests()

	out := make([]string, len(digests))
	for i, d := range digests {
		out[i] = d.String()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"digests": out,
		"count":   len(out),
	})
}

// handleGetDigest checks if a specific digest exists on this node.
func (h *DigestHandler) handleGetDigest(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("digest")
	// Reconstruct digest with algorithm prefix if needed
	if !strings.Contains(raw, ":") {
		raw = "sha256:" + raw
	}

	dgst, err := digest.Parse(raw)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid digest format"})
		return
	}

	if h.tracker.HasDigest(dgst) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"digest": dgst.String(),
			"exists": true,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]any{
		"digest": dgst.String(),
		"exists": false,
	})
}
