package proxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// Proxy is an HTTP/HTTPS proxy server that can intercept OCI registry requests.
// When a CertManager is provided, HTTPS CONNECT requests are MITM'd to enable
// interception of OCI registry traffic over TLS.
type Proxy struct {
	interceptor func(w http.ResponseWriter, r *http.Request) bool
	certMgr     *CertManager
}

// NewProxy creates a new proxy server.
// The interceptor function is called for requests matching OCI registry patterns.
// If certMgr is non-nil, HTTPS traffic will be MITM'd for OCI interception.
func NewProxy(interceptor func(w http.ResponseWriter, r *http.Request) bool, certMgr ...*CertManager) *Proxy {
	p := &Proxy{interceptor: interceptor}
	if len(certMgr) > 0 {
		p.certMgr = certMgr[0]
	}
	return p
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
	if p.interceptor != nil && isOCIRequest(r.URL.Path) {
		slog.Debug("intercepting OCI request", "method", r.Method, "url", r.URL.String())
		if p.interceptor(w, r) {
			return
		}
	}

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
// If a CertManager is available and the target is an OCI registry host,
// it performs TLS MITM to intercept and inspect the decrypted traffic.
// Otherwise, it falls back to transparent TCP tunneling.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Normalize: ensure host:port format
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}
	hostname := hostOnly(host)

	slog.Debug("CONNECT", "host", host, "mitm", p.certMgr != nil)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	// Send 200 Connection Established
	w.WriteHeader(http.StatusOK)
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// If we have a CertManager, do MITM to intercept OCI requests
	if p.certMgr != nil {
		p.handleMITM(clientConn, clientBuf, hostname, host)
		return
	}

	// Fallback: transparent TCP tunnel
	destConn, err := net.DialTimeout("tcp", host, 10e9)
	if err != nil {
		return
	}
	defer destConn.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(destConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, destConn); done <- struct{}{} }()
	<-done
}

// handleMITM performs TLS man-in-the-middle on the hijacked connection.
// It presents a dynamically generated certificate to the client, decrypts
// the traffic, and either intercepts OCI requests or forwards to upstream.
func (p *Proxy) handleMITM(clientConn net.Conn, _ *bufio.ReadWriter, hostname, hostPort string) {
	cert, err := p.certMgr.GetCertificate(hostname)
	if err != nil {
		slog.Error("MITM cert generation failed", "host", hostname, "error", err)
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
	}
	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		slog.Debug("MITM TLS handshake failed", "host", hostname, "error", err)
		return
	}
	defer tlsConn.Close()

	// Read HTTP requests from the decrypted TLS connection
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return // Connection closed or error
		}

		// Reconstruct the full URL for the request
		req.URL.Scheme = "https"
		req.URL.Host = hostname
		req.RequestURI = "" // Required for http.Client

		slog.Debug("MITM intercepted", "method", req.Method, "host", hostname, "path", req.URL.Path)

		// Try interceptor for OCI requests
		if p.interceptor != nil && isOCIRequest(req.URL.Path) {
			rw := newBufferedResponseWriter()
			if p.interceptor(rw, req) {
				rw.WriteTo(tlsConn)
				continue
			}
		}

		// Forward to upstream via TLS
		p.forwardToUpstream(tlsConn, req, hostPort)
	}
}

// forwardToUpstream sends the request to the real server and writes the response back.
func (p *Proxy) forwardToUpstream(clientTLS net.Conn, req *http.Request, hostPort string) {
	upstreamConn, err := tls.Dial("tcp", hostPort, &tls.Config{
		ServerName: hostOnly(hostPort),
	})
	if err != nil {
		slog.Error("upstream TLS dial failed", "host", hostPort, "error", err)
		writeHTTPError(clientTLS, 502, "Bad Gateway")
		return
	}
	defer upstreamConn.Close()

	if err := req.Write(upstreamConn); err != nil {
		writeHTTPError(clientTLS, 502, "Bad Gateway")
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(upstreamConn), req)
	if err != nil {
		writeHTTPError(clientTLS, 502, "Bad Gateway")
		return
	}
	defer resp.Body.Close()

	resp.Write(clientTLS)
}

// writeHTTPError writes a simple HTTP error response to a raw connection.
func writeHTTPError(conn net.Conn, status int, msg string) {
	resp := &http.Response{
		StatusCode: status,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(msg)),
	}
	resp.Header.Set("Content-Type", "text/plain")
	resp.Write(conn)
}

// bufferedResponseWriter captures an HTTP response in memory so we can
// write it to a raw TLS connection (not an http.ResponseWriter).
type bufferedResponseWriter struct {
	statusCode int
	header     http.Header
	body       []byte
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{
		statusCode: http.StatusOK,
		header:     make(http.Header),
	}
}

func (w *bufferedResponseWriter) Header() http.Header     { return w.header }
func (w *bufferedResponseWriter) WriteHeader(code int)     { w.statusCode = code }
func (w *bufferedResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

// WriteTo writes the captured response as an HTTP response to a raw connection.
func (w *bufferedResponseWriter) WriteTo(conn net.Conn) {
	resp := &http.Response{
		StatusCode:    w.statusCode,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          io.NopCloser(strings.NewReader(string(w.body))),
		ContentLength: int64(len(w.body)),
	}
	resp.Write(conn)
}

// hostOnly extracts the hostname from a host:port string.
func hostOnly(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return h
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
