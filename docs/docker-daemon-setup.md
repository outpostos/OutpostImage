# Docker Daemon Integration Guide

## Prerequisites

- Docker 19.03+ with overlay2 storage driver
- OutpostImage binary installed and running

## 1. Configure Docker Daemon Proxy

Create a systemd drop-in configuration for the Docker daemon:

```bash
sudo mkdir -p /etc/systemd/system/docker.service.d
cat <<EOF | sudo tee /etc/systemd/system/docker.service.d/proxy.conf
[Service]
Environment="HTTP_PROXY=http://127.0.0.1:3128"
Environment="HTTPS_PROXY=http://127.0.0.1:3128"
Environment="NO_PROXY=localhost,127.0.0.1"
EOF
```

## 2. Install CA Certificate

OutpostImage generates a self-signed CA for intercepting HTTPS registry traffic.
Install the CA certificate into the system trust store:

```bash
# Copy CA cert
sudo cp /etc/outpostimage/ca.crt /usr/local/share/ca-certificates/outpostimage.crt
sudo update-ca-certificates
```

## 3. Restart Docker

```bash
sudo systemctl daemon-reload
sudo systemctl restart docker
```

## 4. Verify

```bash
# Check that traffic flows through OutpostImage
docker pull nginx:latest

# Check OutpostImage logs for intercepted requests
journalctl -u outpostimage -f
```

## Configuration Reference

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--registry-addr` | `REGISTRY_ADDR` | `:5000` | P2P Registry API listen address |
| `--proxy-addr` | `PROXY_ADDR` | `:3128` | HTTP Proxy listen address |
| `--metrics-addr` | `METRICS_ADDR` | `:9090` | Metrics listen address |
| `--docker-root` | `DOCKER_ROOT` | `/var/lib/docker` | Docker storage root |
| `--peers` | `PEERS` | | Static peer list (host:port) |
| `--peers-file` | `PEERS_FILE` | | Peers file path (hot-reload) |
| `--log-level` | `LOG_LEVEL` | `INFO` | Log level |
