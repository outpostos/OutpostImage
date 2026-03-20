# OutpostImage Product Requirements Document (PRD)

**Version**: 0.1
**Status**: Draft
**Author**: PM John
**Created**: 2026-03-01

## Change Log

| Date | Version | Description | Author |
|------|---------|-------------|--------|
| 2026-03-01 | 0.1 | Initial PRD draft from Project Brief + Architecture Optimization | PM John |

---

## 1. Goals

- **G1**: 实现 Docker overlay2 存储适配，将本地容器镜像以 OCI Distribution Spec v2 API 暴露给集群内其他节点
- **G2**: 通过 HTTP Proxy 透明拦截容器运行时的镜像拉取请求，自动路由到 P2P 网络或回退到上游 registry
- **G3**: 实现静态节点发现与内存元数据索引，支持 10-100 节点规模的边缘集群
- **G4**: 零额外存储开销 -- 直接复用 Docker/containerd 的 OCI 本地存储，不引入独立缓存层
- **G5**: 降低边缘集群 80% 的外网镜像拉取带宽消耗
- **G6**: P90 镜像拉取时间 < 5 分钟（5GB 镜像，10 节点集群）
- **G7**: 提供 Prometheus 监控指标，支撑运维可观测性

## 2. Background Context

OutpostImage 项目应对边缘计算场景中容器镜像分发的核心瓶颈：边缘节点内网带宽远大于出口带宽（典型 10Gbps vs 100Mbps），但现有方案（Registry Mirror、Dragonfly、Spegel）要么只支持单一 registry，要么需要独立缓存导致存储翻倍，要么仅绑定 containerd。

OutpostImage 的核心创新是**直接复用容器运行时的 OCI 本地存储**，将每个边缘节点转化为 OCI Registry，构建零额外存储开销的 P2P 镜像共享网络。基于对成熟的 Depot 项目分析，MVP 采用 HTTP Proxy + 动态证书作为拦截方式，静态节点发现作为 P2P 发现机制（DHT + Gossip 留待 Phase 2），将原 12 周 MVP 计划压缩至 8-10 周。

### 竞品对比

| 方案 | 存储策略 | Registry 支持 | 运行时支持 | 发现机制 |
|------|---------|-------------|-----------|---------|
| Registry Mirror | 独立存储 | 单一 registry | Docker/containerd | 静态配置 |
| Dragonfly | 独立缓存 | 多 registry | Docker/containerd | SuperNode |
| Spegel | 复用 containerd | 多 registry | 仅 containerd | OCI 标准 |
| **OutpostImage** | **复用 OCI 存储** | **多 registry** | **Docker (MVP)** | **静态 + HTTP Proxy** |

### 目标用户

- **Primary**: 边缘平台 DevOps 工程师（管理 10-1000 节点的边缘集群）
- **Secondary**: 边缘应用开发者（频繁部署/更新镜像到边缘节点）

---

## 3. Functional Requirements

- **FR1**: 存储适配层 -- 解析 Docker overlay2 存储结构（`/var/lib/docker/image/overlay2/imagedb/` 和 `/var/lib/docker/overlay2/`），提取镜像 manifest、config 和 layer blob，通过 chainID 算法准确映射 digest 到文件路径
- **FR2**: OCI Registry API -- 实现 OCI Distribution Spec v2 核心 endpoints：`GET /v2/`（版本检查）、`GET /v2/<name>/manifests/<reference>`（manifest）、`HEAD/GET /v2/<name>/blobs/<digest>`（blob），支持 Docker Image Manifest V2 Schema 2 和 OCI Image Manifest
- **FR3**: HTTP Proxy 拦截 -- 实现 HTTP/HTTPS 代理服务，识别 OCI Registry API 路径模式，支持 HTTPS CONNECT 隧道，对容器运行时完全透明（通过 `HTTP_PROXY` 环境变量配置）
- **FR4**: 动态证书管理 -- 为 HTTPS registry 请求动态生成域名证书（自签名 CA），支持 insecure-registry 白名单配置
- **FR5**: 智能请求路由 -- 查询本地元数据索引判断 P2P 可用性，优先从本地节点拉取，支持多节点并行下载（取最快响应），P2P 失败时自动回退到上游 registry
- **FR6**: 静态节点发现 -- 通过 YAML 配置文件指定集群节点列表（IP/域名 + 端口），支持配置文件热更新（fsnotify 监听，10 秒内生效），定期健康检查并自动剔除不可用节点
- **FR7**: 元数据内存索引 -- 启动时扫描本地 Docker 存储构建 `map[digest]LayerInfo` 索引，支持通过 fsnotify 增量更新（检测新镜像拉取和删除），提供 API 供其他节点查询本地 digest 列表（`GET /api/v1/digests`）
- **FR8**: 自动回退机制 -- 本地节点请求超时 5 秒后自动回退上游 registry，失败节点采用指数退避（1min/2min/5min），回退过程对用户完全透明
- **FR9**: Prometheus 监控指标 -- 暴露 `/metrics` endpoint，核心指标包括：`outpostimage_pulls_total{source}`、`outpostimage_bytes_transferred{source}`、`outpostimage_pull_duration_seconds`（histogram）、`outpostimage_cache_hit_rate{type}`、`outpostimage_cluster_nodes_total`

---

## 4. Non-Functional Requirements

- **NFR1**: 启动性能 -- 服务冷启动到可用 < 10 秒（含 1000 layer 扫描）
- **NFR2**: 内存占用 -- idle 状态 < 100MB，高负载 < 500MB
- **NFR3**: CPU 占用 -- idle < 5%，传输中 < 50%
- **NFR4**: 元数据索引内存 -- 1000 layers 索引 < 50MB
- **NFR5**: 可用性 -- 所有 P2P 节点不可用时，镜像拉取仍 100% 成功（自动回退外网）
- **NFR6**: 拉取性能 -- 内网 P2P 拉取速度 > 100MB/s（1Gbps 网络环境）
- **NFR7**: 只读安全 -- 对容器运行时存储仅做只读访问，不修改任何运行时数据
- **NFR8**: 平台兼容 -- 支持 Linux x86_64 和 ARM64，Docker 19.03+（overlay2 存储驱动）
- **NFR9**: 单二进制部署 -- Go 静态编译，产出单个可执行文件，支持 Docker 镜像和 K8s DaemonSet 部署
- **NFR10**: 配置热更新 -- 节点配置变更无需重启服务

---

## 5. Technical Assumptions & Constraints

### Technical Assumptions

| # | Assumption | Impact if Wrong | Mitigation |
|---|-----------|----------------|------------|
| **TA1** | Docker overlay2 的 `imagedb/` 和 `overlay2/` 目录结构在 Docker 19.03-27.x 保持稳定 | 存储适配层解析失败，无法暴露本地镜像 | 抽象 StorageAdapter 接口，版本检测 + 适配 |
| **TA2** | `HTTP_PROXY` 环境变量被 Docker daemon 正确传递到镜像拉取流程 | 代理拦截失效，请求直连上游 | Docker daemon 配置文档 + 集成测试覆盖 |
| **TA3** | 边缘集群内网为扁平网络，节点间可直接 TCP 通信 | P2P 传输被防火墙/NAT 阻断 | 文档明确网络前提条件，提供端口配置指南 |
| **TA4** | 边缘节点具有对本地 Docker 存储目录的只读权限 | 无法扫描本地镜像元数据 | 以 root 运行或通过 Docker group 授权，部署文档说明 |
| **TA5** | 10-100 节点规模下，内存元数据索引方案满足性能需求 | 大规模集群内存溢出或查询过慢 | NFR4 设定 50MB/1000-layer 上限，超出时告警 |
| **TA6** | 自签名 CA 证书可被注入到容器运行时的信任链 | HTTPS registry 拉取时 TLS 校验失败 | 提供 CA 证书分发脚本，支持 insecure-registry 降级 |
| **TA7** | OCI Distribution Spec v2 的 manifest 和 blob API 足以覆盖主流 registry | 某些 registry 的非标准扩展导致兼容性问题 | MVP 聚焦 Docker Hub + Harbor，其他 registry 列为 Phase 2 验证 |

### Constraints

| # | Constraint | Rationale |
|---|-----------|-----------|
| **C1** | 仅支持 Linux 平台 | 容器运行时存储结构依赖 Linux 文件系统 |
| **C2** | MVP 仅支持 Docker overlay2 存储驱动 | 占 90%+ 生产部署，containerd 列入 Phase 2 |
| **C3** | 不引入外部数据库依赖 | 边缘场景运维成本敏感，单二进制部署原则 |
| **C4** | 不实现认证/鉴权 | MVP 假设局域网信任模型，Phase 2 考虑 mTLS |
| **C5** | 不修改容器运行时存储 | 安全红线：只读访问，避免影响运行中的容器 |
| **C6** | Go 1.22+ | 最低版本要求，确保 slog、泛型和 ServeMux 路由增强可用 |

### Out of Scope (MVP)

- Containerd content store 适配
- DNS 劫持模式
- mDNS 零配置发现
- DHT + Gossip P2P 协议（Phase 2，MVP 使用静态发现 + HTTP API 查询）
- 镜像预热/推送
- 超级节点模式
- 自定义缓存淘汰策略
- 多集群联邦
- Web 管理界面
- Windows/macOS 支持

---

## 6. Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.22+ | 高性能网络编程，单二进制部署，ServeMux 路由增强 |
| HTTP | net/http (stdlib) | 参考 Spegel，标准库足够，避免框架锁定 |
| CLI Args | alexflint/go-arg | 参考 Spegel，环境变量 + CLI 参数一体化 |
| OCI Libraries | opencontainers/go-digest, image-spec | OCI 标准实现 |
| Monitoring | prometheus/client_golang | 行业标准 |
| Logging | log/slog + go-logr | 参考 Spegel，结构化日志 |
| TLS | golang.org/x/crypto | 动态证书生成 |
| Concurrency | golang.org/x/sync/errgroup | 参考 Spegel，goroutine 生命周期管理 |

### Project Structure (参考 Spegel)

```
outpostimage/
├── main.go                          # 入口，subcommand 路由，signal handling
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile
├── .goreleaser.yaml
│
├── internal/                        # 内部工具包（不导出）
│   ├── channel/                     # channel 工具（fan-in/gate 等）
│   ├── cleanup/                     # 清理逻辑
│   └── option/                      # 函数式选项模式
│
├── pkg/                             # 核心业务包（可导出）
│   ├── oci/                         # OCI 存储抽象层
│   │   ├── store.go                 # Store interface 定义
│   │   ├── docker.go                # Docker overlay2 适配器
│   │   ├── image.go                 # Image/Reference 数据结构
│   │   ├── distribution.go          # OCI Distribution Spec 常量/工具
│   │   ├── filter.go                # 镜像过滤器
│   │   └── memory.go                # 内存索引实现
│   │
│   ├── registry/                    # OCI Registry API Server
│   │   └── registry.go              # Handler: /v2/, manifests, blobs
│   │
│   ├── proxy/                       # HTTP Proxy 拦截层
│   │   ├── proxy.go                 # HTTP/HTTPS 代理服务
│   │   └── cert.go                  # 动态证书管理
│   │
│   ├── routing/                     # 节点发现与路由
│   │   ├── routing.go               # Router interface 定义
│   │   ├── static.go                # 静态节点配置
│   │   ├── balancer.go              # 负载均衡/节点选择
│   │   └── bootstrap.go             # 节点引导
│   │
│   ├── state/                       # 状态追踪（索引更新）
│   │   └── state.go                 # Track: 订阅存储事件 → 广播路由
│   │
│   ├── metrics/                     # Prometheus 指标定义
│   │   └── metrics.go
│   │
│   └── httpx/                       # HTTP 工具包
│       ├── error.go                 # OCI 标准错误响应
│       ├── header.go                # Header 常量
│       ├── range.go                 # Range 请求支持
│       └── response.go             # 响应工具
│
├── charts/                          # Helm Chart
│   └── outpostimage/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── daemonset.yaml
│           ├── service.yaml
│           ├── rbac.yaml
│           └── servicemonitor.yaml
│
├── deploy/                          # 非 Helm 部署方案
│   └── docker-compose.yaml
│
└── docs/                            # 文档
```

**与原 Brief 方案的关键变更**（参考 Spegel 实践）:

1. **去掉 `cmd/outpostimage/` 子目录** -- 参考 Spegel，`main.go` 直接放根目录，单二进制项目不需要 cmd 层级
2. **去掉 go-restful 框架** -- Spegel 证明标准库 `net/http` 足以实现 OCI Registry API，避免不必要的框架依赖
3. **去掉 Viper 配置框架** -- 参考 Spegel 使用 `alexflint/go-arg`，环境变量 + CLI 参数一体化，边缘场景更适合（K8s 通过 env 注入配置）
4. **新增 `internal/` 目录** -- 内部工具包不导出，参考 Spegel 的 channel/cleanup/option 模式
5. **新增 `pkg/httpx/`** -- 参考 Spegel，HTTP 工具包独立抽象（错误响应、Range 支持等）
6. **新增 `pkg/state/`** -- 参考 Spegel，状态追踪独立于存储层，负责"监听本地存储变更 → 更新路由广播"
7. **新增 Helm Chart** -- 参考 Spegel，K8s 部署的标准方式

---

## 7. Epic List

| Epic | Name | Covers | Dependencies | Complexity |
|------|------|--------|-------------|------------|
| **E1** | 项目脚手架与基础框架 | 项目结构、配置管理、go-restful 骨架、健康检查 | -- | S |
| **E2** | Docker 存储适配与元数据索引 | FR1, FR7, NFR1, NFR4 | E1 | L |
| **E3** | OCI Registry API Server | FR2, NFR7 | E2 | L |
| **E4** | HTTP Proxy 与证书管理 | FR3, FR4, NFR8 | E3 | XL |
| **E5** | 节点发现与 P2P 路由 | FR5, FR6, FR8, NFR5, NFR10 | E3 | XL |
| **E6** | 监控、部署与端到端验证 | FR9, NFR2, NFR3, NFR9 | E4, E5 | M |

### Dependency Graph

```
E1 ──> E2 ──> E3 ──┬──> E4 ──┐
                    │         ├──> E6
                    └──> E5 ──┘
```

Note: E4 and E5 can be developed in parallel after E3 is complete.

---

## 8. Epic Details

### Epic 1: 项目脚手架与基础框架

**Goal**: 建立可运行的 Go 服务骨架，为后续所有 Epic 提供基础设施。

#### Story 1.1: 初始化 Go 项目结构（参考 Spegel）

- 创建 `go.mod`（module: `github.com/outpostos/outpostimage`）
- 建立目录结构：`main.go`（根目录）、`internal/`（channel/cleanup/option）、`pkg/`（oci/registry/proxy/routing/state/metrics/httpx）、`deploy/`、`charts/`
- 创建 `main.go` 入口，使用 `alexflint/go-arg` 解析 subcommand（registry/configuration）
- 使用 `errgroup` 管理 goroutine 生命周期，signal.NotifyContext 处理 SIGTERM

**AC:**
- [ ] `go build .` 编译通过
- [ ] 目录结构参考 Spegel 布局

#### Story 1.2: 配置管理（go-arg，参考 Spegel）

- 集成 `alexflint/go-arg`，CLI 参数 + 环境变量一体化
- 定义配置结构体（参考 Spegel 的 `RegistryCmd`）：
  - `--registry-addr` / `REGISTRY_ADDR`（Registry API 监听地址，默认 `:5000`）
  - `--proxy-addr` / `PROXY_ADDR`（HTTP Proxy 监听地址，默认 `:3128`）
  - `--metrics-addr` / `METRICS_ADDR`（Metrics 监听地址，默认 `:9090`）
  - `--docker-root` / `DOCKER_ROOT`（Docker 存储根目录，默认 `/var/lib/docker`）
  - `--peers` / `PEERS`（静态节点列表）
  - `--log-level` / `LOG_LEVEL`（日志级别，默认 INFO）
- 静态节点列表支持通过配置文件加载（`--peers-file`）+ fsnotify 热更新

**AC:**
- [ ] 服务启动时解析 CLI 参数并打印关键配置
- [ ] 环境变量 `REGISTRY_ADDR=:6000` 覆盖默认值
- [ ] `--peers-file` 指定的文件变更后 10 秒内自动重新加载

#### Story 1.3: HTTP Server 骨架（net/http，参考 Spegel）

- 使用标准库 `net/http` + `http.NewServeMux()`
- 使用 `errgroup` 管理多个 HTTP server 的生命周期（registry server + metrics server）
- 实现 `GET /healthz` 返回 `{"status": "ok"}`
- 实现 `GET /readyz` 返回就绪状态
- 支持 graceful shutdown（signal.NotifyContext + server.Shutdown）

**AC:**
- [ ] `curl http://localhost:5000/healthz` 返回 200 + JSON
- [ ] 服务收到 SIGTERM 后完成在途请求再退出
- [ ] 启动日志包含 listen address 和 port

#### Story 1.4: 结构化日志（参考 Spegel）

- 集成 `log/slog` + `go-logr/logr`（参考 Spegel 的日志模式）
- JSON 输出到 stderr，日志级别通过 `--log-level` 控制
- 使用 `logr.FromContextOrDiscard(ctx)` 在各包中获取 logger
- 请求中间件记录 HTTP 访问日志（method, path, status, duration）

**AC:**
- [ ] 默认输出 JSON 格式日志到 stderr
- [ ] `--log-level=DEBUG` 时可见 debug 级别日志
- [ ] 每个 HTTP 请求产生一条访问日志

---

### Epic 2: Docker 存储适配与元数据索引

**Goal**: 读取 Docker overlay2 本地存储，构建内存 digest 索引，暴露查询 API。

#### Story 2.1: Store 接口定义（参考 Spegel pkg/oci）

- 在 `pkg/oci/` 中定义 `Store` interface（参考 Spegel 的 oci.Store）：
  - `Name() string`
  - `ListImages(ctx) ([]Image, error)`
  - `Resolve(ctx, ref) (digest.Digest, error)`
  - `GetManifest(ctx, dgst) ([]byte, string, error)`
  - `GetBlob(ctx, dgst) (io.ReadCloser, int64, error)`
  - `HasBlob(ctx, dgst) bool`
  - `Subscribe(ctx) (<-chan OCIEvent, error)` -- 存储变更事件流
- 定义 `Image`、`Reference`、`OCIEvent` 数据结构（参考 Spegel）

**AC:**
- [ ] 接口定义在 `pkg/oci/store.go`
- [ ] 包含完整的 GoDoc 注释
- [ ] 编译通过

#### Story 2.2: Docker overlay2 Store 实现（对标 Spegel pkg/oci/containerd.go）

- 在 `pkg/oci/docker.go` 中实现 `DockerStore`（实现 Store interface）
- 扫描 `/var/lib/docker/image/overlay2/imagedb/content/sha256/` 获取 image config
- 解析 image config 提取 manifest 引用
- 读取 `/var/lib/docker/image/overlay2/repositories.json` 建立 repo:tag -> digest 映射
- 通过 diffID -> chainID -> cacheID 算法定位 layer 文件路径
- 实现 `Subscribe()` 方法：使用 fsnotify 监听 imagedb 目录变更，发送 OCIEvent

**AC:**
- [ ] 给定本地已有 `nginx:latest` 镜像，`ListImages()` 返回包含该镜像的列表
- [ ] `GetManifest(ctx, digest)` 返回合法的 OCI manifest JSON
- [ ] `GetBlob(ctx, layerDigest)` 返回可读的 layer 数据流
- [ ] 对不存在的 digest，`HasBlob()` 返回 false

#### Story 2.3: State Tracker（参考 Spegel pkg/state）

- 在 `pkg/state/state.go` 中实现 `Track()` 函数（参考 Spegel 的 state.Track）
- 启动时调用 `Store.ListImages()` 全量扫描，构建初始索引
- 订阅 `Store.Subscribe()` 事件流，处理 CREATE/DELETE 事件的增量更新
- 将变更事件转化为路由广播（调用 Router.Advertise/Withdraw）

**AC:**
- [ ] 1000 layers 初始扫描完成 < 10 秒（NFR1）
- [ ] 索引内存占用 < 50MB（NFR4）
- [ ] 本地 `docker pull` 新镜像后，索引在 30 秒内自动更新并广播

#### Story 2.4: Digest 查询 API

- 实现 `GET /api/v1/digests` -- 返回本节点所有 digest 列表
- 实现 `GET /api/v1/digests/{digest}` -- 查询单个 digest 是否存在
- 支持 `?repo=<name>` 过滤

**AC:**
- [ ] API 返回 JSON 格式 digest 列表
- [ ] 查询不存在的 digest 返回 404
- [ ] 响应时间 < 10ms（内存查询）

---

### Epic 3: OCI Registry API Server

**Goal**: 本节点作为只读 OCI Registry，可被 `docker pull` 直接访问。

#### Story 3.1: OCI Version Check

- 实现 `GET /v2/` 返回 `{}` + 200
- 设置 `Docker-Distribution-API-Version: registry/2.0` header

**AC:**
- [ ] `curl http://localhost:8080/v2/` 返回 200
- [ ] 响应包含 `Docker-Distribution-API-Version` header

#### Story 3.2: Manifest API

- 实现 `GET /v2/{name}/manifests/{reference}` -- reference 支持 tag 和 digest
- 从 StorageAdapter 获取 manifest 数据
- 设置正确的 `Content-Type` 和 `Docker-Content-Digest` header

**AC:**
- [ ] `docker pull localhost:8080/library/nginx:latest` 的 manifest 阶段成功
- [ ] 通过 tag 和 digest 两种方式都能获取 manifest
- [ ] 不存在的镜像返回 404 + OCI error response

#### Story 3.3: Blob API

- 实现 `HEAD /v2/{name}/blobs/{digest}` -- 返回 blob 元信息
- 实现 `GET /v2/{name}/blobs/{digest}` -- 流式返回 blob 数据
- 支持 Range 请求（断点续传）

**AC:**
- [ ] HEAD 请求返回正确的 `Content-Length` 和 `Docker-Content-Digest`
- [ ] GET 请求流式传输 blob 数据，大文件不占满内存
- [ ] 不存在的 blob 返回 404
- [ ] 完整的 `docker pull localhost:8080/library/nginx:latest` 成功

#### Story 3.4: OCI Error Response 规范

- 实现 OCI 标准错误响应格式：`{"errors": [{"code": "...", "message": "...", "detail": ...}]}`
- 覆盖错误码：`NAME_UNKNOWN`, `MANIFEST_UNKNOWN`, `BLOB_UNKNOWN`

**AC:**
- [ ] 所有 404 响应符合 OCI error response JSON 格式
- [ ] error code 和 message 符合 OCI Distribution Spec 定义

---

### Epic 4: HTTP Proxy 与证书管理

**Goal**: 透明拦截容器运行时的镜像拉取请求。

#### Story 4.1: HTTP Proxy Server

- 实现 HTTP 代理服务（独立端口，如 3128）
- 处理 HTTP CONNECT 方法（HTTPS 隧道）
- 处理普通 HTTP 代理请求
- 识别 OCI Registry API 路径模式（`/v2/`）

**AC:**
- [ ] 设置 `HTTP_PROXY=http://localhost:3128` 后，`curl` 请求通过代理转发
- [ ] 非 OCI 请求透明转发到目标服务器
- [ ] OCI 请求被识别并标记

#### Story 4.2: 动态 HTTPS 证书生成

- 启动时生成自签名 CA（或加载已有 CA）
- 拦截 HTTPS CONNECT 请求时，为目标域名动态生成证书
- 证书缓存（避免重复生成）
- 支持 insecure-registry 白名单

**AC:**
- [ ] 首次访问 `docker.io` 时动态生成域名证书
- [ ] 再次访问同域名使用缓存证书
- [ ] insecure-registry 列表中的地址跳过 TLS 处理
- [ ] CA 证书持久化到配置目录

#### Story 4.3: 请求路由决策

- OCI 请求拦截后，查询本地元数据索引
- 本地可用：路由到本地 OCI Registry API
- 本地不可用：查询 P2P 节点或回退上游
- 记录路由决策日志

**AC:**
- [ ] 本地存在的镜像从本地获取，日志显示 `source=local`
- [ ] 本地不存在时转发到上游 registry，日志显示 `source=upstream`
- [ ] 路由决策延迟 < 5ms

#### Story 4.4: Docker Daemon 集成配置

- 编写 Docker daemon 代理配置指南
- 提供 systemd drop-in 配置模板
- 提供 CA 证书安装脚本

**AC:**
- [ ] 按照文档配置后，`docker pull nginx:latest` 流量经过 OutpostImage 代理
- [ ] 日志中可见拦截的请求详情

---

### Epic 5: 节点发现与 P2P 路由

**Goal**: 多节点间发现、查询、拉取镜像的 P2P 能力。

#### Story 5.1: Router 接口与静态路由（参考 Spegel pkg/routing）

- 在 `pkg/routing/routing.go` 中定义 `Router` interface（参考 Spegel）：
  - `Ready(ctx) (bool, error)`
  - `Lookup(ctx, key, count) (Balancer, error)`
  - `Advertise(ctx, keys) error`
  - `Withdraw(ctx, keys) error`
- 在 `pkg/routing/static.go` 中实现 `StaticRouter`（MVP 简化版本）
- 解析 `--peers` 或 `--peers-file` 配置的节点列表
- fsnotify 监听 peers 文件变更，10 秒内生效

**AC:**
- [ ] 配置 3 个 peer 地址，启动后节点列表正确加载
- [ ] 修改 peers 文件增删 peer，服务自动更新列表

#### Story 5.2: 节点健康检查

- 定期（可配置间隔，默认 30s）对所有 peer 执行 `GET /healthz`
- 连续 3 次失败标记为 `unhealthy`
- 恢复后自动重新标记为 `healthy`

**AC:**
- [ ] 健康节点状态为 `healthy`
- [ ] 关闭一个 peer 后，30-90 秒内被标记为 `unhealthy`
- [ ] 重新启动该 peer 后，自动恢复为 `healthy`

#### Story 5.3: 远端 Digest 查询

- 本地不存在请求的 digest 时，并行查询所有健康 peer 的 `GET /api/v1/digests/{digest}`
- 收集拥有该 digest 的节点列表
- 超时控制：单个查询 2 秒超时

**AC:**
- [ ] 查询返回拥有该 digest 的节点列表
- [ ] 任一节点超时不阻塞其他查询
- [ ] 所有节点都不持有时，返回空列表

#### Story 5.4: Balancer 与 P2P 镜像拉取（参考 Spegel pkg/routing/balancer.go）

- 在 `pkg/routing/balancer.go` 中实现 `Balancer` 接口（参考 Spegel）
- Lookup 返回 Balancer，调用方通过 `Balancer.Next()` 获取下一个 peer
- 从拥有 digest 的 peer 节点拉取 manifest 和 blob
- 拉取通过 peer 的 OCI Registry API，失败时 Balancer 自动切换到下一个 peer

**AC:**
- [ ] 通过 P2P 从 peer 拉取完整镜像成功
- [ ] 多节点可用时 Balancer 正确轮转
- [ ] 传输速率 > 100MB/s（1Gbps 网络，NFR6）

#### Story 5.5: 自动回退与指数退避

- P2P 拉取超时 5 秒后回退到上游 registry
- 失败节点进入退避期（1min -> 2min -> 5min -> 10min 上限）
- 回退过程对用户完全透明

**AC:**
- [ ] 所有 peer 不可用时，`docker pull` 仍成功
- [ ] 回退过程 `docker pull` 无错误输出
- [ ] 失败节点退避期内不被查询
- [ ] 退避周期正确递增

---

### Epic 6: 监控、部署与端到端验证

**Goal**: 生产就绪的可观测性、部署方案和端到端验证。

#### Story 6.1: Prometheus 指标集成

- 实现 FR9 中定义的所有指标
- 暴露 `GET /metrics` endpoint
- 请求中间件自动采集 duration 和 bytes 指标

**AC:**
- [ ] `curl http://localhost:9090/metrics` 返回 Prometheus 格式指标
- [ ] `docker pull` 一次后，`outpostimage_pulls_total` 计数器 +1
- [ ] `source` label 正确标记为 `local`、`p2p` 或 `upstream`

#### Story 6.2: Dockerfile 与容器镜像

- 多阶段构建 Dockerfile（builder + distroless/static）
- 支持 `linux/amd64` 和 `linux/arm64`
- 镜像大小 < 30MB

**AC:**
- [ ] `docker build` 成功
- [ ] 产出镜像可正常运行
- [ ] 镜像内仅包含二进制 + 默认配置

#### Story 6.3: Kubernetes DaemonSet 部署

- 编写 DaemonSet manifest：挂载 `/var/lib/docker`（只读）、配置 hostNetwork
- 编写 ConfigMap 示例
- 编写 Docker Compose 部署方案

**AC:**
- [ ] `kubectl apply` 部署到 K8s 集群成功
- [ ] DaemonSet 在每个节点运行一个 Pod
- [ ] Pod 可访问宿主机 Docker 存储

#### Story 6.4: 端到端集成测试

- 3 节点测试场景：
  1. Node 1 拉取 `nginx:latest`（从上游 registry）
  2. Node 2 拉取 `nginx:latest`（从 Node 1 P2P）
  3. Node 3 拉取 `nginx:latest`（从 Node 1 或 Node 2 P2P）
- 验证带宽节省（3 节点 140MB vs 420MB）
- 验证回退场景

**AC:**
- [ ] P2P 拉取成功率 100%
- [ ] P2P hit rate >= 60%
- [ ] 回退场景拉取成功
- [ ] 测试报告包含 pull duration 和 bandwidth 数据

---

## 9. MVP Success Criteria

| Metric | Target | Test Method |
|--------|--------|-------------|
| P2P hit rate | >= 60%（10 节点重复拉取场景） | 3 节点 nginx 集成测试 |
| 内网拉取速度 | > 100MB/s | 1Gbps 网络带宽测试 |
| 自动回退成功率 | 100% | 关闭所有 peer 后拉取测试 |
| 内存占用 | < 100MB idle, < 500MB peak | 1000 layer 压力测试 |
| 启动时间 | < 10 秒 | 1000 layer 冷启动计时 |
| 带宽节省 | >= 67%（3 节点场景） | 140MB vs 420MB 对比 |

### MVP Test Scenario

```bash
# 3-node cluster, nginx:latest (~140MB)
Node 1: docker pull nginx:latest    -> ~20s (external)
Node 2: docker pull nginx:latest    -> ~5s  (P2P from node 1)
Node 3: docker pull nginx:latest    -> ~5s  (P2P from node 1/2)
Total bandwidth: 140MB (vs 420MB without P2P) = 67% savings
```

---

## 10. Post-MVP Roadmap

**Phase 2**:
- Containerd content store 适配
- DNS 劫持模式
- DHT (libp2p-kad-dht) + Gossip (hashicorp/memberlist) P2P 协议
- HTTPS with mTLS

**Phase 3+**:
- 多集群联邦
- 镜像预热/推送
- Web 管理界面
- 高级可观测性（Grafana dashboard 模板）
