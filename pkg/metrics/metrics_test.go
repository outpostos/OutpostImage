package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newTestMetrics creates a Metrics instance registered with a custom registry
// to avoid conflicts with the default global registry.
func newTestMetrics() (*Metrics, *prometheus.Registry) {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_http_requests_total"},
			[]string{"server", "method", "path"},
		),
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "test_http_request_duration_seconds", Buckets: []float64{0.01, 0.1, 1.0}},
			[]string{"server", "method", "path"},
		),
		HTTPResponsesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_http_responses_total"},
			[]string{"server", "method", "path", "status"},
		),
		PeerHealthStatus: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "test_peer_health_status"},
			[]string{"peer"},
		),
		RouterLookupsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_router_lookups_total"},
			[]string{"result"},
		),
		RouterLookupDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "test_router_lookup_duration_seconds", Buckets: []float64{0.01, 0.1, 1.0}},
			[]string{},
		),
		RouterBalancerPeers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "test_router_balancer_peers"},
			[]string{"digest"},
		),
		StorageDigestsGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{Name: "test_storage_digests"},
		),
		StorageImagesGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{Name: "test_storage_images"},
		),
		ProxyRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_proxy_requests_total"},
			[]string{"method"},
		),
		ProxyInterceptedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_proxy_intercepted_total"},
			[]string{"type"},
		),
		ProxyUpstreamTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_proxy_upstream_total"},
			[]string{"type"},
		),
		MDNSPeersDiscoveredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_mdns_peers_discovered_total"},
		),
		PeerLatencySeconds: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "test_peer_latency_seconds"},
			[]string{"peer"},
		),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.HTTPResponsesTotal,
		m.PeerHealthStatus, m.RouterLookupsTotal, m.RouterLookupDuration,
		m.RouterBalancerPeers, m.StorageDigestsGauge, m.StorageImagesGauge,
		m.ProxyRequestsTotal, m.ProxyInterceptedTotal, m.ProxyUpstreamTotal,
		m.MDNSPeersDiscoveredTotal, m.PeerLatencySeconds,
	)

	return m, reg
}

func TestObserveHTTPRequest(t *testing.T) {
	m, reg := newTestMetrics()

	start := time.Now().Add(-50 * time.Millisecond)
	m.ObserveHTTPRequest("registry", "GET", "/v2/blobs", start, 200)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, mf := range mfs {
		found[mf.GetName()] = true
	}

	for _, name := range []string{"test_http_requests_total", "test_http_request_duration_seconds", "test_http_responses_total"} {
		if !found[name] {
			t.Errorf("metric %s not found after ObserveHTTPRequest", name)
		}
	}
}

func TestSetPeerHealth(t *testing.T) {
	m, reg := newTestMetrics()

	m.SetPeerHealth("10.0.0.2:5000", true)
	m.SetPeerHealth("10.0.0.3:5000", false)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		if mf.GetName() != "test_peer_health_status" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			peer := metric.GetLabel()[0].GetValue()
			val := metric.GetGauge().GetValue()
			switch peer {
			case "10.0.0.2:5000":
				if val != 1.0 {
					t.Errorf("healthy peer should be 1.0, got %f", val)
				}
			case "10.0.0.3:5000":
				if val != 0.0 {
					t.Errorf("unhealthy peer should be 0.0, got %f", val)
				}
			}
		}
	}
}

func TestRecordRouterLookup(t *testing.T) {
	m, reg := newTestMetrics()

	m.RecordRouterLookup("found", 100*time.Millisecond)
	m.RecordRouterLookup("not_found", 50*time.Millisecond)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	var lookupCount float64
	for _, mf := range mfs {
		if mf.GetName() == "test_router_lookups_total" {
			for _, metric := range mf.GetMetric() {
				lookupCount += metric.GetCounter().GetValue()
			}
		}
	}
	if lookupCount != 2 {
		t.Errorf("expected 2 lookups, got %f", lookupCount)
	}
}

func TestSetDigestAndImageCount(t *testing.T) {
	m, reg := newTestMetrics()

	m.SetDigestCount(42)
	m.SetImageCount(15)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		switch mf.GetName() {
		case "test_storage_digests":
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 42 {
				t.Errorf("digest count: expected 42, got %f", v)
			}
		case "test_storage_images":
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 15 {
				t.Errorf("image count: expected 15, got %f", v)
			}
		}
	}
}

func TestProxyMetrics(t *testing.T) {
	m, reg := newTestMetrics()

	m.RecordProxyRequest("GET")
	m.RecordProxyRequest("CONNECT")
	m.RecordProxyIntercept("blob")
	m.RecordProxyIntercept("manifest")
	m.RecordProxyUpstream("blob")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	counts := map[string]float64{}
	for _, mf := range mfs {
		var total float64
		for _, metric := range mf.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
		counts[mf.GetName()] = total
	}

	if counts["test_proxy_requests_total"] != 2 {
		t.Errorf("proxy requests: expected 2, got %f", counts["test_proxy_requests_total"])
	}
	if counts["test_proxy_intercepted_total"] != 2 {
		t.Errorf("proxy intercepted: expected 2, got %f", counts["test_proxy_intercepted_total"])
	}
	if counts["test_proxy_upstream_total"] != 1 {
		t.Errorf("proxy upstream: expected 1, got %f", counts["test_proxy_upstream_total"])
	}
}

func TestMetricsHandler(t *testing.T) {
	_, reg := newTestMetrics()

	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "test_storage_digests") {
		t.Error("metrics handler should include registered metrics")
	}
}
