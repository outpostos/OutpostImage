package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics for OutpostImage.
type Metrics struct {
	// HTTP request metrics
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec
	HTTPResponsesTotal   *prometheus.CounterVec

	// P2P routing metrics
	PeerHealthStatus        *prometheus.GaugeVec
	RouterLookupsTotal      *prometheus.CounterVec
	RouterLookupDuration    *prometheus.HistogramVec
	RouterBalancerPeers     *prometheus.GaugeVec

	// Storage metrics
	StorageDigestsGauge     prometheus.Gauge
	StorageImagesGauge      prometheus.Gauge

	// Proxy metrics
	ProxyRequestsTotal      *prometheus.CounterVec
	ProxyInterceptedTotal   *prometheus.CounterVec
	ProxyUpstreamTotal      *prometheus.CounterVec

	// mDNS metrics
	MDNSPeersDiscoveredTotal prometheus.Counter

	// Peer latency metrics
	PeerLatencySeconds *prometheus.GaugeVec
}

// DefaultMetrics returns the default metrics registry.
var Default = New()

// New creates a new Metrics instance.
func New() *Metrics {
	m := &Metrics{
		// HTTP request metrics
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_http_requests_total",
				Help: "Total number of HTTP requests received.",
			},
			[]string{"server", "method", "path"},
		),
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "outpostimage_http_request_duration_seconds",
				Help:    "HTTP request latency in seconds.",
				Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0},
			},
			[]string{"server", "method", "path"},
		),
		HTTPResponsesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_http_responses_total",
				Help: "Total number of HTTP responses sent.",
			},
			[]string{"server", "method", "path", "status"},
		),

		// P2P routing metrics
		PeerHealthStatus: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "outpostimage_peer_health_status",
				Help: "Peer health status (1=healthy, 0=unhealthy).",
			},
			[]string{"peer"},
		),
		RouterLookupsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_router_lookups_total",
				Help: "Total number of P2P digest lookups.",
			},
			[]string{"result"}, // found, not_found, error
		),
		RouterLookupDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "outpostimage_router_lookup_duration_seconds",
				Help:    "P2P digest lookup latency in seconds.",
				Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 5.0},
			},
			[]string{},
		),
		RouterBalancerPeers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "outpostimage_router_balancer_peers",
				Help: "Number of peers in the balancer for a digest.",
			},
			[]string{"digest"},
		),

		// Storage metrics
		StorageDigestsGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "outpostimage_storage_digests",
				Help: "Total number of unique digests tracked.",
			},
		),
		StorageImagesGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "outpostimage_storage_images",
				Help: "Total number of images in the store.",
			},
		),

		// Proxy metrics
		ProxyRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_proxy_requests_total",
				Help: "Total number of proxy requests received.",
			},
			[]string{"method"},
		),
		ProxyInterceptedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_proxy_intercepted_total",
				Help: "Number of OCI requests intercepted and served locally.",
			},
			[]string{"type"}, // blob, manifest
		),
		ProxyUpstreamTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outpostimage_proxy_upstream_total",
				Help: "Number of requests forwarded to upstream registry.",
			},
			[]string{"type"}, // blob, manifest
		),

		// mDNS metrics
		MDNSPeersDiscoveredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "outpostimage_mdns_peers_discovered_total",
				Help: "Total number of peers discovered via mDNS.",
			},
		),

		// Peer latency metrics
		PeerLatencySeconds: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "outpostimage_peer_latency_seconds",
				Help: "EWMA latency to peer in seconds.",
			},
			[]string{"peer"},
		),
	}

	// Register all metrics with the default registry.
	prometheus.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.HTTPResponsesTotal,
		m.PeerHealthStatus,
		m.RouterLookupsTotal,
		m.RouterLookupDuration,
		m.RouterBalancerPeers,
		m.StorageDigestsGauge,
		m.StorageImagesGauge,
		m.ProxyRequestsTotal,
		m.ProxyInterceptedTotal,
		m.ProxyUpstreamTotal,
		m.MDNSPeersDiscoveredTotal,
		m.PeerLatencySeconds,
	)

	return m
}

// Handler returns an HTTP handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

// ObserveHTTPRequest records an HTTP request.
func (m *Metrics) ObserveHTTPRequest(server, method, path string, start time.Time, statusCode int) {
	duration := time.Since(start).Seconds()
	m.HTTPRequestsTotal.WithLabelValues(server, method, path).Inc()
	m.HTTPRequestDuration.WithLabelValues(server, method, path).Observe(duration)
	m.HTTPResponsesTotal.WithLabelValues(server, method, path, strconv.Itoa(statusCode)).Inc()
}

// SetPeerHealth sets the health status of a peer.
func (m *Metrics) SetPeerHealth(peer string, healthy bool) {
	value := 0.0
	if healthy {
		value = 1.0
	}
	m.PeerHealthStatus.WithLabelValues(peer).Set(value)
}

// RecordRouterLookup records a P2P digest lookup result.
func (m *Metrics) RecordRouterLookup(result string, duration time.Duration) {
	m.RouterLookupsTotal.WithLabelValues(result).Inc()
	m.RouterLookupDuration.WithLabelValues().Observe(duration.Seconds())
}

// SetDigestCount sets the total digest count.
func (m *Metrics) SetDigestCount(count int) {
	m.StorageDigestsGauge.Set(float64(count))
}

// SetImageCount sets the total image count.
func (m *Metrics) SetImageCount(count int) {
	m.StorageImagesGauge.Set(float64(count))
}

// RecordProxyRequest records a proxy request.
func (m *Metrics) RecordProxyRequest(method string) {
	m.ProxyRequestsTotal.WithLabelValues(method).Inc()
}

// RecordProxyIntercept records an intercepted OCI request.
func (m *Metrics) RecordProxyIntercept(reqType string) {
	m.ProxyInterceptedTotal.WithLabelValues(reqType).Inc()
}

// RecordProxyUpstream records a request forwarded to upstream.
func (m *Metrics) RecordProxyUpstream(reqType string) {
	m.ProxyUpstreamTotal.WithLabelValues(reqType).Inc()
}

// Handler returns the default metrics handler.
func Handler() http.Handler {
	return Default.Handler()
}
