package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// Proxy is an HTTP/HTTPS proxy server that can intercept OCI registry requests.
type Proxy struct {
	// interceptor is called for OCI registry API requests.
	// If nil, all requests are forwarded transparently.
	interceptor func(w http.ResponseWriter, r *http.Request) bool
}

// NewProxy creates a new proxy server.
// The interceptor function is called for requests matching OCI registry patterns.
// If the interceptor returns true, the request was handled; otherwise it's forwarded upstream.
func NewProxy(interceptor func(w http.ResponseWriter, r *http.Request) bool) *Proxy {
	return &Proxy{interceptor: interceptor}
}

// ServeHTTP handles both HTTP proxy requests and HTTPS CONNECT tunneling.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

// handleHTTP handles plain HTTP proxy requests.
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Check if this is an OCI registry request
	if p.interceptor != nil && isOCIRequest(r.URL.Path) {
		slog.Debug("intercepting OCI request", "method", r.Method, "url", r.URL.String())
		if p.interceptor(w, r) {
			return
		}
	}

	// Forward transparently
	slog.Debug("forwarding request", "method", r.Method, "url", r.URL.String())

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(outReq.Header, r.Header)

	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleConnect handles HTTPS CONNECT tunnel requests.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	slog.Debug("CONNECT tunnel", "host", r.Host)

	destConn, err := net.DialTimeout("tcp", r.Host, 10e9) // 10 second timeout
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer destConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	// Send 200 Connection Established
	w.WriteHeader(http.StatusOK)

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(destConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, destConn)
		done <- struct{}{}
	}()
	<-done
}

// isOCIRequest checks if the URL path matches OCI Distribution Spec patterns.
func isOCIRequest(path string) bool {
	return strings.HasPrefix(path, "/v2/") || path == "/v2"
}

// copyHeaders copies headers from src to dst, skipping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
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
