package proxy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Admitter checks manifest digests against a hash whitelist table.
// Implemented by admission.Checker.
type Admitter interface {
	// Allowed reports whether the manifest digest is in the whitelist.
	Allowed(dgst digest.Digest) bool
	// Version identifies the active whitelist table revision (for alerts).
	Version() string
}

// maxManifestRefLen bounds the reference segment parsed from the request
// path (external input). A sha256 digest string is 71 bytes; tags are
// limited to 128 by the distribution spec.
const maxManifestRefLen = 256

// maxAdmissionManifestSize bounds how much manifest body the admission
// interceptor will buffer for digest verification. OCI recommends
// manifests stay below 4 MiB.
const maxAdmissionManifestSize = 4 << 20

// AdmissionInterceptor wraps a proxy interceptor with hash whitelist
// admission on manifest requests.
//
// Semantics:
//   - GET/HEAD {name}/manifests/{digest}: the digest is checked before any
//     routing; a digest not in the whitelist is answered 403 DENIED and never
//     fetched or forwarded.
//   - GET {name}/manifests/{tag}: the response (from local/P2P via next, or
//     from upstream fetched by this interceptor on a next-miss) is buffered,
//     its content digest computed and checked; unlisted content is replaced
//     by 403 DENIED. Manifest requests are therefore always handled here —
//     they never fall through to the proxy's unchecked forwarding path.
//   - HEAD {name}/manifests/{tag}: passed through. No manifest content is
//     transferred; the follow-up GET (by digest or tag) is checked.
//   - Blob requests are NOT checked: blobs are content-addressed and
//     self-verifying, and admission happens at the manifest layer only.
//
// The returned interceptor always denies on verification failure rather than
// falling open (fail-safe).
func AdmissionInterceptor(adm Admitter, next func(w http.ResponseWriter, r *http.Request) bool) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if adm == nil {
			return next(w, r)
		}

		reference, ok := manifestReference(r.URL.Path)
		if !ok {
			// Not a manifest request (blobs, /v2/ version check, ...): no
			// admission at this layer.
			return next(w, r)
		}
		if len(reference) > maxManifestRefLen {
			writeAdmissionDenied(w, r, adm, "", "reference exceeds length limit")
			return true
		}

		// Digest-referenced manifest: check before any routing.
		if strings.Contains(reference, ":") {
			dgst, err := digest.Parse(reference)
			if err != nil {
				// Fail closed locally (M-1): falling through to next would
				// end in unchecked upstream forwarding on a miss.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"errors":[{"code":"DIGEST_INVALID","message":"invalid digest format","detail":%q}]}%s`, reference, "\n")
				return true
			}
			if !adm.Allowed(dgst) {
				writeAdmissionDenied(w, r, adm, dgst.String(), "manifest digest not in admission whitelist")
				return true
			}
			return next(w, r)
		}

		// Tag-referenced HEAD: no content is transferred; pass through.
		if r.Method == http.MethodHead {
			return next(w, r)
		}

		// Tag-referenced GET: buffer whatever next serves and verify its
		// content digest before releasing it to the client.
		rec := newBufferedResponseWriter()
		if next(rec, r) {
			serveCheckedManifest(w, r, adm, rec.statusCode, rec.header, rec.body)
			return true
		}

		// next missed (no local/P2P copy): fetch upstream ourselves so the
		// manifest is verified instead of being forwarded unchecked.
		status, header, body, err := fetchUpstreamManifest(r)
		if err != nil {
			slog.Error("admission upstream manifest fetch failed", "path", r.URL.Path, "error", err)
			http.Error(w, "admission upstream fetch failed", http.StatusBadGateway)
			return true
		}
		serveCheckedManifest(w, r, adm, status, header, body)
		return true
	}
}

// manifestReference extracts the {reference} segment from an OCI manifest
// path, reporting false for non-manifest paths.
func manifestReference(path string) (string, bool) {
	i := strings.LastIndex(path, "/manifests/")
	if i < 0 {
		return "", false
	}
	return path[i+len("/manifests/"):], true
}

// serveCheckedManifest verifies a buffered manifest response against the
// whitelist and writes either the original response or a denial.
//
// B-1 hardening — status handling is exhaustive and fail-closed:
//   - 200: content digest checked against the whitelist; miss → 403 DENIED.
//   - other 2xx/3xx (e.g. 206, redirects): clients treat 200–399 as
//     success, so a body here could smuggle an unverified manifest. These
//     are answered 502 with no body relayed, even if the digest matches
//     (only a verified 200 releases manifest content).
//   - >=400: the status and headers are relayed (auth challenges like
//     Www-Authenticate must survive) but the body is dropped so no
//     manifest content can ride an error status.
func serveCheckedManifest(w http.ResponseWriter, r *http.Request, adm Admitter, status int, header http.Header, body []byte) {
	switch {
	case status == http.StatusOK:
		dgst := digest.FromBytes(body)
		if !adm.Allowed(dgst) {
			writeAdmissionDenied(w, r, adm, dgst.String(), "manifest digest not in admission whitelist")
			return
		}
		copyHeaders(w.Header(), header)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	case status < http.StatusBadRequest:
		slog.Warn("admission blocked non-200 success manifest response",
			"alert", "admission_denied",
			"status", status,
			"path", r.URL.Path,
			"host", r.URL.Host,
			"whitelist_version", adm.Version(),
		)
		http.Error(w, "upstream returned non-200 success for manifest", http.StatusBadGateway)
	default:
		copyHeaders(w.Header(), header)
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		// body intentionally dropped
	}
}

// fetchUpstreamManifest fetches the manifest from the original registry.
// Proxy interceptors always receive absolute URLs (plain HTTP proxying keeps
// the absolute form; the MITM path reconstructs scheme and host).
func fetchUpstreamManifest(r *http.Request) (int, http.Header, []byte, error) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		return 0, nil, nil, fmt.Errorf("no upstream host in request URL %q", r.URL.String())
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	copyHeaders(req.Header, r.Header)

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdmissionManifestSize+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(body) > maxAdmissionManifestSize {
		return 0, nil, nil, errors.New("manifest exceeds admission size limit")
	}
	header := resp.Header.Clone()
	header.Del("Content-Length") // body may be re-written; let the server recompute
	return resp.StatusCode, header, body, nil
}

// writeAdmissionDenied writes a 403 with an OCI-style DENIED error body and
// emits a structured alert log.
func writeAdmissionDenied(w http.ResponseWriter, r *http.Request, adm Admitter, dgst, message string) {
	slog.Warn("admission denied: manifest not in whitelist",
		"alert", "admission_denied",
		"digest", dgst,
		"path", r.URL.Path,
		"host", r.URL.Host,
		"whitelist_version", adm.Version(),
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"errors":[{"code":"DENIED","message":%q,"detail":%q}]}%s`, message, dgst, "\n")
}
