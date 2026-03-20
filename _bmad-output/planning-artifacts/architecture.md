---
stepsCompleted: [1, 2, 3, 4, 5, 6, 7, 8]
lastStep: 8
status: 'complete'
completedAt: '2026-03-05'
inputDocuments:
  - docs/prd.md
  - docs/brief.md
  - docs/architecture-optimization-depot-learnings.md
  - docs/brainstorming-session-results.md
workflowType: 'architecture'
project_name: 'outpostimage'
user_name: 'neov'
date: '2026-03-02'
---

# Architecture Decision Document

_This document builds collaboratively through step-by-step discovery. Sections are appended as we work through each architectural decision together._

## Project Context Analysis

### Requirements Overview

**Functional Requirements (9 FRs):**

OutpostImage 的功能需求围绕三个核心子系统展开：

1. **存储适配层** (FR1, FR7) — 通过 Store 接口抽象运行时差异。两个适配器实现：containerd (gRPC Content Service API) 和 Docker (overlay2 目录扫描 + inotify)。Observer 模式，只读访问运行时存储，不写入任何数据。
2. **代理拦截层** (FR2-FR5) — HTTP Proxy (:3128) 拦截 OCI 请求，P2P Registry Server (:5000) 暴露 OCI Distribution Spec v2 API。MITM 动态证书用于 HTTPS registry 拦截。
3. **P2P 协调层** (FR5, FR6, FR8) — 静态节点发现 (MVP)，内存元数据索引，P2P 路由与自动回退。P2P 协议参考 Spegel 实现。

**Non-Functional Requirements (10 NFRs):**

架构影响最大的 NFRs：
- **NFR1** (启动 <10s) + **NFR4** (索引 <50MB) → 内存索引方案，非外部存储
- **NFR5** (100% 回退可用) → 每条请求路径必须有 fallback 链
- **NFR6** (>100MB/s) → 流式传输，无磁盘缓冲
- **NFR7** (只读) → Observer 模式的安全红线
- **NFR9** (单二进制) → 无外部依赖，零运维开销

### 已确认的架构决策

通过 PRD 与架构设计文档的对比讨论和 Party Mode 审视，以下决策已确认：

| # | 决策 | 结论 | 依据 |
|---|------|------|------|
| D1 | TLS 处理 | MITM + 动态证书 | 99% 生产 registry 使用 HTTPS，必须拦截 CONNECT 隧道以识别 OCI 路径 |
| D2 | Manifest P2P | Manifest 走 P2P | 边缘离线场景需要完整 P2P 能力，不只是 blob |
| D3 | 存储模式 | Observer（只读） | 运行时自管存储，Proxy 不写入任何 blob cache |
| D4 | 适配器分类 | 仅 containerd + Docker | Docker adapter 即文件扫描方式，无需独立 fallback 类别 |
| D5 | 端口架构 | 单 Proxy 端口 :3128 + P2P 端口 :5000 | :3128 面向本地运行时，:5000 面向集群 P2P |
| D6 | P2P 协议 | 参考 Spegel | 不重新造轮子，直接参考 Spegel 的 P2P 实现模式 |

### Party Mode 补充结论

| # | 方向 | 决策 |
|---|------|------|
| PM1 | 冷启动优化 | 不考虑，非 MVP 关注点 |
| PM2 | 成功指标 | 关注效率（传输速度、带宽节省），不追踪命中率 |
| PM3 | 架构深度 | 先跑通完整流程再优化，避免过度设计影响架构决策 |
| PM4 | 适配器优先级 | containerd 先做（API 明确，Spegel 已验证），Docker 后做 |

### Scale & Complexity

- **Primary domain**: 分布式系统 / 网络代理 / OCI 生态
- **Complexity level**: Medium-High（P2P 协调 + 运行时适配 + MITM 代理三重复杂度）
- **Estimated architectural components**: 7 个核心 package (oci, registry, proxy, routing, state, metrics, httpx)
- **Node scale**: 10-100 节点 (MVP)，内存方案足够

### Technical Constraints & Dependencies

- Go 1.22+（slog、泛型、ServeMux 路由增强）
- 仅 Linux（依赖文件系统结构）
- 无外部数据库/中间件依赖
- 不实现认证/鉴权（MVP 假设局域网信任）
- 对运行时存储只读访问

### Cross-Cutting Concerns

1. **错误传播与降级** — 每个层级都需要 fallback 链（local → P2P → upstream）
2. **流式传输** — 全链路 stream，不缓冲到磁盘
3. **生命周期管理** — errgroup 统一管理所有 goroutine（Proxy/Registry/State/Metrics）
4. **可观测性** — Prometheus metrics 贯穿所有组件
5. **配置热更新** — peers 文件变更无需重启

## Starter Template Evaluation

### Primary Technology Domain

**Go 后端 / 分布式系统 / 网络代理** — 单二进制 daemon，无 Web 前端，无数据库。

### Starter Options Considered

| 方案 | 描述 | 评估 |
|------|------|------|
| `go mod init` + 手动搭建 | 从零开始，结合 Spegel 参考 | **选择** |
| gonew (Go 官方模板工具) | 从模板仓库克隆 | 无适用的 P2P proxy 模板 |
| 直接 fork Spegel | 基于 Spegel 代码修改 | 架构差异过大，不适合 |

### Selected Starter: `go mod init` + Spegel 结构参考

**Rationale:** OutpostImage 的架构与 Spegel 有本质差异（HTTP Proxy vs containerd mirror），无法直接复用代码。PRD 已定义完整项目结构，手动搭建即可。

**Initialization:** `go mod init github.com/outpostos/outpostimage`

**Tech Stack (已在 PRD 确认):**

| 类别 | 选择 | 来源 |
|------|------|------|
| Language | Go 1.22+ | PRD |
| HTTP | net/http stdlib | 参考 Spegel |
| CLI/Config | alexflint/go-arg | 参考 Spegel |
| Logging | log/slog + go-logr | 参考 Spegel |
| Monitoring | prometheus/client_golang | 行业标准 |
| OCI | opencontainers/go-digest, image-spec | OCI 标准 |
| Concurrency | golang.org/x/sync/errgroup | 参考 Spegel |
| TLS | golang.org/x/crypto | 动态证书 |

**Code Organization:** main.go 根目录, pkg/ 公共包 (oci/registry/proxy/routing/state/metrics/httpx), internal/ 私有包 (channel/cleanup/option)

**Build Tooling:** `go build .`, Makefile, 多阶段 Dockerfile, goreleaser

**Testing:** `go test ./...`, stdlib testing package

## Core Architectural Decisions

### Decision Priority Analysis

**Critical Decisions (Block Implementation):** D1-D6 已在 Step 2 确认。

**Important Decisions (Shape Architecture):**

### 内部数据架构

| 决策 | 选择 | 理由 |
|------|------|------|
| 内存索引结构 | `map` + `sync.RWMutex` | 类型安全，读多写少场景 RWMutex 性能优于 sync.Map，Spegel 同模式 |
| Event 模型 | channel-based (`<-chan OCIEvent`) | Go 惯用方式，Store.Subscribe() 返回 channel，errgroup 消费 |
| Image Reference 解析 | `distribution/reference` 库 | OCI 标准库，避免自行处理 tag/digest/域名解析边界情况 |

### API 与通信

| 决策 | 选择 | 理由 |
|------|------|------|
| OCI Error Response | 严格遵循 OCI Distribution Spec | 运行时依赖标准错误码进行重试决策 |
| P2P 节点间查询协议 | OCI Registry API 子集 (`/v2/`) | peer 直接暴露 Registry API，查询和拉取用同一套接口，参考 Spegel |
| Timeout 策略 | 分层配置 | proxy→upstream 30s, proxy→peer 5s, peer health check 2s |

### 基础设施与部署

| 决策 | 选择 | 理由 |
|------|------|------|
| CI/CD | GitHub Actions | 项目托管在 GitHub |
| Release | goreleaser | 参考 Spegel，自动化多平台构建 |
| 容器基础镜像 | distroless/static | 参考 Spegel，安全最小化 |
| Helm Chart 值管理 | 最小默认值 | MVP 只暴露必要参数 |

### Deferred Decisions (Post-MVP)

| 决策 | 延迟理由 |
|------|---------|
| mTLS 节点间认证 | MVP 假设局域网信任模型 |
| DHT/Gossip P2P 协议 | Phase 2，MVP 用静态发现 |
| 多集群联邦通信 | Phase 3+ |
| 镜像预热调度策略 | Phase 2 |
| Grafana dashboard 模板 | Post-MVP 可观测性增强 |

### Decision Impact Analysis

**Implementation Sequence:**
1. 项目脚手架 + 配置管理 (E1)
2. Store 接口 + containerd 适配器 (E2) — 优先 containerd
3. OCI Registry API Server (E3) — 依赖 Store
4. HTTP Proxy + MITM 证书 (E4) ∥ P2P 路由 (E5) — 可并行
5. 监控 + 部署 + E2E 测试 (E6)

**Cross-Component Dependencies:**
- Proxy (pkg/proxy) 依赖 Registry (pkg/registry) 的 handler 处理 OCI 请求
- State (pkg/state) 桥接 Store (pkg/oci) 和 Router (pkg/routing)
- Metrics (pkg/metrics) 被所有组件引用
- httpx (pkg/httpx) 提供 OCI 错误响应等公共工具

## Implementation Patterns & Consistency Rules

### Naming Patterns

**Go Code Naming:**

| 场景 | 规则 | 示例 |
|------|------|------|
| Package | 小写单词，不用下划线 | `oci`, `httpx`, `routing` |
| 导出类型 | PascalCase | `Store`, `DockerStore`, `OCIEvent` |
| 非导出 | camelCase | `digestIndex`, `peerList` |
| 接口 | 动词 or -er 后缀 | `Store`, `Router`, `Balancer` |
| 文件 | 小写，下划线分隔 | `docker_store.go`, `static_router.go` |
| 测试文件 | `_test.go` 后缀，同包 | `docker_store_test.go` |
| 常量 | PascalCase (导出) / camelCase (非导出) | `DefaultProxyAddr`, `maxRetries` |

**OCI API Naming:**

| 场景 | 规则 | 示例 |
|------|------|------|
| Registry API 路径 | 严格遵循 OCI Distribution Spec | `GET /v2/{name}/manifests/{reference}` |
| 内部 API 路径 | 小写，斜杠分隔 | `GET /api/v1/digests` |
| Metrics 命名 | `outpostimage_` 前缀，snake_case | `outpostimage_pulls_total` |
| Log 字段 | snake_case | `"digest": ..., "source": ..., "peer_addr": ...` |

### Structure Patterns

**Test Organization:**
- 测试与源码同目录，`_test.go` 后缀
- Table-driven tests (`[]struct{ name string; ... }`)
- 优先 stdlib `testing`
- Mock/Fake 放在同包 `mock_test.go` 或 `fake_test.go`

**Interface 定义位置:**
- 接口定义在使用者的包中（Go 惯例）
- 例外：`Store` 接口定义在 `pkg/oci/store.go`（核心领域接口）

**依赖方向:**
```
main.go → pkg/proxy → pkg/registry → pkg/oci
                    → pkg/routing
         pkg/state → pkg/oci (Subscribe)
                   → pkg/routing (Advertise/Withdraw)
         pkg/metrics ← (被所有包引用)
         pkg/httpx  ← (被 registry/proxy 引用)
```

### Format Patterns

**OCI Error Response (严格遵循 Spec):**
```json
{"errors": [{"code": "BLOB_UNKNOWN", "message": "blob unknown to registry", "detail": "sha256:abc123..."}]}
```

**JSON 字段:** camelCase（OCI Spec 要求）

### Communication Patterns

**OCIEvent 结构:**
```go
type OCIEvent struct {
    Type   EventType     // EventCreate | EventDelete
    Digest digest.Digest
    Image  string        // 可选，关联的镜像 ref
}
```

**Context 传播:**
- 所有公共方法第一参数为 `context.Context`
- 通过 `logr.FromContextOrDiscard(ctx)` 获取 logger
- 超时通过 `context.WithTimeout` 传递

**Channel 使用:**
- Subscribe 返回 `<-chan OCIEvent`（只读 channel）
- Channel 关闭由生产者负责
- 消费者用 `for range` 或 `select` + `ctx.Done()`

### Process Patterns

**Error Handling:**
- `fmt.Errorf("operation: %w", err)` 包装错误
- 不丢弃错误（`_ = fn()` 禁止，除非有注释说明）
- OCI API 层：错误转换为 OCI 标准错误码
- P2P 层：peer 错误触发 fallback，不向上传播为用户错误

**Fallback Chain:**
```
Local Store → P2P Peer (Balancer.Next()) → Upstream Registry
     ↓ fail        ↓ fail / timeout            ↓ fail
  skip to P2P   try next peer or upstream    return error
```

**Graceful Shutdown:**
- `signal.NotifyContext(ctx, SIGTERM, SIGINT)` 创建根 context
- `errgroup.Group` 管理所有 long-running goroutine
- 每个 server 在 `g.Go()` 中运行，context 取消时 `server.Shutdown(ctx)`

**Logging Levels:**

| Level | 用途 | 示例 |
|-------|------|------|
| Error | 需要运维关注 | upstream registry 不可达 |
| Warn | 可自动恢复 | peer 健康检查失败 |
| Info | 关键业务事件 | 服务启动、镜像拉取完成 |
| Debug | 开发调试 | 路由决策详情、digest 查询 |

### Anti-Patterns

| 避免 | 应该 |
|------|------|
| `sync.Mutex` 读多写少场景 | `sync.RWMutex` |
| 全局变量传递配置 | 构造函数注入 / functional options |
| `panic()` 处理业务错误 | 返回 `error` |
| goroutine 中忽略 context | `select` + `ctx.Done()` |
| 手动管理 goroutine 生命周期 | `errgroup` |
| `ioutil.ReadAll` 读取 blob | 流式 `io.Copy` |

## Project Structure & Boundaries

### Complete Project Directory Structure

```
outpostimage/
├── main.go                              # 入口，go-arg subcommand，signal handling，errgroup
├── go.mod
├── go.sum
├── Makefile                             # build, test, lint, docker
├── Dockerfile                           # 多阶段构建 (builder + distroless/static)
├── .goreleaser.yaml                     # 多平台 release
├── .github/
│   └── workflows/
│       ├── ci.yml                       # lint + test + build
│       └── release.yml                  # tag-triggered goreleaser
│
├── internal/                            # 内部工具（不导出）
│   ├── channel/
│   │   └── channel.go                   # fan-in, gate 等 channel 工具
│   ├── cleanup/
│   │   └── cleanup.go                   # defer cleanup 注册
│   └── option/
│       └── option.go                    # functional options pattern
│
├── pkg/                                 # 核心业务包
│   ├── oci/                             # [E2] 存储抽象层
│   │   ├── store.go                     # Store interface 定义
│   │   ├── containerd.go                # containerd adapter (gRPC Content Service)
│   │   ├── containerd_test.go
│   │   ├── docker.go                    # Docker overlay2 adapter (文件扫描 + inotify)
│   │   ├── docker_test.go
│   │   ├── image.go                     # Image, Reference, OCIEvent 数据结构
│   │   ├── distribution.go              # OCI Distribution Spec 常量
│   │   ├── filter.go                    # 镜像过滤器
│   │   └── memory.go                    # 内存索引 (map + RWMutex)
│   │
│   ├── registry/                        # [E3] OCI Registry API Server
│   │   ├── registry.go                  # Handler: /v2/, manifests, blobs
│   │   └── registry_test.go
│   │
│   ├── proxy/                           # [E4] HTTP Proxy 拦截层
│   │   ├── proxy.go                     # HTTP/HTTPS 代理，OCI 路径识别
│   │   ├── proxy_test.go
│   │   ├── cert.go                      # 动态 CA + 域名证书生成/缓存
│   │   └── cert_test.go
│   │
│   ├── routing/                         # [E5] 节点发现与路由
│   │   ├── routing.go                   # Router interface
│   │   ├── static.go                    # StaticRouter (peers 文件 + fsnotify)
│   │   ├── static_test.go
│   │   ├── balancer.go                  # Balancer interface + 实现
│   │   ├── balancer_test.go
│   │   └── bootstrap.go                # 节点引导/健康检查
│   │
│   ├── state/                           # [E2] 状态追踪
│   │   ├── state.go                     # Track(): 全量扫描 + Subscribe → Advertise/Withdraw
│   │   └── state_test.go
│   │
│   ├── metrics/                         # [E6] Prometheus 指标
│   │   └── metrics.go                   # outpostimage_* 指标定义
│   │
│   └── httpx/                           # HTTP 公共工具
│       ├── error.go                     # OCI 标准错误响应
│       ├── header.go                    # OCI Header 常量
│       ├── range.go                     # Range 请求支持
│       └── response.go                 # 响应工具
│
├── charts/                              # [E6] Helm Chart
│   └── outpostimage/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── daemonset.yaml
│           ├── service.yaml
│           ├── rbac.yaml
│           └── servicemonitor.yaml
│
├── deploy/                              # [E6] 非 Helm 部署
│   └── docker-compose.yaml
│
└── docs/
    ├── prd.md
    ├── brief.md
    └── p2p-oci-proxy-architecture.md
```

### Architectural Boundaries

**Port Boundaries:**

| 端口 | 角色 | 面向 | 协议 |
|------|------|------|------|
| :3128 | HTTP Proxy | 本地运行时 (127.0.0.1) | HTTP/HTTPS CONNECT |
| :5000 | P2P Registry | 集群内 peer 节点 | HTTP (OCI Distribution API) |
| :9090 | Metrics | Prometheus scraper | HTTP (/metrics) |

**Package Boundaries:**

```
                    ┌──────────┐
                    │ main.go  │  配置解析 + errgroup 编排
                    └────┬─────┘
              ┌──────────┼──────────┐
              ↓          ↓          ↓
        ┌──────────┐ ┌────────┐ ┌─────────┐
        │ proxy    │ │ state  │ │ metrics │
        │ :3128    │ │ bridge │ │ :9090   │
        └────┬─────┘ └───┬────┘ └─────────┘
             │           │
     ┌───────┴───┐   ┌───┴────┐
     ↓           ↓   ↓        ↓
┌──────────┐ ┌───────┐ ┌──────────┐
│ registry │ │  oci   │ │ routing  │
│ /v2/ API │ │ Store  │ │ Router   │
│ :5000    │ │ iface  │ │ iface    │
└──────────┘ └───┬────┘ └──────────┘
                 │
          ┌──────┴──────┐
          ↓             ↓
    ┌───────────┐ ┌───────────┐
    │containerd │ │  docker   │
    │ adapter   │ │  adapter  │
    └───────────┘ └───────────┘
```

### Epic to Structure Mapping

| Epic | Package | 核心文件 |
|------|---------|----------|
| E1 脚手架 | root, internal/ | main.go, go.mod, Makefile |
| E2 存储适配 | pkg/oci/, pkg/state/ | store.go, containerd.go, docker.go, state.go |
| E3 Registry API | pkg/registry/, pkg/httpx/ | registry.go, error.go |
| E4 HTTP Proxy | pkg/proxy/ | proxy.go, cert.go |
| E5 P2P 路由 | pkg/routing/ | routing.go, static.go, balancer.go |
| E6 监控部署 | pkg/metrics/, charts/, deploy/ | metrics.go, daemonset.yaml |

### Data Flow (三阶段生命周期)

```
=== Phase 1: 启动 (main.go → errgroup) ===

main.go
  ├─ 解析配置 (go-arg)
  ├─ 初始化 Store (containerd/docker adapter)
  ├─ 初始化 Router (StaticRouter, 加载 peers)
  │
  ├─ g.Go: state.Track(ctx, store, router)
  │         ├─ store.ListImages() → 全量扫描本地存储
  │         ├─ 构建 memory index (map[digest]→metadata)
  │         └─ router.Advertise(所有本地 digest) → 通告 P2P 网络
  │
  ├─ g.Go: registry server (:5000)  ← 等 state ready 后才标记 healthy
  ├─ g.Go: proxy server (:3128)
  └─ g.Go: metrics server (:9090)

=== Phase 2: 运行时后台 (state.Track 持续运行) ===

state.Track(ctx, store, router):
  for event := range store.Subscribe(ctx):
      switch event.Type:
      case EventCreate:
          memory index 添加 digest
          router.Advertise([]string{digest})
      case EventDelete:
          memory index 删除 digest
          router.Withdraw([]string{digest})

=== Phase 3: 请求处理 ===

docker pull nginx:latest
       │
       ↓ HTTP_PROXY=127.0.0.1:3128
  pkg/proxy (:3128)
    1. CONNECT → MITM → 解密 HTTPS
    2. 解析 OCI 路径: /v2/library/nginx/blobs/<digest>
    3. 查询 memory index (Store.HasBlob)
       ├─ HIT  → pkg/registry 从本地 Store 流式返回
       ├─ MISS → pkg/routing Lookup(digest)
       │   ├─ peer HIT → GET peer:5000/v2/…/blobs/ → stream → 运行时
       │   └─ peer MISS → GET upstream registry → stream → 运行时
       └─ 运行时写入自身存储
          └─ store.Subscribe() 检测 → Phase 2 循环处理
```

## Architecture Validation Results

### Coherence Validation ✅

**Decision Compatibility:** 所有技术选型互相兼容。Go 1.22+ 支持 slog/ServeMux/泛型；net/http 与 MITM 代理兼容；go-arg/errgroup/slog+go-logr 均经 Spegel 验证。

**Pattern Consistency:** 命名、context 传播、channel 模式、error 处理规则一致，无矛盾。

**Structure Alignment:** 目录结构支撑全部 6 个 Epic，package 依赖单向无循环。

### Requirements Coverage ✅

| 类型 | 总数 | 覆盖 | 状态 |
|------|------|------|------|
| Functional Requirements | 9 | 9 | ✅ 100% |
| Non-Functional Requirements | 10 | 10 | ✅ 100% |
| Epics | 6 | 6 | ✅ 100% |

### Implementation Readiness ✅

- [x] 所有 critical 决策已记录（D1-D6 + PM1-PM4）
- [x] Tech stack 完整指定
- [x] 命名/结构/格式/通信/流程模式全覆盖
- [x] Anti-patterns 定义
- [x] 完整目录树 + 边界图 + 数据流
- [x] Epic → Package 映射

### Gap Analysis

**Critical Gaps:** 无

**Important Gaps (实现时细化):**
1. containerd client 最小依赖集确认
2. CA cert 注入自动化脚本 (Story 4.4 覆盖)

### Architecture Readiness Assessment

**Status:** READY FOR IMPLEMENTATION | **Confidence:** HIGH

**First Priority:** Epic 1 Story 1.1 — `go mod init` + 项目目录结构 + main.go 骨架
