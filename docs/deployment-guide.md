# OutpostImage Deployment Guide

## Prerequisites

- Linux x86_64 or ARM64
- Docker 19.03+ (overlay2 storage driver) or containerd 1.6+
- Outbound access to container registries (for initial pulls and cache misses)
- Ports 5000, 3128, 9090 available on each node

---

## 1. Bare-Metal Deployment (systemd)

### Install the binary

Releases publish a container image (`ghcr.io/outpostos/outpostimage:latest`), not
standalone binaries. For a bare-metal install, build the binary from source with
Go 1.25+:

```bash
git clone https://github.com/outpostos/outpostimage
cd outpostimage
make build
sudo install -m 0755 bin/outpostimage /usr/local/bin/outpostimage

# Verify
outpostimage --version
```

### Create a peers file

```bash
mkdir -p /etc/outpostimage

# List one peer address per line (host:port)
cat > /etc/outpostimage/peers.txt <<EOF
192.168.1.101:5000
192.168.1.102:5000
192.168.1.103:5000
EOF
```

### Create the systemd unit file

```ini
# /etc/systemd/system/outpostimage.service
[Unit]
Description=OutpostImage P2P OCI Registry Proxy
After=docker.service network-online.target
Wants=network-online.target
PartOf=docker.service

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/outpostimage registry \
  --registry-addr=:5000 \
  --proxy-addr=:3128 \
  --metrics-addr=:9090 \
  --docker-root=/var/lib/docker \
  --peers-file=/etc/outpostimage/peers.txt \
  --ca-dir=/etc/outpostimage \
  --log-level=INFO
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

# Environment variables (alternative to CLI flags)
# Environment=PEERS_FILE=/etc/outpostimage/peers.txt
# Environment=LOG_LEVEL=INFO

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable outpostimage
systemctl start outpostimage
systemctl status outpostimage
```

### Configure Docker to use the proxy

```bash
# Create Docker proxy configuration
mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/proxy.conf <<EOF
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
Environment="NO_PROXY=localhost,127.0.0.1"
EOF

systemctl daemon-reload
systemctl restart docker
```

### Trust the generated CA certificate

OutpostImage generates a self-signed CA on first startup at `/etc/outpostimage/ca.crt`.

```bash
# Wait for OutpostImage to start and generate the CA
sleep 5

# Add to system trust store (Debian/Ubuntu)
cp /etc/outpostimage/ca.crt /usr/local/share/ca-certificates/outpostimage-ca.crt
update-ca-certificates

# Add to system trust store (RHEL/CentOS)
cp /etc/outpostimage/ca.crt /etc/pki/ca-trust/source/anchors/outpostimage-ca.crt
update-ca-trust

# Restart Docker to pick up the new CA
systemctl restart docker
```

### Verify

```bash
# Check health
curl http://localhost:9090/healthz
curl http://localhost:5000/healthz

# Test an image pull (should hit P2P if peers have it)
docker pull nginx:latest
```

---

## 2. Docker / Docker Compose Deployment

### Single node (Docker run)

The container image is built locally (the GHCR package is currently private):

```bash
git clone https://github.com/outpostos/outpostimage
cd outpostimage
docker build -t outpostimage:latest .

docker run -d \
  --name outpostimage \
  --network host \
  --restart unless-stopped \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v outpostimage-ca:/etc/outpostimage \
  -e LOG_LEVEL=INFO \
  -e PEERS=192.168.1.101:5000,192.168.1.102:5000 \
  outpostimage:latest registry
```

### Multi-node (Docker Compose)

Clone the repository and customize the peers file:

```bash
git clone https://github.com/outpostos/outpostimage
cd outpostimage

# Create peers file
cat > peers.txt <<EOF
192.168.1.101:5000
192.168.1.102:5000
192.168.1.103:5000
EOF

# Enable the static peer list: uncomment the peers volume mount and the
# PEERS_FILE environment entry in docker-compose.yml
docker compose up -d
```

The default `docker-compose.yml` starts:
- `outpostimage` on ports 5000, 3128, 9090
- `prometheus` on port 9091 (scrapes outpostimage metrics)

After starting, configure Docker to use the proxy as described in Section 1.

### Build from source

```bash
git clone https://github.com/outpostos/outpostimage
cd outpostimage
docker build -t outpostimage:latest .
# If proxy.golang.org is unreachable:
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct -t outpostimage:latest .
```

Base images (`golang:1.25-alpine`, `alpine:3.19`) are pulled from the public
`swr.cn-east-3.myhuaweicloud.com/risecloud` namespace (amd64+arm64), not from
Docker Hub; `deploy/docker/dockerfile_test.go` enforces this.

---

## 3. Kubernetes DaemonSet Deployment (Helm)

### Install via Helm

```bash
# Add the chart (replace with actual chart repo when published)
helm install outpostimage ./charts/outpostimage \
  --namespace outpostimage \
  --create-namespace \
  --set config.logLevel=INFO
```

### Configure peers via values.yaml

```yaml
# values.yaml
image:
  repository: outpostimage
  tag: latest

config:
  logLevel: INFO
  dockerRoot: /var/lib/docker
  registryAddr: ":5000"
  proxyAddr: ":3128"
  metricsAddr: ":9090"
  caDir: /etc/outpostimage

# Newline-delimited list of peers
# OutpostImage on each node will advertise its own address via mDNS
# For static configuration, list all node IPs here
peers: |
  192.168.1.101:5000
  192.168.1.102:5000
  192.168.1.103:5000

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 256Mi

# Enable Prometheus ServiceMonitor (requires prometheus-operator)
serviceMonitor:
  enabled: true
  interval: 30s
```

```bash
helm install outpostimage ./charts/outpostimage \
  --namespace outpostimage \
  --create-namespace \
  -f values.yaml
```

### Configure Docker on each K8s node

After deploying the DaemonSet, configure Docker on every node to use the proxy. This is typically done via a DaemonSet or node configuration tool:

```bash
# On each node
mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/proxy.conf <<EOF
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
EOF
systemctl daemon-reload && systemctl restart docker
```

### Verify DaemonSet

```bash
kubectl -n outpostimage get pods -o wide
kubectl -n outpostimage logs -l app.kubernetes.io/name=outpostimage --tail=50
kubectl -n outpostimage exec -it <pod-name> -- wget -qO- http://localhost:9090/healthz
```

---

## 4. Edge Node Configuration

### Configure Docker HTTP_PROXY

OutpostImage intercepts image pulls via the HTTP proxy. Configure Docker to route traffic through it:

```json
// /etc/docker/daemon.json
{
  "proxies": {
    "http-proxy": "http://localhost:3128",
    "https-proxy": "http://localhost:3128",
    "no-proxy": "localhost,127.0.0.1"
  }
}
```

Or via systemd drop-in (takes precedence):

```ini
# /etc/systemd/system/docker.service.d/proxy.conf
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
Environment="NO_PROXY=localhost,127.0.0.1"
```

Restart Docker after making changes: `systemctl restart docker`

### Insecure Registry Configuration

For private registries without valid TLS certificates, use `--insecure-registry` to skip TLS verification. OutpostImage will connect to those registries without verifying their certificate chain:

```bash
# Binary
outpostimage registry \
  --insecure-registry registry.internal:5000 \
  --insecure-registry 192.168.1.50:5000

# Environment variable (comma- or space-separated)
INSECURE_REGISTRIES="registry.internal:5000 192.168.1.50:5000" outpostimage registry
```

In Docker Compose:

```yaml
environment:
  - INSECURE_REGISTRIES=registry.internal:5000 192.168.1.50:5000
```

In Helm values:

```yaml
config:
  insecureRegistries: "registry.internal:5000 192.168.1.50:5000"
```

> **Note**: `--insecure-registry` only affects how OutpostImage connects to upstream registries. Docker itself does not need the registry marked as insecure because OutpostImage acts as the proxy and handles the upstream TLS.

### HTTPS Interception (CA Certificate)

OutpostImage generates a CA certificate on first startup (`/etc/outpostimage/ca.crt`). The proxy signs per-domain certificates on the fly to intercept HTTPS registry traffic. This CA must be trusted by Docker on each node.

```bash
# View the generated CA
cat /etc/outpostimage/ca.crt

# Add to Docker's certificate store for a specific registry
mkdir -p /etc/docker/certs.d/registry.example.com
cp /etc/outpostimage/ca.crt /etc/docker/certs.d/registry.example.com/ca.crt

# Or add to system-wide trust (then restart Docker)
cp /etc/outpostimage/ca.crt /usr/local/share/ca-certificates/outpostimage-ca.crt
update-ca-certificates
systemctl restart docker
```

---

## 5. Multi-Node P2P Networking

### Static peer configuration

Each node needs to know the addresses of other nodes. Configure via `--peers`, `--peers-file`, or the `PEERS` environment variable.

**peers.txt** (one address per line, hot-reloaded on change):

```
192.168.1.101:5000
192.168.1.102:5000
192.168.1.103:5000
```

The file is watched with inotify. Changes take effect within 10 seconds without restarting.

### mDNS peer discovery (LAN only)

On LAN environments, mDNS is enabled by default. Nodes automatically discover each other without any manual configuration:

```bash
# mDNS is on by default
outpostimage registry

# Restrict to a specific network interface
outpostimage registry --mdns-interface eth0

# Disable mDNS (use only static peers)
outpostimage registry --mdns-enabled=false
```

mDNS discovery is suitable for small clusters on the same subnet. For multi-subnet or cloud environments, use static peers.

### Mixed: static peers + mDNS

```bash
outpostimage registry \
  --peers-file=/etc/outpostimage/peers.txt \
  --mdns-enabled=true \
  --mdns-interface=eth0
```

### Firewall rules

Open the following ports between all cluster nodes:

| Port | Protocol | Purpose |
|------|----------|---------|
| 5000 | TCP | P2P Registry API (blob/manifest serving) |
| 5353 | UDP | mDNS peer discovery (LAN only) |

The proxy port (3128) and metrics port (9090) are node-local and do not need to be open between nodes.

### Health check behavior

OutpostImage performs periodic health checks on all peers every 30 seconds. Unhealthy peers are automatically removed from the routing pool and re-added when they become healthy again. Exponential backoff is applied to repeatedly failing peers (1min, 2min, 5min intervals).

### Verifying P2P is working

```bash
# Check which digests this node knows about
curl http://localhost:5000/api/v1/digests | jq .

# Check if a specific digest exists on a peer
curl http://192.168.1.101:5000/api/v1/digests/sha256:abc123...

# Check metrics for P2P hit rate
curl http://localhost:9090/metrics | grep outpostimage_proxy_intercepted_total
```
