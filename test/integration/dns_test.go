package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/outpostos/outpostimage/pkg/dns"

	mdns "github.com/miekg/dns"
)

// TestDNSHijackE2E tests the embedded DNS server hijacking registry domains.
func TestDNSHijackE2E(t *testing.T) {
	localIP := net.ParseIP("10.0.0.99")

	srv, err := dns.NewServer(dns.ServerConfig{
		Addr:          "127.0.0.1:15354",
		LocalIP:       localIP,
		HijackDomains: []string{"docker.io", "registry-1.docker.io", "ghcr.io", "registry.k8s.io", "quay.io"},
		UpstreamDNS:   "8.8.8.8:53",
		TTL:           30,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	client := new(mdns.Client)
	dnsAddr := "127.0.0.1:15354"

	// Test 1: Hijacked domain A record
	t.Run("HijackA_DockerIO", func(t *testing.T) {
		msg := new(mdns.Msg)
		msg.SetQuestion("docker.io.", mdns.TypeA)

		resp, _, err := client.Exchange(msg, dnsAddr)
		if err != nil {
			t.Fatal(err)
		}

		if len(resp.Answer) != 1 {
			t.Fatalf("expected 1 A answer, got %d", len(resp.Answer))
		}

		a, ok := resp.Answer[0].(*mdns.A)
		if !ok {
			t.Fatalf("expected A record, got %T", resp.Answer[0])
		}
		if !a.A.Equal(localIP) {
			t.Errorf("A record = %s, want %s", a.A, localIP)
		}
		if a.Hdr.Ttl != 30 {
			t.Errorf("TTL = %d, want 30", a.Hdr.Ttl)
		}
		t.Logf("docker.io A → %s (TTL %d)", a.A, a.Hdr.Ttl)
	})

	// Test 2: All registry domains are hijacked
	t.Run("HijackA_AllRegistries", func(t *testing.T) {
		domains := []string{"ghcr.io.", "registry.k8s.io.", "quay.io.", "registry-1.docker.io."}

		for _, domain := range domains {
			msg := new(mdns.Msg)
			msg.SetQuestion(domain, mdns.TypeA)

			resp, _, err := client.Exchange(msg, dnsAddr)
			if err != nil {
				t.Fatalf("%s query failed: %v", domain, err)
			}

			if len(resp.Answer) != 1 {
				t.Errorf("%s: expected 1 answer, got %d", domain, len(resp.Answer))
				continue
			}

			a, ok := resp.Answer[0].(*mdns.A)
			if !ok {
				t.Errorf("%s: expected A record", domain)
				continue
			}
			if !a.A.Equal(localIP) {
				t.Errorf("%s: A = %s, want %s", domain, a.A, localIP)
			}
			t.Logf("%s A → %s", domain, a.A)
		}
	})

	// Test 3: AAAA for hijacked domain should be empty (force IPv4)
	t.Run("HijackAAAA_Empty", func(t *testing.T) {
		msg := new(mdns.Msg)
		msg.SetQuestion("docker.io.", mdns.TypeAAAA)

		resp, _, err := client.Exchange(msg, dnsAddr)
		if err != nil {
			t.Fatal(err)
		}

		if len(resp.Answer) != 0 {
			t.Errorf("expected 0 AAAA answers for hijacked domain, got %d", len(resp.Answer))
		}
		t.Log("docker.io AAAA → empty (forces IPv4)")
	})

	// Test 4: Non-hijacked domain should NOT be hijacked
	t.Run("NonHijacked_PassThrough", func(t *testing.T) {
		if srv.IsHijacked("example.com") {
			t.Error("example.com should not be hijacked")
		}
		if srv.IsHijacked("google.com") {
			t.Error("google.com should not be hijacked")
		}
		if !srv.IsHijacked("docker.io") {
			t.Error("docker.io should be hijacked")
		}
		t.Log("Non-hijacked domains correctly excluded")
	})

	// Test 5: DNS config generation
	t.Run("ConfigGen_DNSMasq", func(t *testing.T) {
		config := dns.GenerateDNSMasqConfig(":5353", []string{"docker.io", "ghcr.io"})
		if len(config) == 0 {
			t.Fatal("empty dnsmasq config")
		}
		t.Logf("dnsmasq config generated (%d bytes)", len(config))
	})

	t.Run("ConfigGen_CoreDNS", func(t *testing.T) {
		config := dns.GenerateCoreDNSConfig("127.0.0.1:5353", []string{"docker.io", "ghcr.io"})
		if len(config) == 0 {
			t.Fatal("empty CoreDNS config")
		}
		t.Logf("CoreDNS config generated (%d bytes)", len(config))
	})

	t.Logf("=== DNS Hijack E2E Report ===")
	t.Logf("Hijacked domains: docker.io, registry-1.docker.io, ghcr.io, registry.k8s.io, quay.io")
	t.Logf("Local IP: %s", localIP)
	t.Logf("TTL: 30s")
	t.Logf("AAAA suppression: enabled")
}

// TestDNSServerLifecycle tests server startup and graceful shutdown.
func TestDNSServerLifecycle(t *testing.T) {
	srv, err := dns.NewServer(dns.ServerConfig{
		Addr:    "127.0.0.1:15355",
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

	// Verify server is responding
	client := new(mdns.Client)
	msg := new(mdns.Msg)
	msg.SetQuestion("docker.io.", mdns.TypeA)

	resp, _, err := client.Exchange(msg, "127.0.0.1:15355")
	if err != nil {
		t.Fatalf("query failed while server running: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatal("expected answer while server running")
	}

	// Graceful shutdown
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down in time")
	}

	t.Log("DNS server lifecycle: started → served queries → graceful shutdown")
}
