package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
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

	// certMgr generates TLS certificates for MITM interception.
	// If nil, CONNECT requests are tunneled transparently.
	certMgr *CertManager

	// mitmDomains is the set of domains to perform MITM on.
	// Only CONNECT requests to these domains are intercepted.
	mitmDomains map[string]struct{}
}

// NewProxy creates a new proxy server.
// The interceptor function is called for requests matching OCI registry patterns.
// If the interceptor returns true, the request was handled; otherwise it's forwarded upstream.
func NewProxy(interceptor func(w http.ResponseWriter, r *http.Request) bool) *Proxy {
	return &Proxy{interceptor: interceptor}
}

// SetCertManager enables HTTPS MITM for the given domains.
func (p *Proxy) SetCertManager(cm *CertManager, domains []string) {
	p.certMgr = cm
	p.mitmDomains = make(map[string]struct{}, len(domains))
	for _, d := range domains {
		p.mitmDomains[strings.ToLower(d)] = struct{}{}
	}
}

// shouldMITM reports whether the given host should be MITM'd.
func (p *Proxy) shouldMITM(host string) bool {
	if p.certMgr == nil || p.mitmDomains == nil {
		return false
	}
	// Strip port if present
	h := host
	if idx := strings.LastIndex(h, ":"); idx >= 0 {
		h = h[:idx]
	}
	_, ok := p.mitmDomains[strings.ToLower(h)]
	return ok
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
// If the host is in the MITM domain list and CertManager is configured,
// TLS is terminated and OCI requests are intercepted.
// Otherwise, a simple TCP tunnel is created.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if p.shouldMITM(r.Host) {
		p.handleMITM(w, r)
		return
	}

	slog.Debug("CONNECT tunnel (passthrough)", "host", r.Host)

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

// handleMITM terminates TLS for CONNECT requests and inspects the traffic.
// OCI requests are intercepted; non-OCI HTTPS traffic is forwarded transparently.
func (p *Proxy) handleMITM(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Strip port for cert generation
	hostname := host
	if idx := strings.LastIndex(hostname, ":"); idx >= 0 {
		hostname = hostname[:idx]
	}

	slog.Debug("CONNECT MITM", "host", host)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	// Send 200 Connection Established before hijacking
	w.WriteHeader(http.StatusOK)

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		slog.Error("MITM hijack failed", "error", err)
		return
	}
	defer clientConn.Close()

	// Generate TLS certificate for the target host
	cert, err := p.certMgr.GetCertificate(hostname)
	if err != nil {
		slog.Error("MITM cert generation failed", "host", hostname, "error", err)
		return
	}

	// Wrap client connection with TLS
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
	}
	tlsConn := tls.Server(connWithBuf{clientConn, clientBuf}, tlsConfig)
	defer tlsConn.Close()

	if err := tlsConn.Handshake(); err != nil {
		slog.Debug("MITM TLS handshake failed", "host", hostname, "error", err)
		return
	}

	// Read HTTP requests over the TLS connection
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return // Connection closed
		}

		// Set the full URL for the request
		req.URL.Scheme = "https"
		req.URL.Host = host

		if isOCIRequest(req.URL.Path) && p.interceptor != nil {
			mitmWriter := &mitmResponseWriter{conn: tlsConn}
			if p.interceptor(mitmWriter, req) {
				continue
			}
		}

		// Forward to upstream
		p.forwardMITMRequest(tlsConn, req, host)
	}
}

// forwardMITMRequest forwards a request from a MITM connection to the upstream server.
func (p *Proxy) forwardMITMRequest(clientConn net.Conn, req *http.Request, host string) {
	// Connect to real upstream
	upstreamConn, err := tls.Dial("tcp", host, &tls.Config{
		ServerName: strings.Split(host, ":")[0],
	})
	if err != nil {
		slog.Error("MITM upstream connect failed", "host", host, "error", err)
		return
	}
	defer upstreamConn.Close()

	// Forward request
	if err := req.Write(upstreamConn); err != nil {
		slog.Error("MITM upstream write failed", "error", err)
		return
	}

	// Read response and forward back
	resp, err := http.ReadResponse(bufio.NewReader(upstreamConn), req)
	if err != nil {
		slog.Error("MITM upstream read failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if err := resp.Write(clientConn); err != nil {
		slog.Error("MITM response write failed", "error", err)
	}
}

// connWithBuf wraps a net.Conn with a bufio.ReadWriter to handle buffered data.
type connWithBuf struct {
	net.Conn
	buf *bufio.ReadWriter
}

func (c connWithBuf) Read(b []byte) (int, error) {
	return c.buf.Read(b)
}

// mitmResponseWriter implements http.ResponseWriter for MITM connections.
type mitmResponseWriter struct {
	conn       net.Conn
	bufw       *bufio.Writer
	header     http.Header
	statusCode int
	written    bool
}

func (w *mitmResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *mitmResponseWriter) WriteHeader(statusCode int) {
	if w.written {
		return
	}
	w.statusCode = statusCode
	w.written = true
	w.bufw = bufio.NewWriter(w.conn)

	// Write status line
	statusText := http.StatusText(statusCode)
	fmt.Fprintf(w.bufw, "HTTP/1.1 %d %s\r\n", statusCode, statusText)
	// Write headers
	for k, vv := range w.header {
		for _, v := range vv {
			fmt.Fprintf(w.bufw, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(w.bufw, "\r\n")
	w.bufw.Flush()
}

func (w *mitmResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(b)
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
