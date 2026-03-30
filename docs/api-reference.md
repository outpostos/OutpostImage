# OutpostImage API Reference

OutpostImage exposes three HTTP servers:

| Server | Default Port | Purpose |
|--------|-------------|---------|
| Registry | `:5000` | OCI Distribution API + P2P digest query + health |
| Proxy | `:3128` | HTTP/HTTPS proxy (not directly called by users) |
| Metrics | `:9090` | Prometheus metrics + health checks |

---

## OCI Distribution API (`:5000`)

Implements the [OCI Distribution Spec v1](https://github.com/opencontainers/distribution-spec) for read-only operations. This allows container runtimes and tools like `docker pull`, `crane`, and `skopeo` to pull images directly from a peer node.

### `GET /v2/`

OCI version check. Returns `200 OK` if the registry is ready to serve requests.

**Response:**

```http
HTTP/1.1 200 OK
Content-Type: application/json
Docker-Distribution-API-Version: registry/2.0

{}
```

---

### `GET /v2/{name}/manifests/{reference}`

Fetch a manifest by tag or digest.

- `name` — image name, may contain slashes (e.g. `library/nginx`)
- `reference` — tag (e.g. `latest`) or digest (e.g. `sha256:abc123...`)

**Response (success):**

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.docker.distribution.manifest.v2+json
Docker-Content-Digest: sha256:abc123...

{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": { ... },
  "layers": [ ... ]
}
```

**Response (not found):**

```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "errors": [
    {
      "code": "MANIFEST_UNKNOWN",
      "message": "manifest unknown to registry",
      "detail": "library/nginx:latest"
    }
  ]
}
```

---

### `HEAD /v2/{name}/blobs/{digest}`

Check whether a blob exists on this node without downloading it.

- `digest` — blob digest in the format `sha256:<hex>`

**Response (exists):**

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Docker-Content-Digest: sha256:abc123...
Content-Length: 1234567
```

**Response (not found):**

```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "errors": [
    {
      "code": "BLOB_UNKNOWN",
      "message": "blob unknown to registry",
      "detail": "sha256:abc123..."
    }
  ]
}
```

---

### `GET /v2/{name}/blobs/{digest}`

Download a blob (image layer or config) by digest.

**Response (success):**

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Docker-Content-Digest: sha256:abc123...
Content-Length: 1234567

<binary blob data>
```

**Response (not found):**

```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "errors": [
    {
      "code": "BLOB_UNKNOWN",
      "message": "blob unknown to registry",
      "detail": "sha256:abc123..."
    }
  ]
}
```

---

## P2P Internal API (`:5000`)

These endpoints are used by OutpostImage nodes to query each other's local digest inventory. They are typically called by the proxy's routing logic, but can also be used for monitoring and debugging.

### `GET /api/v1/digests`

List all digests known to this node (from the local Docker/containerd store).

**Response:**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "digests": [
    "sha256:abc123...",
    "sha256:def456...",
    "sha256:789ghi..."
  ],
  "count": 3
}
```

---

### `GET /api/v1/digests/{digest}`

Check whether a specific digest exists on this node.

- `digest` — full digest string (`sha256:<hex>`) or just the hex part (algorithm defaults to `sha256`)

**Response (exists):**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "digest": "sha256:abc123...",
  "exists": true
}
```

**Response (not found):**

```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "digest": "sha256:abc123...",
  "exists": false
}
```

**Response (invalid digest format):**

```http
HTTP/1.1 400 Bad Request
Content-Type: application/json

{
  "error": "invalid digest format"
}
```

---

## Health Check Endpoints

Available on both the registry server (`:5000`) and the metrics server (`:9090`).

### `GET /healthz`

Liveness probe. Returns `200 OK` as long as the process is running.

```http
HTTP/1.1 200 OK
Content-Type: text/plain

OK
```

### `GET /readyz`

Readiness probe. Returns `200 OK` when the service is ready to handle requests (store initialized and indexed).

```http
HTTP/1.1 200 OK
Content-Type: text/plain

OK
```

Use `/readyz` for Kubernetes readiness probes and `/healthz` for liveness probes.

---

## Prometheus Metrics (`:9090/metrics`)

Exposed in [Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/) at `GET /metrics`.

### HTTP Request Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `outpostimage_http_requests_total` | Counter | `server`, `method`, `path` | Total HTTP requests received |
| `outpostimage_http_request_duration_seconds` | Histogram | `server`, `method`, `path` | HTTP request latency (buckets: 1ms–1s) |
| `outpostimage_http_responses_total` | Counter | `server`, `method`, `path`, `status` | Total HTTP responses sent |

**Labels:**
- `server`: `registry`, `proxy`, or `metrics`
- `method`: HTTP method (`GET`, `HEAD`, `POST`, `CONNECT`)
- `path`: request path
- `status`: HTTP status code

### P2P Routing Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `outpostimage_router_lookups_total` | Counter | `result` | Total P2P digest lookups |
| `outpostimage_router_lookup_duration_seconds` | Histogram | — | P2P lookup latency (buckets: 10ms–5s) |
| `outpostimage_peer_health_status` | Gauge | `peer` | Peer health (1=healthy, 0=unhealthy) |
| `outpostimage_router_balancer_peers` | Gauge | `digest` | Number of peers serving a digest |
| `outpostimage_peer_latency_seconds` | Gauge | `peer` | EWMA round-trip latency to peer |

**`result` label values:**
- `found` — digest located on a P2P peer
- `not_found` — no peer has this digest
- `error` — lookup failed due to network/timeout error

### Storage Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `outpostimage_storage_digests` | Gauge | — | Total unique digests in the local store |
| `outpostimage_storage_images` | Gauge | — | Total images in the local store |

### Proxy Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `outpostimage_proxy_requests_total` | Counter | `method` | Total proxy requests received |
| `outpostimage_proxy_intercepted_total` | Counter | `type` | OCI requests served locally (P2P hit) |
| `outpostimage_proxy_upstream_total` | Counter | `type` | Requests forwarded to upstream registry |

**`type` label values:**
- `blob` — layer blob request
- `manifest` — manifest request

**P2P hit rate** (PromQL):
```promql
rate(outpostimage_proxy_intercepted_total[5m])
  /
(rate(outpostimage_proxy_intercepted_total[5m]) + rate(outpostimage_proxy_upstream_total[5m]))
```

### mDNS Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `outpostimage_mdns_peers_discovered_total` | Counter | — | Cumulative peers discovered via mDNS |

### Example: Query all metrics

```bash
curl http://localhost:9090/metrics
```

Example output:

```
# HELP outpostimage_proxy_intercepted_total Number of OCI requests intercepted and served locally.
# TYPE outpostimage_proxy_intercepted_total counter
outpostimage_proxy_intercepted_total{type="blob"} 42
outpostimage_proxy_intercepted_total{type="manifest"} 8

# HELP outpostimage_proxy_upstream_total Number of requests forwarded to upstream registry.
# TYPE outpostimage_proxy_upstream_total counter
outpostimage_proxy_upstream_total{type="blob"} 3
outpostimage_proxy_upstream_total{type="manifest"} 1

# HELP outpostimage_peer_health_status Peer health status (1=healthy, 0=unhealthy).
# TYPE outpostimage_peer_health_status gauge
outpostimage_peer_health_status{peer="192.168.1.101:5000"} 1
outpostimage_peer_health_status{peer="192.168.1.102:5000"} 1
outpostimage_peer_health_status{peer="192.168.1.103:5000"} 0

# HELP outpostimage_storage_digests Total number of unique digests tracked.
# TYPE outpostimage_storage_digests gauge
outpostimage_storage_digests 157
```

### Prometheus scrape configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: outpostimage
    static_configs:
      - targets:
          - "node1:9090"
          - "node2:9090"
          - "node3:9090"
    metrics_path: /metrics
    scrape_interval: 15s
```

For Kubernetes, enable the `ServiceMonitor` in Helm values (requires prometheus-operator):

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
```
