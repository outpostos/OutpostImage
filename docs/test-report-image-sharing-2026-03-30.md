# OutpostImage Node-to-Node Image Sharing — Test Report

**Date**: 2026-03-30
**Version**: v0.1.0 (commit `ba2469e`)
**Tester**: CEO Agent
**Project**: OutpostImage

---

## 1. Test Objective

Validate that OutpostImage can achieve node-to-node container image sharing: Docker on VM 103 (openyurt-edge-node) can pull images from T480 (192.168.1.200).

## 2. Test Environment

| Node | Role | IP | OS | Docker Version | Notes |
|------|------|----|----|----|-------|
| T480 | Image source (Registry) | 192.168.1.200 | Proxmox VE (Linux 6.2.16) | 27.5.1 | Proxmox host, 11 Docker images pre-loaded |
| VM 103 | Image consumer (Client) | 192.168.1.103 | Ubuntu 20.04.6 LTS | 26.1.3 | Proxmox KVM VM, 2C/2G/30G, fresh Docker install |

### Network Topology

```
T480 (192.168.1.200)          VM 103 (192.168.1.103)
┌─────────────────┐           ┌─────────────────┐
│  Docker Engine  │           │  Docker Engine  │
│  11 images      │           │  0 images       │
│                 │           │                 │
│  OutpostImage   │◄─────────►│  Docker pull    │
│  :15000 registry│  vmbr0    │  (insecure reg) │
│  :13128 proxy   │  LAN      │                 │
│  :19090 metrics │           │                 │
└─────────────────┘           └─────────────────┘
```

## 3. OutpostImage Configuration

### T480 (Image Source)

```bash
outpostimage registry \
  --registry-addr=:15000 \
  --proxy-addr=:13128 \
  --metrics-addr=:19090 \
  --store-type=docker \
  --peers=192.168.1.103:15000 \
  --mdns-enabled=false \
  --log-level=INFO \
  --ca-dir=/etc/outpostimage
```

Startup log:
```
docker store initialized: images=11, tags=11, blobs=22, layers=100, layer_blobs=100
```

### VM 103 (Image Consumer)

Docker daemon config (`/etc/docker/daemon.json`):
```json
{
  "insecure-registries": ["192.168.1.200:15000"]
}
```

## 4. Test Cases and Results

### TC-01: OCI Registry API Connectivity

| Step | Operation | Expected | Actual | Status |
|------|-----------|---------|--------|--------|
| 1 | `curl http://192.168.1.200:15000/v2/` | Returns `{}` | `{}` | PASS |
| 2 | `curl http://192.168.1.200:15000/api/v1/digests` | Returns digest list | 11 digests | PASS |
| 3 | Access T480 registry from VM 103 | Same as above | Same as above | PASS |

### TC-02: Single-Layer Image Pull (debian:bookworm-slim)

| Step | Operation | Expected | Actual | Status |
|------|-----------|---------|--------|--------|
| 1 | Fetch manifest | schemaVersion=2, 1 layer | Correct distribution manifest returned | PASS |
| 2 | `docker pull 192.168.1.200:15000/debian:bookworm-slim` | Pull successful | Downloaded and checksum verified | PASS |
| 3 | `docker run --rm ... cat /etc/debian_version` | Output Debian version | `12.13` | PASS |

Pull output:
```
bookworm-slim: Pulling from debian
5712ebd750aa: Pull complete
Digest: sha256:42a264b6a07c8501a0677b301d5e4510f9bdd2949c3c7e7ff412a272b6e088c0
Status: Downloaded newer image for 192.168.1.200:15000/debian:bookworm-slim
```

Manifest details:
```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "digest": "sha256:d1c8cb627495af1273dfd812f15bbf942f15775047fff6a4ea34f66f0288760a",
    "size": 452
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar",
      "digest": "sha256:5712ebd750aa6886b5e20c16b3ecc65a5f7010fba68b26063a75f5b64d9c7c37",
      "size": 81697280
    }
  ]
}
```

### TC-03: Multi-Layer Image Pull (redis:7-alpine, 8 layers)

| Step | Operation | Expected | Actual | Status |
|------|-----------|---------|--------|--------|
| 1 | Fetch manifest | schemaVersion=2, 8 layers | Correct 8-layer distribution manifest returned | PASS |
| 2 | `docker pull 192.168.1.200:15000/redis:7-alpine` | Pull successful | All 8 layers downloaded and checksum verified | PASS |
| 3 | `docker run --rm ... redis-server --version` | Output Redis version | `Redis server v=7.4.7` | PASS |

Pull output:
```
7-alpine: Pulling from redis
34c35a53577f: Pull complete
2c7a5921b968: Pull complete
f72e42786ed1: Pull complete
9410ff42c14d: Pull complete
ce813de5e721: Pull complete
f45f34f5586b: Pull complete
5f70bf18a086: Pull complete
972d55cb1c64: Pull complete
Digest: sha256:1204e164683759497befc680d80c8afbdffde0e9d6a1c6c94f4b8c1dba0f7701
Status: Downloaded newer image for 192.168.1.200:15000/redis:7-alpine
```

### TC-04: Image Integrity Verification

| Check | debian:bookworm-slim | redis:7-alpine | Status |
|-------|---------------------|----------------|--------|
| Image ID consistency | d1c8cb627495 | d050212bffac | PASS |
| Image size reasonable | 78.6MB | 41.7MB | PASS |
| Container starts normally | `cat /etc/debian_version` → `12.13` | `redis-server --version` → `v7.4.7` | PASS |
| Digest checksum verified | Docker auto-verifies checksum | Docker auto-verifies checksum | PASS |

## 5. VM 103 Final State

```
REPOSITORY                   TAG             IMAGE ID       CREATED        SIZE
192.168.1.200:15000/debian   bookworm-slim   d1c8cb627495   4 weeks ago    78.6MB
192.168.1.200:15000/redis    7-alpine        d050212bffac   4 months ago   41.7MB
```

## 6. Code Changes

One critical defect was found and fixed during this validation:

**Issue**: In `pkg/oci/docker.go`, `GetBlob` only returned image config blob, unable to serve layer data; `GetManifest` returned image config instead of OCI distribution manifest.

**Fix** (commit `ba2469e`):
- Generate tar blob from overlay2 diff directory and compute digest
- Build OCI Distribution Spec compliant manifest (schemaVersion 2)
- Rewrite image config `diff_ids` field to match generated tar digest
- Add `rewrittenConfigs` in-memory store to serve rewritten config blob

**Impact**: +341 lines / -29 lines, only modifies `pkg/oci/docker.go`

## 7. Known Limitations

| Limitation | Impact | Priority |
|-----------|--------|---------|
| Init phase tar computation ~80s (100 layers) | Slow startup, recommend lazy loading | Medium |
| HTTP insecure registry mode only | Requires Docker `insecure-registries` configuration | Medium |
| HTTPS MITM not connected | Cannot transparently intercept Docker Hub HTTPS requests | Low (Phase 3) |
| Layer tar differs from original tar metadata | Requires rewriting config diff_ids, image digest differs from upstream | Low |

## 8. Conclusion

**Tests passed.** OutpostImage successfully achieved node-to-node container image sharing:

- Docker on VM 103 can pull complete container images from OutpostImage registry on T480
- Both single-layer images (debian) and multi-layer images (redis, 8 layers) verified
- Pulled images can start containers and execute commands normally
- Docker checksum verification all passed, image integrity guaranteed

---

*Report generated: 2026-03-30 | OutpostImage v0.1.0 | Commit ba2469e*
