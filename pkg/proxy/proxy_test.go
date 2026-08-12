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
