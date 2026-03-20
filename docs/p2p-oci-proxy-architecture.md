# P2P OCI Proxy — 架构设计文档

> **项目定位**：一个运行时无关的 P2P 镜像加速代理，通过 HTTP Proxy 拦截 OCI 请求，利用集群内节点间的 P2P 网络分发镜像层，显著降低跨节点、跨地域镜像拉取的带宽消耗与延迟。

---

## 一、背景与问题

### 1.1 现有方案的局限

[Spegel](https://github.com/XiaoMi/Spegel) 是目前最成熟的 Kubernetes P2P 镜像加速方案，其核心机制是将每个节点注册为 containerd 的 mirror endpoint，P2P 协调发生在 containerd 内部。

这一设计带来了根本性的运行时绑定问题：

| 依赖点 | Spegel 的实现 | 约束 |
|--------|-------------|------|
| 请求拦截 | containerd mirror 配置 | 仅支持 containerd |
| 本地库存查询 | containerd gRPC Content Store API | 仅支持 containerd |
| 配置方式 | hosts.toml / config.toml | 与 containerd 深度绑定 |

在边缘计算场景中，节点运行时并不统一——containerd、Docker、甚至定制化运行时都可能共存。Spegel 无法覆盖这类场景。

### 1.2 核心洞察

问题的本质在于 Spegel 将**拦截层**和**库存查询层**都实现在了运行时内部，导致运行时成为不可替换的基础设施。

如果将这两层**上移到 HTTP 协议层**，运行时差异就可以被彻底隔离：

- **拦截层** → 每节点本地 HTTP Proxy，所有运行时通过 `http_proxy` 环境变量接入
- **库存查询层** → 抽象 Content Store 接口，针对不同运行时提供适配器实现

---

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 运行时无关 | 同时支持 containerd、Docker，以及任何遵循 OCI Distribution Spec 的客户端 |
| 零侵入 | 不修改运行时配置文件，仅通过标准环境变量接入 |
| 高可用 | 每节点独立部署，无全局单点，单节点故障不影响集群 |
| 渐进降级 | P2P Miss 时自动回源，不阻断正常镜像拉取 |
| 轻量 | 单个 Go 二进制，资源占用极低 |

---

## 三、整体架构

```
┌──────────────────────────────────────────────────────────────────┐
│                          Kubernetes Node                          │
│                                                                   │
│  ┌─────────────┐    ┌─────────────┐                              │
│  │ containerd  │    │   dockerd   │  (任意 OCI 客户端)            │
│  └──────┬──────┘    └──────┬──────┘                              │
│         │ http_proxy        │ http_proxy                          │
│         └─────────┬─────────┘                                    │
│                   ↓                                               │
│         ┌─────────────────┐                                       │
│         │  Local Proxy    │  127.0.0.1:5000                      │
│         │                 │                                       │
│         │  ┌───────────┐  │                                       │
│         │  │  OCI 解析  │  │  识别 /v2/<name>/blobs/<digest>      │
│         │  └─────┬─────┘  │                                       │
│         │        ↓        │                                       │
│         │  ┌───────────┐  │                                       │
│         │  │ P2P 协调   │  │  查询集群内 peer 节点                  │
│         │  └─────┬─────┘  │                                       │
│         │     ┌──┴──┐     │                                       │
│         │   Hit │   │ Miss │                                       │
│         │     ↓ │   ↓     │                                       │
│         │  Peer │  上游   │                                       │
│         │  节点 │ Registry │                                       │
│         └───────────────── ┘                                       │
│                   ↓                                               │
│         ┌─────────────────┐                                       │
│         │  Content Store  │  本地库存管理 + P2P 广播               │
│         │  Abstraction    │                                       │
│         │  ┌───────────┐  │                                       │
│         │  │containerd │  │  gRPC Content Service                │
│         │  │ Adapter   │  │                                       │
│         │  ├───────────┤  │                                       │
│         │  │  Docker   │  │  扫描 layerdb 目录 + inotify         │
│         │  │ Adapter   │  │                                       │
│         │  └───────────┘  │                                       │
│         └─────────────────┘                                       │
└──────────────────────────────────────────────────────────────────┘
                     ↕ P2P 协议 (libp2p / 自实现)
┌──────────────────────────────────────────────────────────────────┐
│                    其他集群节点（相同结构）                         │
└──────────────────────────────────────────────────────────────────┘
```

---

## 四、核心组件详解

### 4.1 Local Proxy（本地代理）

每个节点运行一个监听在 `127.0.0.1:5000` 的 HTTP 代理进程。

**为何不需要 HTTPS/MITM：**

运行时（containerd/dockerd）只与本地回环地址通信，这段链路走 HTTP 完全合理，无需任何证书配置。对上游 Registry 的 HTTPS 请求由 Proxy 自己处理，运行时完全不感知。

```
运行时 → HTTP → 127.0.0.1:5000 (Proxy) → HTTPS → registry.example.com
```

**OCI 请求识别：**

Proxy 解析 OCI Distribution Spec 定义的路径，仅对 blob 请求介入 P2P 逻辑：

```
GET /v2/<name>/manifests/<ref>   → 直接回源（manifest 体积小，无需 P2P）
GET /v2/<name>/blobs/<digest>    → 触发 P2P 查询
HEAD /v2/<name>/blobs/<digest>   → 查询本地/P2P 库存
```

**流式转发：**

Proxy 采用流式处理，不在本地落盘后再转发，降低延迟和磁盘 IO：

```
Peer 节点 → stream → Proxy → stream → 运行时
                       ↓
                  同时写入本地 blob cache
```

---

### 4.2 P2P 协调层

负责节点间的库存发现与 blob 传输。

**库存广播：**

每个节点的 Proxy 在本地 Content Store 发生变化时，向 P2P 网络广播自己持有的 digest 列表。采用增量广播，非全量同步：

```
新增 blob → inotify 事件 / containerd event → 广播 {digest, nodeIP:port}
```

**节点发现：**

支持两种模式：

| 模式 | 适用场景 | 实现 |
|------|---------|------|
| Kubernetes Service 广播 | 标准 K8s 集群 | 通过 Endpoints API 枚举所有 Proxy 节点 |
| mDNS / 静态配置 | 边缘离线场景 | 本地网络广播或预配置 peer 列表 |

**查询流程：**

```
1. 收到 GET /v2/.../blobs/<digest>
2. 广播查询：集群内谁有 sha256:abc123？
3a. 有响应 → 建立连接，流式拉取 blob，同时缓存到本地 blob cache
3b. 无响应 → 回源上游 registry，拉取后缓存到本地 blob cache，并广播新增库存
```

---

### 4.3 Content Store 抽象层

这是实现运行时无关性的关键。定义统一接口，针对不同运行时提供适配器。

**接口定义：**

```go
type ContentStore interface {
    // 列出本节点所有已有的 blob digest
    List(ctx context.Context) ([]digest.Digest, error)

    // 判断某个 digest 是否存在
    Exists(ctx context.Context, dgst digest.Digest) (bool, error)

    // 读取某个 blob 的内容（用于向 peer 提供数据）
    Reader(ctx context.Context, dgst digest.Digest) (io.ReadCloser, error)

    // 订阅库存变化事件
    Subscribe(ctx context.Context) (<-chan Event, error)
}
```

**containerd Adapter：**

直接调用 containerd 的 gRPC Content Service API：

```go
func (a *ContainerdAdapter) List(ctx context.Context) ([]digest.Digest, error) {
    var digests []digest.Digest
    err := a.client.ContentStore().Walk(ctx, func(info content.Info) error {
        digests = append(digests, info.Digest)
        return nil
    })
    return digests, err
}
```

**Docker Adapter：**

扫描 Docker 的 layerdb 目录结构，无需任何 Docker API 权限：

```go
// Docker 存储路径：/var/lib/docker/image/overlay2/layerdb/sha256/<digest>
func (a *DockerAdapter) List(ctx context.Context) ([]digest.Digest, error) {
    entries, _ := os.ReadDir("/var/lib/docker/image/overlay2/layerdb/sha256")
    var digests []digest.Digest
    for _, e := range entries {
        if d, err := digest.Parse("sha256:" + e.Name()); err == nil {
            digests = append(digests, d)
        }
    }
    return digests, nil
}

// 增量更新：使用 inotify 监听目录变化
func (a *DockerAdapter) Subscribe(ctx context.Context) (<-chan Event, error) {
    watcher, _ := fsnotify.NewWatcher()
    watcher.Add("/var/lib/docker/image/overlay2/layerdb/sha256")
    // ... 将 inotify 事件转换为 Event
}
```

**文件兜底（任意运行时）：**

对于无法识别的运行时，Proxy 可以直接管理自己的 blob cache 目录作为库存来源，P2P 数据不依赖运行时，而是独立存储：

```
/var/lib/p2p-oci-proxy/blobs/sha256/<digest>
```

此时 Content Store 退化为 Proxy 自身管理的缓存，运行时从 Proxy 取到 blob 后自己写入本地存储，Proxy 不感知这个过程。

---

### 4.4 运行时接入方式

**containerd：**

```toml
# /etc/systemd/system/containerd.service.d/proxy.conf
[Service]
Environment="HTTP_PROXY=http://127.0.0.1:5000"
Environment="HTTPS_PROXY=http://127.0.0.1:5000"
Environment="NO_PROXY=localhost,127.0.0.1,10.0.0.0/8"
```

**Docker：**

```json
// /etc/docker/daemon.json 或 systemd drop-in
{
  "proxies": {
    "default": {
      "httpProxy": "http://127.0.0.1:5000",
      "httpsProxy": "http://127.0.0.1:5000",
      "noProxy": "localhost,127.0.0.1"
    }
  }
}
```

两种运行时均原生支持 proxy 配置，且 **无需重启运行时主进程**（Docker 23.0+ 支持热更新 proxy 配置）。

---

## 五、部署架构

```
┌─────────────────────────────────────────┐
│            Kubernetes Cluster            │
│                                          │
│  ┌─────────────────────────────────┐    │
│  │         DaemonSet               │    │
│  │   p2p-oci-proxy (每节点一个)     │    │
│  │                                  │    │
│  │  - 挂载 /var/lib/containerd      │    │
│  │  - 挂载 /var/lib/docker          │    │
│  │  - hostNetwork: true             │    │
│  │  - 监听 127.0.0.1:5000          │    │
│  └─────────────────────────────────┘    │
│                                          │
│  ┌──────────────────────────────────┐   │
│  │    ConfigMap / 初始化 DaemonSet   │   │
│  │  自动注入 proxy 环境变量到运行时   │   │
│  └──────────────────────────────────┘   │
└─────────────────────────────────────────┘
```

**节点间通信：**

Proxy 之间通过 Pod IP 直连（hostNetwork 模式），无需额外 Service。节点列表通过 Kubernetes Endpoints API 自动维护，节点上下线实时感知。

---

## 六、与 Spegel 的对比

| 维度 | Spegel | P2P OCI Proxy |
|------|--------|---------------|
| 运行时支持 | 仅 containerd | containerd、Docker、任意 OCI 客户端 |
| 拦截机制 | containerd mirror endpoint | 标准 HTTP Proxy |
| TLS 处理 | 运行时内部处理 | 本地回环 HTTP，无需 TLS |
| 库存查询 | containerd gRPC API | 可插拔适配器（containerd / Docker / 文件） |
| 配置方式 | hosts.toml（containerd 专有） | 标准 HTTP_PROXY 环境变量 |
| 边缘离线场景 | 依赖 K8s API，支持有限 | mDNS / 静态 peer 列表，天然支持 |
| 核心 P2P 逻辑与运行时耦合 | 是 | 否（通过接口隔离） |
| 实现复杂度 | 中 | 中（TLS 简化后，主要复杂度在适配器） |

---

## 七、关键设计决策记录

### 决策 1：为何用 HTTP Proxy 而不是 Mirror

Mirror 方式的拦截点在运行时内部，换运行时就必须重新适配。HTTP Proxy 是所有 OCI 客户端都遵循的标准协议接口，将拦截点上移到协议层，运行时差异自然消失。

### 决策 2：为何不需要 TLS MITM

本地回环通信（127.0.0.1）不需要 TLS，这绕开了 SSL 拦截最复杂的部分。Proxy 对外的 TLS 连接完全由 Proxy 自己管理，运行时不参与。

### 决策 3：为何 Docker Adapter 用文件扫描而不是 Docker API

Docker 没有提供等价于 containerd Content Service 的底层 API 来查询 blob digest。直接扫描 layerdb 目录：无需 Docker Socket 权限，无需 Docker API 版本兼容处理，且 inotify 的实时性完全满足需求。

### 决策 4：Manifest 不走 P2P

Manifest 体积极小（通常 < 10KB），P2P 的协调开销超过了传输收益。同时 Manifest 变更频率高，缓存一致性处理复杂。只对 blob（镜像层，通常 MB~GB 级别）走 P2P，收益最大化。

### 决策 5：文件兜底策略

对于无法识别的运行时，Proxy 以自身 blob cache 作为库存，不依赖运行时内部状态。这使得项目可以在任何 OCI 兼容环境中提供基础 P2P 能力，即使无法复用运行时已有的本地数据。

---

## 八、后续演进方向

- **Range Request 支持**：允许对单个 blob 进行分片，从多个 peer 并行下载，进一步提升大镜像层的拉取速度
- **BitTorrent 协议对齐**：参考 Dragonfly/Torrent 的分片策略，优化大规模集群下的 P2P 效率
- **镜像预热**：在 Pod 调度前主动将镜像层推送到目标节点，实现零延迟启动
- **Podman / CRI-O Adapter**：为其他主流运行时补充 Content Store Adapter
- **指标与可观测性**：P2P 命中率、节流带宽、回源次数等核心指标暴露为 Prometheus Metrics
