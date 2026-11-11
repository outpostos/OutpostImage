# OutpostImage

P2P OCI image distribution proxy for edge computing. OutpostImage turns every edge node into a zero-overhead OCI registry by reusing the local Docker/containerd image store, enabling node-to-node P2P image sharing without extra storage.

## Overview

```
┌──────────────────────────────────────────────────────────────────┐
│                          Edge Node                               │
│                                                                  │
│  ┌─────────────┐    ┌─────────────┐                             │
│  │  containerd │    │   dockerd   │  (any OCI client)           │
│  └──────┬──────┘    └──────┬──────┘                             │
│         │ HTTP_PROXY        │ HTTP_PROXY                         │
│         └──────────┬────────┘                                   │
│                    ↓                                             │
│          ┌──────────────────┐                                    │
│          │  OutpostImage    │  :3128 (proxy)                     │
│          │  HTTP Proxy      │                                    │
│          └────────┬─────────┘                                    │
│                   │                                              │
│         ┌─────────┴──────────┐                                   │
│         ↓                    ↓                                   │
│  ┌─────────────┐    ┌──────────────────┐                        │
│  │ Local Store │    │  P2P Peers       │  other edge nodes       │
│  │ (Docker/    │    │  :5000 registry  │                        │
│  │  containerd)│    │  API             │                        │
│  └─────────────┘    └──────────────────┘                        │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
         │ cache miss
         ↓
    Upstream Registry (internet)
```

**How it works:**

1. Docker/containerd is configured with `HTTP_PROXY=http://localhost:3128`
2. OutpostImage intercepts image pull requests via the HTTP proxy
3. For HTTPS registries, it performs MITM with a locally-generated CA certificate
4. The proxy checks if any P2P peer has the requested blob/manifest
5. If found in P2P network → serve from peer (LAN speed)
6. If not found → forward to upstream registry (internet)

## Key Differences from Spegel

| Feature | Spegel | OutpostImage |
|---------|--------|--------------|
| Runtime support | containerd only | Docker + containerd (via HTTP proxy) |
| Interception | containerd mirror config | HTTP Proxy (`HTTP_PROXY` env var) |
| Extra storage | None (containerd reuse) | None (Docker/containerd reuse) |
| P2P discovery | OCI/libp2p | Static peers + mDNS + K8s Endpoints |
| Multi-registry | Yes | Yes |
| Invasiveness | Modifies containerd config | Zero-intrusion (env var only) |

## Quick Start

### Binary

```bash
# Download the latest release binary (v1.0.0+)
curl -L https://github.com/outpostos/OutpostImage/releases/latest/download/outpostimage-linux-amd64 \
  -o /usr/local/bin/outpostimage && chmod +x /usr/local/bin/outpostimage

# Run with defaults (auto-detects Docker or containerd store)
outpostimage registry

# Run with explicit peers
outpostimage registry --peers node2:5000 --peers node3:5000
```

### Docker

```bash
docker run -d \
  --name outpostimage \
  --network host \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  ghcr.io/outpostos/outpostimage:latest registry
```

### Docker Compose

```bash
git clone https://github.com/outpostos/outpostimage
cd outpostimage

# Optional: static peer list. Nodes find each other via mDNS by default.
# To use a static list, create peers.txt, then uncomment the peers volume
# and PEERS_FILE entry in docker-compose.yml.
echo "192.168.1.101:5000" > peers.txt
echo "192.168.1.102:5000" >> peers.txt

docker compose up -d
```

### Configure Docker to use the proxy

```bash
# /etc/docker/daemon.json
{
  "proxies": {
    "http-proxy": "http://localhost:3128",
    "https-proxy": "http://localhost:3128"
  }
}

# Or set environment variables for dockerd
# /etc/systemd/system/docker.service.d/proxy.conf
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
```

After configuring the proxy, trust the OutpostImage CA certificate:

```bash
# Copy the generated CA to Docker's trust store
cp /etc/outpostimage/ca.crt /etc/docker/certs.d/registry.example.com/ca.crt

# Or add to system trust store
cp /etc/outpostimage/ca.crt /usr/local/share/ca-certificates/outpostimage-ca.crt
update-ca-certificates
```

## Configuration Reference

All flags can also be set via environment variables (shown in parentheses).

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--registry-addr` | `REGISTRY_ADDR` | `:5000` | P2P Registry API listen address |
| `--proxy-addr` | `PROXY_ADDR` | `:3128` | HTTP Proxy listen address |
| `--metrics-addr` | `METRICS_ADDR` | `:9090` | Prometheus metrics listen address |
| `--docker-root` | `DOCKER_ROOT` | `/var/lib/docker` | Docker storage root directory |
| `--containerd-root` | `CONTAINERD_ROOT` | `/var/lib/containerd` | Containerd storage root directory |
| `--store-type` | `STORE_TYPE` | `auto` | Storage backend: `auto`, `docker`, `containerd` |
| `--peers` | `PEERS` | — | Static peer list, repeatable (e.g. `node1:5000`) |
| `--peers-file` | `PEERS_FILE` | — | Path to a newline-delimited peers file (hot-reloaded) |
| `--mdns-enabled` | `MDNS_ENABLED` | `true` | Enable mDNS peer discovery |
| `--mdns-interface` | `MDNS_INTERFACE` | — | Network interface for mDNS (default: all) |
| `--mdns-domain` | `MDNS_DOMAIN` | `local` | mDNS domain |
| `--log-level` | `LOG_LEVEL` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `--ca-dir` | `CA_DIR` | `/etc/outpostimage` | Directory for CA certificate and key |
| `--k8s-discovery` | `K8S_DISCOVERY` | `false` | Enable Kubernetes Endpoints peer discovery |
| `--k8s-service-name` | `K8S_SERVICE_NAME` | `outpostimage` | Kubernetes Service name for peer discovery |
| `--k8s-namespace` | `K8S_NAMESPACE` | — | Kubernetes namespace (auto-detected if empty) |
| `--insecure-registry` | `INSECURE_REGISTRIES` | — | Registries to skip TLS verification, repeatable |

## API Endpoints Overview

OutpostImage exposes three servers:

| Server | Default Port | Purpose |
|--------|-------------|---------|
| Registry | `:5000` | OCI Distribution API + P2P digest query |
| Proxy | `:3128` | HTTP/HTTPS proxy for intercepting pull requests |
| Metrics | `:9090` | Prometheus metrics + health checks |

**Registry API (`:5000`)**:
- `GET /v2/` — OCI version check
- `GET /v2/{name}/manifests/{reference}` — fetch manifest
- `HEAD /v2/{name}/blobs/{digest}` — check blob existence
- `GET /v2/{name}/blobs/{digest}` — fetch blob
- `GET /api/v1/digests` — list all local digests (P2P peer query)
- `GET /api/v1/digests/{digest}` — check if a digest exists locally
- `GET /healthz` — liveness probe
- `GET /readyz` — readiness probe

See [docs/api-reference.md](docs/api-reference.md) for full API documentation.

## Kubernetes Deployment (Helm)

```bash
helm install outpostimage charts/outpostimage \
  --namespace outpostimage --create-namespace
```

By default, the chart:
- Deploys a DaemonSet on every node
- Creates a headless Service for automatic peer discovery via Kubernetes Endpoints
- Sets up RBAC (ServiceAccount, Role, RoleBinding) for endpoints access
- Disables mDNS (not reliable in k8s) and enables Kubernetes discovery
- Uses `hostNetwork: true` so Docker/containerd can reach the proxy at `localhost:3128`

No manual peer configuration needed — nodes discover each other automatically.

## Deployment

See [docs/deployment-guide.md](docs/deployment-guide.md) for:
- Bare-metal / systemd deployment
- Docker Compose deployment
- Kubernetes DaemonSet deployment (Helm)
- Multi-node P2P networking configuration
- Edge node proxy and insecure registry setup
