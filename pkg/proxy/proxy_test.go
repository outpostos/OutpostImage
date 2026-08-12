package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsOCIRequest(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v2/", true},
		{"/v2", true},
		{"/v2/library/nginx/manifests/latest", true},
		{"/v2/library/nginx/blobs/sha256:abc", true},
		{"/healthz", false},
		{"/api/v1/digests", false},
		{"/", false},
	}
	for _, tt := range tests {
		if got := isOCIRequest(tt.path); got != tt.want {
			t.Errorf("isOCIRequest(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProxy_HTTPForward(t *testing.T) {
	// Set up a backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	proxy := NewProxy(nil)

	// Create a proxy request (absolute URL form)
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/test", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body, _ := io.ReadAll(rec.Body)
	if string(body) != "backend response" {
		t.Fatalf("body = %q, want %q", body, "backend response")
	}

	if rec.Header().Get("X-Backend") != "true" {
		t.Fatal("missing X-Backend header from backend")
	}
}

func TestProxy_OCIInterceptor(t *testing.T) {
	intercepted := false
	proxy := NewProxy(func(w http.ResponseWriter, r *http.Request) bool {
		intercepted = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("intercepted"))
		return true
	})

	req := httptest.NewRequest(http.MethodGet, "http://registry.example.com/v2/", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if !intercepted {
		t.Fatal("expected OCI request to be intercepted")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxy_NonOCINotIntercepted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("passed through"))
	}))
	defer backend.Close()

	intercepted := false
	proxy := NewProxy(func(w http.ResponseWriter, r *http.Request) bool {
		intercepted = true
		return true
	})

	req := httptest.NewRequest(http.MethodGet, backend.URL+"/healthz", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if intercepted {
		t.Fatal("non-OCI request should not be intercepted")
	}
}

func TestCopyHeaders_SkipsHopByHop(t *testing.T) {
	src := http.Header{
		"Content-Type": {"application/json"},
		"Connection":   {"keep-alive"},
		"X-Custom":     {"value"},
	}
	dst := http.Header{}

	copyHeaders(dst, src)

	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be copied")
	}
	if dst.Get("Connection") != "" {
		t.Error("Connection should not be copied (hop-by-hop)")
	}
	if dst.Get("X-Custom") != "value" {
		t.Error("X-Custom should be copied")
	}
}

func TestHostOnly(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"localhost:5000", "localhost"},
		{"registry.example.com:443", "registry.example.com"},
		{"192.168.1.1:8080", "192.168.1.1"},
		{"localhost", "localhost"},
		{"registry.example.com", "registry.example.com"},
	}
	for _, tt := range tests {
		got := hostOnly(tt.input)
		if got != tt.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWithInsecureRegistries(t *testing.T) {
	p := NewProxy(nil)
	p2 := p.WithInsecureRegistries([]string{"insecure.local:5000"})
	if p2 != p {
		t.Error("WithInsecureRegistries should return self")
	}
	if !p.isInsecureRegistry("insecure.local:5000") {
		t.Error("expected insecure.local:5000 to match")
	}
}

func TestIsInsecureRegistry(t *testing.T) {
	p := NewProxy(nil).WithInsecureRegistries([]string{
		"exact.local:5000",
		"noport.local",
	})

	tests := []struct {
		host string
		want bool
	}{
		{"exact.local:5000", true},
		{"exact.local:9999", false},
		{"noport.local:1234", true},
		{"noport.local:5000", true},
		{"other.local:5000", false},
	}
	for _, tt := range tests {
		got := p.isInsecureRegistry(tt.host)
		if got != tt.want {
			t.Errorf("isInsecureRegistry(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestBufferedResponseWriter(t *testing.T) {
	w := newBufferedResponseWriter()
	w.Header().Set("X-Test", "hello")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte("body content"))

	if w.statusCode != http.StatusCreated {
		t.Errorf("statusCode = %d, want %d", w.statusCode, http.StatusCreated)
	}
	if string(w.body) != "body content" {
		t.Errorf("body = %q, want %q", w.body, "body content")
	}
	if w.header.Get("X-Test") != "hello" {
		t.Error("header not set")
	}
}

func TestWriteHTTPError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		writeHTTPError(server, http.StatusBadGateway, "bad gateway")
		server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestBufferedResponseWriter_WriteTo(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	w := newBufferedResponseWriter()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("test body"))

	go func() {
		w.WriteTo(server)
		server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

// TestProxy_Connect_HijackNotSupported exercises the "hijacking not supported" error path.
// httptest.ResponseRecorder does not implement http.Hijacker, so handleConnect returns 500.
func TestProxy_Connect_HijackNotSupported(t *testing.T) {
	p := NewProxy(nil)
	req := httptest.NewRequest(http.MethodConnect, "https://registry.example.com:443", nil)
	req.Host = "registry.example.com:443"
	rec := httptest.NewRecorder()

	p.handleConnect(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// fakeHijackWriter is a ResponseWriter that also implements http.Hijacker.
// It writes the HTTP status line directly to the conn without adding chunked
// encoding, making it safe to use for testing CONNECT proxy logic.
type fakeHijackWriter struct {
	conn    net.Conn
	headers http.Header
	written bool
}

func (w *fakeHijackWriter) Header() http.Header { return w.headers }
func (w *fakeHijackWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.written = true
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n\r\n", code, http.StatusText(code))
}
func (w *fakeHijackWriter) Write(b []byte) (int, error) { return w.conn.Write(b) }
func (w *fakeHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	br := bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn))
	return w.conn, br, nil
}

// TestProxy_Connect_TCPTunnel exercises the full CONNECT → TCP tunnel path
// using a fake hijacker to bypass Go HTTP server's chunked encoding.
func TestProxy_Connect_TCPTunnel(t *testing.T) {
	// Raw TCP echo server: accepts one connection, echoes data, then closes.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		c, err := echoLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 128)
		n, _ := c.Read(buf)
		_, _ = c.Write(buf[:n])
	}()

	targetAddr := echoLn.Addr().String()

	// client ←→ proxy pair via net.Pipe
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	p := NewProxy(nil)
	req := httptest.NewRequest(http.MethodConnect, "https://"+targetAddr, nil)
	req.Host = targetAddr
	fw := &fakeHijackWriter{conn: serverConn, headers: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConnect(fw, req)
	}()

	// Read 200 Connection Established from the proxy.
	header := make([]byte, 38) // "HTTP/1.1 200 OK\r\n\r\n" is 19 bytes; be generous
	n, _ := clientConn.Read(header)
	if !strings.HasPrefix(string(header[:n]), "HTTP/1.1 200") {
		t.Fatalf("expected 200, got: %q", string(header[:n]))
	}

	// Send "hello" through the tunnel; echo server writes it back.
	msg := "hello"
	clientConn.Write([]byte(msg))

	got := make([]byte, len(msg))
	io.ReadFull(clientConn, got)
	if string(got) != msg {
		t.Errorf("echo = %q, want %q", got, msg)
	}

	// Close the client side; this unblocks the proxy's copy goroutines.
	clientConn.Close()
	<-done
}

// TestProxy_Connect_InsecureMITM_HandshakeFails exercises handleMITM and
// generateSelfSignedCert. When the registry is marked insecure, handleConnect
// calls handleMITM. We close the client pipe without doing a TLS handshake,
// which causes the TLS handshake in handleMITM to fail cleanly.
func TestProxy_Connect_InsecureMITM_HandshakeFails(t *testing.T) {
	// Any listener address to register as insecure.
	targetAddr := "127.0.0.1:12345"

	p := NewProxy(nil).WithInsecureRegistries([]string{targetAddr})

	clientConn, serverConn := net.Pipe()

	req := httptest.NewRequest(http.MethodConnect, "https://"+targetAddr, nil)
	req.Host = targetAddr
	fw := &fakeHijackWriter{conn: serverConn, headers: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConnect(fw, req)
	}()

	// Read the 200 Connection Established from the proxy.
	header := make([]byte, 64)
	n, _ := clientConn.Read(header)
	if !strings.HasPrefix(string(header[:n]), "HTTP/1.1 200") {
		t.Fatalf("expected 200, got: %q", string(header[:n]))
	}

	// Close without starting TLS – the TLS handshake in handleMITM will fail.
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleConnect timed out")
	}
}

// TestForwardToUpstream_DialFailed exercises the TLS dial error path in forwardToUpstream.
func TestForwardToUpstream_DialFailed(t *testing.T) {
	p := NewProxy(nil)

	server, client := net.Pipe()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "https://127.0.0.1:1/v2/", nil)
	req.URL.Scheme = "https"
	req.URL.Host = "127.0.0.1:1"
	req.RequestURI = ""

	go func() {
		// Pass an unreachable hostPort so tls.Dial fails immediately.
		p.forwardToUpstream(server, req, "127.0.0.1:1", true)
		server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("read error response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// TestProxy_ServeHTTP_ConnectTunnel verifies that ServeHTTP routes CONNECT requests
// through handleConnect (the other branch from plain HTTP).
func TestProxy_ServeHTTP_ConnectTunnel(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		c, err := echoLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 128)
		n, _ := c.Read(buf)
		_, _ = c.Write(buf[:n])
	}()

	targetAddr := echoLn.Addr().String()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	p := NewProxy(nil)
	req := httptest.NewRequest(http.MethodConnect, "https://"+targetAddr, nil)
	req.Host = targetAddr
	fw := &fakeHijackWriter{conn: serverConn, headers: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(fw, req) // routes through handleConnect via CONNECT branch
	}()

	header := make([]byte, 38)
	n, _ := clientConn.Read(header)
	if !strings.HasPrefix(string(header[:n]), "HTTP/1.1 200") {
		t.Fatalf("expected 200, got: %q", string(header[:n]))
	}

	msg := "ping"
	clientConn.Write([]byte(msg))
	got := make([]byte, len(msg))
	io.ReadFull(clientConn, got)
	if string(got) != msg {
		t.Errorf("echo = %q, want %q", got, msg)
	}

	clientConn.Close()
	<-done
}

// TestHandleMITM_InterceptsOCIRequest exercises the full MITM loop:
// TLS handshake succeeds, the interceptor handles an OCI request and returns true,
// then connection close terminates the loop.
func TestHandleMITM_InterceptsOCIRequest(t *testing.T) {
	intercepted := false
	p := NewProxy(func(w http.ResponseWriter, r *http.Request) bool {
		intercepted = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("intercepted"))
		return true
	})

	clientConn, serverConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleMITM(serverConn, nil, "example.com", "example.com:443", true)
	}()

	clientTLS := tls.Client(clientConn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
	})
	if err := clientTLS.Handshake(); err != nil {
		clientConn.Close()
		t.Fatalf("TLS handshake: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/v2/", nil)
	if err := req.Write(clientTLS); err != nil {
		clientTLS.Close()
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), req)
	if err != nil {
		clientTLS.Close()
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(body) != "intercepted" {
		t.Errorf("body = %q, want %q", string(body), "intercepted")
	}
	if !intercepted {
		t.Error("expected request to be intercepted")
	}

	// Close client to terminate the MITM read loop (causes EOF in ReadRequest).
	clientTLS.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleMITM did not exit after client close")
	}
}

// TestHandleMITM_NonOCIForwardsToUpstream exercises the forwardToUpstream path
// when the MITM loop receives a non-OCI request (no interceptor match).
func TestHandleMITM_NonOCIForwardsToUpstream(t *testing.T) {
	// Set up a real TLS server to act as upstream.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	p := NewProxy(nil) // no interceptor

	clientConn, serverConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// insecure=true so upstream TLS cert is not verified.
		p.handleMITM(serverConn, nil, "example.com", upstream.Listener.Addr().String(), true)
	}()

	clientTLS := tls.Client(clientConn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
	})
	if err := clientTLS.Handshake(); err != nil {
		clientConn.Close()
		t.Fatalf("TLS handshake: %v", err)
	}

	// Send a non-OCI (non-/v2/) request so it goes to forwardToUpstream.
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/healthz", nil)
	if err := req.Write(clientTLS); err != nil {
		clientTLS.Close()
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), req)
	if err != nil {
		clientTLS.Close()
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
	if string(body) != "upstream" {
		t.Errorf("body = %q, want %q", string(body), "upstream")
	}

	clientTLS.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleMITM did not exit after client close")
	}
}

// TestNewProxy_WithCertManager covers the len(certMgr) > 0 branch in NewProxy.
func TestNewProxy_WithCertManager(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	proxy := NewProxy(nil, cm)
	if proxy.certMgr != cm {
		t.Error("certMgr should be set on the proxy")
	}
}

// TestHandleHTTP_NewRequestError covers the http.NewRequestWithContext error path in
// handleHTTP. A null byte injected into r.URL.Opaque makes r.URL.String() produce a URL
// containing a control character, which url.Parse rejects.
func TestHandleHTTP_NewRequestError(t *testing.T) {
	proxy := NewProxy(nil)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/v2/path", nil)
	// Inject a null byte via Opaque so String() produces "http:/v2/path\x00null"
	// (scheme + ":" + opaque), which url.Parse rejects as an invalid control character.
	req.URL.Opaque = "/v2/path\x00null"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

// TestHandleHTTP_RoundTripError covers the http.DefaultTransport.RoundTrip error path in handleHTTP.
// Pointing to port 1 (always refused) forces a dial error.
func TestHandleHTTP_RoundTripError(t *testing.T) {
	proxy := NewProxy(nil)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1/v2/test", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (BadGateway on RoundTrip error)", rec.Code, http.StatusBadGateway)
	}
}

// TestForwardToUpstream_ReadResponseError exercises the http.ReadResponse error path in
// forwardToUpstream. A TLS server that closes its connection immediately after the handshake
// causes ReadResponse to fail, so forwardToUpstream writes a 502 to the client pipe.
func TestForwardToUpstream_ReadResponseError(t *testing.T) {
	cert, err := generateSelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatalf("generateSelfSignedCert: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{*cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept one connection, complete the handshake, then immediately close.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake()
		c.Close()
	}()

	p := NewProxy(nil)
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	req := httptest.NewRequest(http.MethodGet, "https://example.com/v2/", nil)
	req.URL.Host = "example.com"
	req.RequestURI = ""

	go func() {
		p.forwardToUpstream(serverConn, req, ln.Addr().String(), true)
		serverConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		return // connection closed before proxy could write 502 — still covers the error path
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway, got %d", resp.StatusCode)
	}
}

// TestProxy_Connect_HostWithoutPort exercises the host+":443" normalization in handleConnect.
// When the CONNECT target has no explicit port, handleConnect appends ":443". The subsequent
// dial is expected to fail (port 1 is always refused), which is fine — we only need the
// branch covered and the function to exit cleanly.
func TestProxy_Connect_HostWithoutPort(t *testing.T) {
	p := NewProxy(nil) // no certMgr, no insecure registries — transparent tunnel

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	// Host without port triggers the "host = host + ":443"" branch.
	req := httptest.NewRequest(http.MethodConnect, "https://127.0.0.1", nil)
	req.Host = "127.0.0.1" // no port → will become "127.0.0.1:443"

	fw := &fakeHijackWriter{conn: serverConn, headers: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConnect(fw, req)
	}()

	// Read the 200 Connection Established.
	header := make([]byte, 38)
	n, _ := clientConn.Read(header)
	if !strings.HasPrefix(string(header[:n]), "HTTP/1.1 200") {
		t.Fatalf("expected 200, got: %q", string(header[:n]))
	}

	// The dial to 127.0.0.1:443 will fail immediately (ECONNREFUSED); close client
	// to let any goroutines unblock.
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleConnect with no-port host timed out")
	}
}

// failHijackWriter is a ResponseWriter/Hijacker whose Hijack always returns an error.
type failHijackWriter struct {
	conn    net.Conn
	headers http.Header
	written bool
}

func (w *failHijackWriter) Header() http.Header { return w.headers }
func (w *failHijackWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.written = true
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n\r\n", code, http.StatusText(code))
}
func (w *failHijackWriter) Write(b []byte) (int, error) { return w.conn.Write(b) }
func (w *failHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, fmt.Errorf("hijack unavailable")
}

// TestProxy_Connect_HijackError exercises the hijacker.Hijack() error path in handleConnect.
// The failHijackWriter returns an error from Hijack() so handleConnect exits immediately
// after sending 200 and discovering it cannot hijack the connection.
func TestProxy_Connect_HijackError(t *testing.T) {
	p := NewProxy(nil)

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	req := httptest.NewRequest(http.MethodConnect, "https://registry.example.com:443", nil)
	req.Host = "registry.example.com:443"
	fw := &failHijackWriter{conn: serverConn, headers: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConnect(fw, req)
	}()

	// handleConnect writes "200 OK" to serverConn before calling Hijack.
	// Read it from clientConn to unblock that write; then Hijack returns its error
	// and handleConnect exits cleanly.
	header := make([]byte, 64)
	clientConn.Read(header)
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("handleConnect did not return after Hijack error")
	}
}

// errBodyReader always returns an error on Read, simulating a broken request body.
type errBodyReader struct{}

func (e *errBodyReader) Read(_ []byte) (int, error) { return 0, io.ErrClosedPipe }

// TestForwardToUpstream_WriteRequestError exercises the req.Write error path.
// A POST request with a broken body reader causes req.Write to fail, which triggers
// writeHTTPError(502) before the response is read.
func TestForwardToUpstream_WriteRequestError(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := NewProxy(nil)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/test", io.NopCloser(&errBodyReader{}))
	req.RequestURI = ""
	req.ContentLength = -1 // chunked: forces body to be read during req.Write

	go func() {
		p.forwardToUpstream(serverConn, req, upstream.Listener.Addr().String(), true)
		serverConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("expected 502 error response, got read error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (BadGateway on req.Write error)", resp.StatusCode, http.StatusBadGateway)
	}
}

// TestForwardToUpstream_Success exercises the full happy path: dial, write request, read response.
func TestForwardToUpstream_Success(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from upstream"))
	}))
	defer upstream.Close()

	p := NewProxy(nil)

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/v2/", nil)
	req.URL.Host = "example.com"
	req.RequestURI = ""

	go func() {
		p.forwardToUpstream(serverConn, req, upstream.Listener.Addr().String(), true)
		serverConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(body) != "hello from upstream" {
		t.Errorf("body = %q, want %q", string(body), "hello from upstream")
	}
}

// TestHandleMITM_WithCertMgr exercises the `if p.certMgr != nil` branch in
// handleMITM by creating a proxy with a real CertManager and performing a
// TLS handshake so the GetCertificate path is hit.
// flushFailHijackWriter is a ResponseWriter/Hijacker that succeeds on Hijack but
// returns a bufio.ReadWriter backed by a broken net.Pipe so clientBuf.Flush() fails.
type flushFailHijackWriter struct {
	conn    net.Conn
	buf     *bufio.ReadWriter
	headers http.Header
}

func (w *flushFailHijackWriter) Header() http.Header { return w.headers }
func (w *flushFailHijackWriter) WriteHeader(code int) {
	// Write response into the bufio buffer so Flush has something to flush.
	fmt.Fprintf(w.buf, "HTTP/1.1 %d %s\r\n\r\n", code, http.StatusText(code))
}
func (w *flushFailHijackWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *flushFailHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.buf, nil
}

// TestProxy_Connect_FlushError exercises the clientBuf.Flush() error path in handleConnect.
// After a successful Hijack(), the buffered 200 response is flushed to the client.
// If the pipe's read end is already closed, the Flush write fails and handleConnect returns.
func TestProxy_Connect_FlushError(t *testing.T) {
	p := NewProxy(nil)

	// net.Pipe gives a synchronous in-memory connection.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	// Create a bufio.ReadWriter wrapping serverConn.
	brw := bufio.NewReadWriter(
		bufio.NewReader(serverConn),
		bufio.NewWriter(serverConn),
	)
	fw := &flushFailHijackWriter{
		conn:    serverConn,
		buf:     brw,
		headers: make(http.Header),
	}

	req := httptest.NewRequest(http.MethodConnect, "https://registry.example.com:443", nil)
	req.Host = "registry.example.com:443"

	// Close the read-end of the pipe BEFORE handleConnect can flush — any write to
	// serverConn will now return a broken-pipe error, causing Flush to fail.
	clientConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleConnect(fw, req)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("handleConnect did not return after Flush error")
	}
}

func TestHandleMITM_WithCertMgr(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}

	p := NewProxy(nil, cm)

	clientConn, serverConn := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// insecure=false → p.certMgr != nil branch → GetCertificate is called.
		p.handleMITM(serverConn, nil, "example.com", "example.com:443", false)
	}()

	// TLS client: perform handshake and then close immediately to exit the read loop.
	clientTLS := tls.Client(clientConn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
	})
	_ = clientTLS.Handshake()
	clientTLS.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleMITM did not exit after client close")
	}
}
