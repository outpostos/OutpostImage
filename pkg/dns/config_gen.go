package dns

import (
	"fmt"
	"strings"
)

// GenerateDNSMasqConfig generates a dnsmasq configuration snippet
// that forwards the hijacked domains to the OutpostImage DNS server.
func GenerateDNSMasqConfig(dnsAddr string, domains []string) string {
	var b strings.Builder
	b.WriteString("# OutpostImage DNS hijack configuration for dnsmasq\n")
	b.WriteString("# Add this to /etc/dnsmasq.d/outpostimage.conf\n\n")

	// Extract host:port
	host, port := parseDNSAddr(dnsAddr)

	for _, d := range domains {
		fmt.Fprintf(&b, "server=/%s/%s#%s\n", d, host, port)
	}
	return b.String()
}

// GenerateCoreDNSConfig generates a CoreDNS configuration snippet
// that forwards the hijacked domains to the OutpostImage DNS server.
func GenerateCoreDNSConfig(dnsAddr string, domains []string) string {
	var b strings.Builder
	b.WriteString("# OutpostImage DNS hijack configuration for CoreDNS\n")
	b.WriteString("# Add this block to your Corefile\n\n")

	for _, d := range domains {
		fmt.Fprintf(&b, "%s {\n", d)
		fmt.Fprintf(&b, "    forward . %s\n", dnsAddr)
		fmt.Fprintf(&b, "}\n\n")
	}
	return b.String()
}

// parseDNSAddr extracts host and port from an address like ":5353" or "127.0.0.1:5353".
func parseDNSAddr(addr string) (string, string) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "53"
	}
	host := addr[:idx]
	port := addr[idx+1:]
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port
}
