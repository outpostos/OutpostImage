package dns

import (
	"context"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func TestNewServer_Defaults(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		LocalIP: net.ParseIP("127.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if srv.config.Addr != ":5353" {
		t.Errorf("Addr = %q, want %q", srv.config.Addr, ":5353")
	}
	if srv.config.TTL != 30 {
		t.Errorf("TTL = %d, want %d", srv.config.TTL, 30)
	}
	if srv.config.UpstreamDNS != "8.8.8.8:53" {
		t.Errorf("UpstreamDNS = %q, want %q", srv.config.UpstreamDNS, "8.8.8.8:53")
	}
	if len(srv.hijackSet) != len(DefaultHijackDomains) {
		t.Errorf("hijackSet size = %d, want %d", len(srv.hijackSet), len(DefaultHijackDomains))
	}
}

func TestServer_IsHijacked(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		LocalIP:       net.ParseIP("10.0.0.1"),
		HijackDomains: []string{"docker.io", "ghcr.io"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		domain string
		want   bool
	}{
		{"docker.io", true},
		{"docker.io.", true},
		{"DOCKER.IO", true},
		{"ghcr.io", true},
		{"example.com", false},
		{"notdocker.io", false},
	}

	for _, tt := range tests {
		if got := srv.IsHijacked(tt.domain); got != tt.want {
			t.Errorf("IsHijacked(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

func TestServer_HijackResponse(t *testing.T) {
	localIP := net.ParseIP("10.0.0.1")
	srv, err := NewServer(ServerConfig{
		Addr:          ":0", // random port
		LocalIP:       localIP,
		HijackDomains: []string{"docker.io"},
		TTL:           30,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Start server on random port
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	udpServer := &mdns.Server{
		PacketConn: pc,
		Handler:    mdns.HandlerFunc(srv.handleDNS),
	}

	go udpServer.ActivateAndServe()
	defer udpServer.Shutdown()

	time.Sleep(50 * time.Millisecond)

	// Query for hijacked domain
	client := new(mdns.Client)
	msg := new(mdns.Msg)
	msg.SetQuestion("docker.io.", mdns.TypeA)

	resp, _, err := client.Exchange(msg, pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(resp.Answer))
	}

	aRecord, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatalf("expected A record, got %T", resp.Answer[0])
	}

	if !aRecord.A.Equal(localIP) {
		t.Errorf("A record = %s, want %s", aRecord.A, localIP)
	}

	if aRecord.Hdr.Ttl != 30 {
		t.Errorf("TTL = %d, want 30", aRecord.Hdr.Ttl)
	}
}

func TestServer_AAAA_Hijacked_Empty(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Addr:          ":0",
		LocalIP:       net.ParseIP("10.0.0.1"),
		HijackDomains: []string{"docker.io"},
	})
	if err != nil {
		t.Fatal(err)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	udpServer := &mdns.Server{
		PacketConn: pc,
		Handler:    mdns.HandlerFunc(srv.handleDNS),
	}

	go udpServer.ActivateAndServe()
	defer udpServer.Shutdown()

	time.Sleep(50 * time.Millisecond)

	client := new(mdns.Client)
	msg := new(mdns.Msg)
	msg.SetQuestion("docker.io.", mdns.TypeAAAA)

	resp, _, err := client.Exchange(msg, pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	// AAAA for hijacked domain should return empty (force IPv4)
	if len(resp.Answer) != 0 {
		t.Errorf("expected 0 AAAA answers for hijacked domain, got %d", len(resp.Answer))
	}
}

func TestServer_RunAndShutdown(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		Addr:    "127.0.0.1:15353",
		LocalIP: net.ParseIP("10.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Run() did not return after cancel")
	}
}
