# Changelog

All notable changes to OutpostImage are documented in this file.

## v1.0.0 (2026-03-31)

### Features

- **Docker and containerd image sharing via P2P mesh** — nodes advertise locally available image digests and serve blobs directly to peers without upstream registry access
- **HTTPS MITM proxy for zero-config registry mirroring** — intercepts Docker's HTTPS requests via a transparent proxy; no daemon.json changes required, only `HTTP_PROXY`/`HTTPS_PROXY` environment variables
- **Multi-registry support** — transparently handles Docker Hub (`docker.io`), GitHub Container Registry (`ghcr.io`), and custom registries with no per-registry configuration
- **Dual runtime support** — reads from both Docker (`/var/lib/docker`) and containerd (`/var/lib/containerd`) local stores; auto-detects and uses MultiStore when both are present
- **Offline / air-gapped mode** — fully functional with no external network access; image sharing relies only on the local LAN mesh
- **Prometheus metrics** — exposes request counts, routing decisions, and transfer stats at `:19090/metrics`
- **OCI Distribution Spec compliant** — implements `GET /v2/`, manifest, and blob endpoints per the OCI Distribution Specification
- **Static single binary** — no dynamic dependencies; 11 MB statically linked binary with only 7 direct Go dependencies

### Performance

- Init startup time: ~14 ms (lazy tar digest computation; was ~80 s in pre-release)
- First manifest request (including lazy build): ~760 ms
- Cached manifest requests: < 1 ms
- 500 MB model image transfer: 18.84 MB/s (VM-to-VM over LAN)
- 2 GB model image transfer: 15.86 MB/s
- Repeated pull (layer cache hit): 123 ms (260× speedup)
- Parallel pulls: 26.7% wall-clock savings vs serial for 500 MB + 2 GB concurrent

### Bug Fixes

- `BUG-VP1-01` — `/v2/` version check was forwarded upstream instead of served locally
- `BUG-VP1-02` — `library/` prefix normalization for short Docker Hub image names
- `BUG-VP2-01` — original registry hostname not passed correctly for multi-registry routing
- `BUG-VP3-01` — `Docker-Content-Digest` returned config digest instead of manifest content hash, breaking containerd pull verification
- `BUG-VP3-02` — containerd BoltDB locked by daemon; read target from sub-bucket instead of JSON value
- `BUG-VP3-03` — containerd canonical reference format (`docker.io/library/redis:7-alpine`) not matched during `Resolve()`
- Metrics middleware `statusRecorder` lost `http.Hijacker` interface, causing HTTPS CONNECT to return 500
- Proxy router address normalization for `:15000` format; tracker now indexes layer blob digests in addition to config digest

### Validated Value Propositions

| VP | Description | Status |
|----|-------------|--------|
| VP-1 | Zero onboarding cost via HTTPS MITM proxy | ✅ PASS |
| VP-2 | Multi-registry transparent coverage (docker.io, ghcr.io, custom) | ✅ PASS |
| VP-3 | Docker + containerd dual runtime (5/5 Store×Consumer combinations) | ✅ PASS |
| VP-4 | Minimal dependencies (7 direct, 56 total, 11 MB binary) | ✅ PASS |
| VP-5 | Offline-first / air-gapped operation | ✅ PASS |
| VP-6 | Large model image distribution (up to 2 GB, parallel pulls) | ✅ PASS |

### Infrastructure

- GitHub Actions CI/CD pipeline with build, test, and lint stages
- Dockerfile and `.dockerignore` for containerized builds
- Cloudflare Pages static landing page
- Release workflow with `ldflags` version injection
- Makefile with `build`, `test`, `lint`, and `release` targets

### Documentation

- `README.md` — project overview, quick-start, and architecture
- `docs/deployment-guide.md` — production deployment instructions
- `docs/api-reference.md` — OCI Distribution Spec endpoint reference
- `docs/test-report-image-sharing-2026-03-30.md` — node-to-node image sharing validation
- `docs/test-report-optimizations-2026-03-30.md` — optimization and bugfix validation
- `docs/test-report-value-propositions-2026-03-30.md` — full VP matrix test results including VP-6 performance
