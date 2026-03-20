# Project Brief: OutpostImage

## Executive Summary

**OutpostImage** is a P2P container image sharing network designed for edge computing environments. It enables edge nodes within the same local network to share container images directly, leveraging high internal bandwidth while avoiding bottlenecks from limited external network connections.

The system solves the storage duplication problem inherent in traditional P2P solutions (like Dragonfly) by reusing existing OCI-compliant local storage from Docker/containerd runtimes. It provides a transparent, zero-configuration image distribution layer that transforms each edge node into an OCI Registry, allowing seamless image sharing across edge clusters without additional storage overhead.

**Key Value Proposition:** Eliminate redundant image downloads and storage duplication in edge environments by intelligently routing image pulls through local peers while maintaining full OCI Distribution Spec compliance.

## Problem Statement

### 当前痛点

在边缘计算场景中，容器镜像分发面临严重的性能和成本瓶颈：

**1. 带宽不对称问题**
- 边缘节点的内网带宽通常远大于出口带宽（例如：内网 10Gbps vs 出口 100Mbps）
- 多个节点同时从外网拉取相同镜像时，出口带宽成为瓶颈
- 大型镜像（数 GB）的拉取时间从分钟级延长到小时级

**2. 存储资源浪费**
- 传统 P2P 方案（如 Dragonfly）需要独立的缓存层，导致存储翻倍
- 容器运行时（Docker/containerd）已有 OCI 本地存储，但无法被其他节点复用
- 在存储受限的边缘设备上，这种浪费尤为严重

**3. 现有方案的局限性**
- **Registry Mirror**：只能配置单一 registry，缺乏灵活性和冗余
- **Dragonfly 等 P2P CDN**：存储翻倍、配置复杂、缺乏对边缘场景的针对性优化
- **Spegel**：深度绑定 containerd，无法支持 Docker 等其他运行时

### 问题影响

**定量影响（典型场景）：**
- 10 个边缘节点拉取 5GB 镜像，出口带宽浪费：50GB（vs P2P 仅需 5GB）
- 拉取时间：单节点 7 分钟 → 并发时 70+ 分钟
- 存储开销：传统 P2P 需要 100GB（10×5×2），本方案仅需 50GB

**业务影响：**
- 应用更新/扩容响应时间过长，影响边缘服务 SLA
- 带宽成本增加（尤其是按流量计费的边缘网络）
- 运维复杂度高（需要管理独立的 P2P 缓存服务）

### 为什么现在解决

1. **边缘计算普及加速**：5G、IoT 场景推动大规模边缘节点部署
2. **容器化成为边缘标准**：Kubernetes@Edge（K3s、KubeEdge）快速增长
3. **OCI 规范成熟**：标准化的存储和分发协议为复用提供了可能
4. **局域网信任模型**：边缘场景的局域网特性允许简化安全机制

## Proposed Solution

### 核心理念

OutpostImage 通过将每个边缘节点转化为 OCI Registry，构建去中心化的本地镜像共享网络。核心创新在于**直接复用容器运行时的本地存储**，而非创建独立缓存层。

### 技术方案概述

**1. 存储复用架构**
- 每个节点运行一个轻量级 OCI Registry 服务
- 通过**存储适配层**将 Docker/containerd 的本地存储暴露为 OCI Distribution API
- 零存储开销：镜像只存在于容器运行时，无需额外缓存

**2. 透明请求拦截**
- **DNS 劫持模式（优先）**：拦截 registry 域名解析，重定向到本地 P2P 网络
- **HTTP Proxy 模式（降级）**：通过 `HTTP_PROXY` 环境变量拦截镜像拉取请求
- 对容器运行时完全透明，无需修改配置

**3. 智能节点发现与路由**
- **渐进式演进路线**：
  - Phase 1 (MVP)：静态配置文件
  - Phase 2：超级节点模式（可选）
  - Phase 3：mDNS 零配置自动发现
- 本地优先策略：优先从内网节点拉取，失败时自动回退到外网 registry

**4. 元数据同步机制**
- 无数据库设计：启动时扫描本地存储，动态构建内存索引
- 通过自定义网络协议（基于 gRPC/Gossip）在节点间同步镜像元信息
- 双层抽象：应用层 MessageBus + 传输层 NetworkLayer

### 为什么会成功（差异化优势）

| 维度 | 传统 P2P（Dragonfly） | Registry Mirror | Spegel | **OutpostImage** |
|------|---------------------|----------------|--------|------------------|
| **存储开销** | 2x（独立缓存） | 2x | 1x | **1x** ✅ |
| **运行时支持** | 通用 | 通用 | 仅 containerd | **Docker + containerd** ✅ |
| **配置复杂度** | 高（服务端+客户端） | 低 | 中 | **极低（零配置目标）** ✅ |
| **边缘场景优化** | 无 | 无 | 部分 | **完全针对边缘** ✅ |
| **OCI 规范兼容** | 部分 | 是 | 是 | **是** ✅ |

**核心成功要素：**
1. **技术可行性已验证**：第一性原理分析确认三阶段（认证、manifest、layer）全部可本地化
2. **边缘场景完美契合**：局域网高带宽 + 信任模型允许简化设计
3. **标准化基础**：基于成熟的 OCI Distribution Spec v2，无需自创协议
4. **渐进式风险控制**：从简单静态配置开始，逐步演进到零配置

### 高层架构愿景

```
┌─────────────────────────────────────────────────────────┐
│                    EdgeOS Core (未来)                    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Image Plugin │  │ Mesh Plugin  │  │Scheduler Plug│  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│  ┌─────────────────────────────────────────────────┐   │
│  │         Core Layer (Discovery, gRPC, Auth)       │   │
│  └─────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘

MVP 阶段聚焦 Image Plugin：
┌─────────────────┐
│  OCI Registry   │ ← HTTP Proxy / DNS 拦截
│      API        │
├─────────────────┤
│  Storage Adapter│ ← 适配 Docker/containerd
├─────────────────┤
│ Node Discovery  │ ← 静态配置（MVP）
│  & Metadata     │
└─────────────────┘
```

## Target Users

### 主要用户群体：边缘计算平台运维团队

**人群画像：**
- **角色**：边缘计算平台架构师、DevOps 工程师、SRE
- **组织类型**：
  - IoT 平台提供商（智能制造、智慧城市、车联网）
  - CDN/边缘云服务商
  - 大型企业的边缘计算部门（零售、物流、能源）
- **规模特征**：管理 10-1000 个边缘节点的中小规模集群
- **技术背景**：熟悉 Kubernetes、Docker/containerd、容器镜像管理

**当前行为与工作流：**
1. 使用 K3s/KubeEdge 等轻量级 K8s 发行版部署边缘应用
2. 通过 Registry Mirror 或 Harbor 管理镜像分发
3. 面临镜像更新时的带宽瓶颈和长时间等待
4. 手动管理多个边缘站点的镜像预加载

**具体痛点：**
- **带宽成本**：每月为镜像拉取支付高额流量费用（按 GB 计费）
- **更新延迟**：应用更新需要等待所有节点完成镜像下载（可能数小时）
- **运维复杂度**：需要为 P2P 缓存服务（如 Dragonfly）维护额外基础设施
- **存储压力**：边缘设备存储有限（如工业网关 64GB），无法承受存储翻倍

**目标与成功标准：**
- 将镜像拉取时间从 30+ 分钟降低到 5 分钟以内
- 减少 80% 的外网带宽使用量
- 零额外存储开销
- "一键部署"的简单性（类似安装一个 DaemonSet）

---

### 次要用户群体：边缘应用开发者

**人群画像：**
- **角色**：应用开发者、AI 模型部署工程师
- **组织类型**：为边缘平台开发应用的 ISV、企业内部开发团队
- **技术背景**：熟悉容器化应用开发，但不关心底层镜像分发机制

**当前行为与工作流：**
1. 使用 `docker build` 构建应用镜像
2. 推送到私有/公有 registry
3. 通过 Kubernetes Deployment/DaemonSet 部署到边缘
4. 在开发/测试阶段频繁更新镜像版本

**具体痛点：**
- **开发迭代慢**：每次镜像更新后等待边缘节点拉取完成才能测试
- **调试困难**：无法快速验证新版本在多个边缘节点的行为
- **资源感知不足**：不了解镜像大小对边缘环境的影响

**目标与成功标准：**
- 完全透明：无需修改 Dockerfile 或部署 YAML
- 快速验证：镜像更新后 5 分钟内所有节点完成部署
- 可观测性：能看到镜像在哪些节点已缓存

## Goals & Success Metrics

### 业务目标

**1. 降低边缘镜像分发的总拥有成本（TCO）**
- **目标**：相比传统方案（外网直接拉取 + P2P 缓存），降低 60% 的总成本
- **衡量指标**：
  - 带宽成本降低 80%（流量费用 $/月）
  - 存储成本降低 50%（无需独立缓存层）
  - 运维成本降低 40%（减少 P2P 服务维护工作量）
- **目标时间**：MVP 发布后 3 个月内

**2. 提升边缘应用的部署与更新速度**
- **目标**：将镜像拉取时间从"分钟-小时级"降低到"秒-分钟级"
- **衡量指标**：
  - P90 镜像拉取时间 < 5 分钟（5GB 镜像，10 节点集群）
  - 冷启动场景：首个节点外网拉取 + 后续节点 P2P 复制 < 10 分钟
- **目标时间**：MVP 功能完成

**3. 建立边缘容器基础设施的技术影响力**
- **目标**：成为边缘 Kubernetes 生态的标准镜像加速方案
- **衡量指标**：
  - GitHub Stars > 500（6 个月内）
  - 至少 3 个外部组织/企业采用
  - 被 K3s/KubeEdge 社区推荐或集成
- **目标时间**：开源后 6-12 个月

### 用户成功指标

**4. 运维团队：零感知部署与运行**
- **部署便捷性**：从下载到运行 < 10 分钟（单条 `kubectl apply` 命令）
- **稳定性**：99% 请求成功率（P2P 失败自动回退外网）
- **可观测性**：提供 Prometheus 指标，集成 Grafana Dashboard

**5. 开发者：透明加速**
- **兼容性**：100% 兼容标准容器工作流（docker pull/push、kubectl apply）
- **透明性**：无需修改 Dockerfile、镜像 tag、部署 YAML
- **可见性**：通过 CLI/API 查看镜像在集群中的分布状态

### 关键性能指标（KPIs）

| KPI | 定义 | 目标值 | 测量方式 |
|-----|------|--------|----------|
| **P2P 命中率** | 从本地节点成功拉取的请求比例 | > 70% | `(local_pulls / total_pulls) * 100` |
| **带宽节省率** | 相比外网直接拉取节省的流量 | > 80% | `(saved_bandwidth / total_bandwidth) * 100` |
| **平均拉取时间** | 单个 layer 的平均下载时间 | < 30 秒/GB | `sum(pull_duration) / sum(layer_size)` |
| **存储效率** | 实际存储占用 vs 缓存方案 | 1x（无额外开销） | `storage_used / container_runtime_storage` |
| **启动延迟** | OutpostImage 服务启动到可用 | < 10 秒 | `time(service_ready - service_start)` |
| **元数据同步延迟** | 新镜像在集群内同步时间 | < 5 秒 | `time(metadata_synced - image_pulled)` |

## MVP Scope

### 核心功能（必须有）

**1. 存储适配层 - Docker Overlay2 支持**
- **功能描述**：读取 Docker overlay2 存储，将本地镜像暴露为 OCI Registry API
- **实现要点**：
  - 解析 `/var/lib/docker/image/overlay2/imagedb/` 获取镜像元信息
  - 通过 digest 映射到 `/var/lib/docker/overlay2/` 的 layer 数据
  - 实现 OCI Distribution Spec v2 核心 endpoints（`/v2/`, `/v2/<name>/manifests/<reference>`, `/v2/<name>/blobs/<digest>`）
- **成功标准**：能够通过 `curl` 访问本地 Registry API 获取已存在的镜像 manifest 和 layer

**2. HTTP Proxy 请求拦截**
- **功能描述**：通过 `HTTP_PROXY` 环境变量拦截容器运行时的镜像拉取请求
- **实现要点**：
  - 识别 OCI Registry 请求（`/v2/` 路径）
  - 查询本地元数据索引，判断是否有 P2P 节点可用
  - 路由到本地节点或回退到外网 registry
- **成功标准**：设置 `HTTP_PROXY` 后，`docker pull` 能自动从本地 P2P 网络拉取已缓存镜像

**3. 静态节点发现**
- **功能描述**：通过配置文件手动指定集群内的节点列表
- **实现要点**：
  - 配置格式：YAML 文件，包含节点 IP/域名、端口
  - 支持热更新（文件变化时自动重载，无需重启）
  - 健康检查：定期 ping 节点，标记不可用节点
- **成功标准**：修改配置文件后 10 秒内生效，不可用节点自动剔除

**4. 元数据内存索引**
- **功能描述**：启动时扫描本地镜像，构建内存索引（digest → 本节点）
- **实现要点**：
  - 启动扫描：遍历 Docker 存储，提取所有 layer digest
  - 数据结构：`map[digest]LayerInfo`（包含大小、路径）
  - 增量更新：文件监听机制（inotify/fsnotify）检测新镜像
- **成功标准**：1000 个 layer 的扫描时间 < 5 秒，内存占用 < 50MB

**5. 基础监控指标**
- **功能描述**：暴露 Prometheus 格式的监控指标
- **实现要点**：
  - 核心指标：
    - `outpostimage_pulls_total{source="local|remote"}` - 拉取次数
    - `outpostimage_bytes_transferred{source="local|remote"}` - 传输字节数
    - `outpostimage_pull_duration_seconds` - 拉取耗时直方图
    - `outpostimage_metadata_sync_duration_seconds` - 元数据同步延迟
  - HTTP endpoint：`/metrics`
- **成功标准**：Prometheus 能成功抓取指标，Grafana 能显示 P2P 命中率

**6. 自动回退机制**
- **功能描述**：P2P 失败时自动回退到原始 registry
- **实现要点**：
  - 超时控制：本地节点请求超时 5 秒后回退
  - 指数退避：失败节点标记后 1min/2min/5min 逐步延长重试间隔
  - 透明性：回退过程对用户完全透明
- **成功标准**：所有 P2P 节点不可用时，镜像拉取仍能 100% 成功（从外网）

---

### MVP 排除范围（不在此版本）

**明确不做的功能：**
- ❌ **认证/鉴权**：局域网信任模型，不实现用户认证
- ❌ **加密传输**：HTTP 明文传输（HTTPS 留待后续）
- ❌ **DNS 劫持模式**：优先度降低，HTTP Proxy 已足够
- ❌ **mDNS 自动发现**：静态配置即可满足 MVP 需求
- ❌ **Containerd 支持**：聚焦 Docker，containerd 留待后续
- ❌ **镜像预加载/推送**：只做被动拉取
- ❌ **超级节点模式**：去中心化架构，不引入中心节点
- ❌ **镜像缓存淘汰策略**：复用容器运行时的镜像管理

---

### MVP 成功标准

**端到端验证场景：**

```bash
# 场景：3 节点集群，拉取 nginx:latest (约 140MB)

# 节点 1（冷启动）
docker pull nginx:latest
# → 从外网拉取，耗时 ~20 秒
# → OutpostImage 扫描新镜像，更新内存索引

# 节点 2（热启动）
docker pull nginx:latest
# → HTTP Proxy 拦截请求
# → 查询发现节点 1 有缓存
# → 从节点 1 拉取，耗时 ~5 秒（内网 1Gbps）
# → P2P 命中率：100%

# 节点 3（同时拉取）
docker pull nginx:latest
# → 从节点 1 或节点 2 拉取
# → 耗时 ~5 秒

# 验证指标：
# - 总带宽使用：140MB（vs 420MB 无 P2P）
# - 节省率：67%
# - 节点 2/3 拉取时间 < 10 秒
```

**定量验收标准：**
- ✅ P2P 命中率 ≥ 60%（10 节点重复拉取场景）
- ✅ 内网拉取速度 > 100MB/s（1Gbps 网络）
- ✅ 自动回退成功率 100%（模拟所有节点离线）
- ✅ 内存占用 < 100MB（1000 layer 索引）
- ✅ 启动时间 < 10 秒（冷启动到服务可用）

## Post-MVP Vision

### Phase 2 功能（近期优先）

**1. Containerd 存储适配支持**
- **价值**：覆盖 K3s、KubeEdge 等主流边缘 Kubernetes 发行版
- **实现要点**：
  - 适配 containerd content store（`/var/lib/containerd/io.containerd.content.v1.content/`）
  - 统一抽象层：`StorageAdapter` 接口支持多运行时
  - 支持同一节点同时启用 Docker + containerd
- **优先级**：高（很多边缘场景使用 K3s）
- **预计工作量**：2-3 周

**2. DNS 劫持透明拦截**
- **价值**：实现真正的零配置，无需设置 `HTTP_PROXY`
- **实现要点**：
  - 集成 CoreDNS/dnsmasq，劫持 registry 域名解析
  - 动态 DNS 记录管理（指向本地 P2P 节点）
  - 负载均衡：多个节点有缓存时的智能选择
- **优先级**：高（用户体验提升显著）
- **预计工作量**：2-3 周

**3. mDNS 零配置节点发现**
- **价值**：消除静态配置文件，真正做到"安装即用"
- **实现要点**：
  - 基于 mDNS 协议（或 hashicorp/mdns 库）
  - 服务广播：`_outpostimage._tcp.local`
  - 健康检查与自动剔除
- **优先级**：中（静态配置已可用，零配置是体验优化）
- **预计工作量**：1-2 周

**4. 双层网络抽象（MessageBus + NetworkLayer）**
- **价值**：为未来扩展到 EdgeOS Core 插件化架构奠定基础
- **实现要点**：
  - 应用层：统一消息总线（元数据同步、事件通知）
  - 传输层：可插拔网络协议（gRPC Stream、Gossip）
  - 解耦镜像服务与通信机制
- **优先级**：中（架构演进，不影响核心功能）
- **预计工作量**：2-3 周

---

### 长期愿景（1-2 年）

**保持专注：边缘场景的最佳镜像分发方案**

OutpostImage 将持续聚焦于成为边缘容器镜像分发的事实标准，通过开放接口支持生态集成：

**核心演进方向：**
- **多运行时统一支持**：Docker、containerd、CRI-O 等主流运行时
- **生产级稳定性**：完善的监控、告警、故障恢复机制
- **性能极致优化**：智能路由、并行传输、增量同步
- **开放接口**：提供 gRPC/REST API 供其他系统集成

**架构定位：**
```
OutpostImage 服务
├─ 核心能力
│  ├─ 存储适配层（多运行时）
│  ├─ P2P 网络层（节点发现与通信）
│  ├─ OCI Registry API
│  └─ 监控与管理
├─ 对外接口（预留但不过度设计）
│  ├─ 镜像元数据查询 API
│  ├─ 节点状态查询 API
│  └─ 事件通知 Webhook
```

**设计原则：**
- 保持独立性：可单独部署，不依赖其他组件
- 接口开放：提供必要的查询接口，但不主动实现复杂协作
- 职责单一：只做镜像分发，做到极致

---

### 扩展场景探索

**1. 混合云镜像加速**
- 边缘节点 + 云端节点混合 P2P 网络
- 云端作为"热备节点"，边缘离线时兜底
- 适用场景：广域网边缘集群（多地域分布）

**2. AI 模型分发**
- 复用 P2P 机制分发大型 AI 模型（数十 GB）
- 支持 HuggingFace/ModelScope 等模型仓库
- 适用场景：边缘 AI 推理集群

**3. OCI Artifacts 通用分发**
- 扩展到 Helm Charts、WASM 模块等 OCI Artifacts
- 成为边缘场景的通用二进制分发层
- 适用场景：边缘应用商店、固件更新

**4. 性能优化探索**
- 智能节点选择算法（基于网络延迟、负载、带宽）
- Layer 级别的并行传输
- 增量同步机制（只传输变化的 layer）

---

### 演进路线图

```
MVP (3 个月)
├─ Docker overlay2 支持
├─ HTTP Proxy 拦截
└─ 静态配置

Phase 2 (6 个月)
├─ Containerd 支持
├─ DNS 劫持模式
├─ mDNS 零配置
└─ 网络抽象层

Phase 3 (12 个月)
├─ 多运行时统一管理
├─ 生产级监控告警
├─ 性能优化（并行传输、智能路由）
└─ 开放 API 体系

Long-term (2 年+)
├─ 边缘镜像分发标准化
├─ AI 模型分发支持（实验性）
└─ 社区生态建设
```

## Technical Considerations

### 平台需求

**目标平台：**
- **Linux x86_64 / ARM64**：覆盖主流边缘设备（工业网关、边缘服务器）
- **容器运行时**：
  - Docker 19.03+（overlay2 存储驱动）
  - Containerd 1.6+（Phase 2）
- **网络环境**：
  - 内网带宽：≥ 1Gbps
  - 局域网可达性：节点间可直接 TCP 连接

**性能要求：**
- 镜像扫描：1000 layers < 5 秒
- 服务启动：< 10 秒
- 内存占用：< 100MB（idle）/ < 500MB（高负载）
- CPU 占用：< 5%（idle）/ < 50%（传输中）

---

### 技术栈偏好

**编程语言：Go**
- **理由**：
  - 容器生态主流语言，与 Docker/containerd 生态契合
  - 优秀的并发性能（goroutine）适合 P2P 网络
  - 静态编译，单二进制部署，适合边缘场景
  - 丰富的库支持（OCI、gRPC、Prometheus）

**核心依赖：**
- **OCI 规范实现**：`github.com/opencontainers/go-digest`、`github.com/opencontainers/image-spec`
- **HTTP 服务框架**：`github.com/emicklei/go-restful`
  - 选择理由：企业级 RESTful 框架，完善的路由和过滤器支持，适合实现标准 OCI Registry API
  - 支持 OpenAPI 文档自动生成，便于 API 文档维护
- **HTTP Proxy**：标准库 `net/http` + `httputil.ReverseProxy`
- **存储访问**：标准库 `os`、`filepath`（直接读取文件系统）
- **监控**：`github.com/prometheus/client_golang`
- **配置管理**：`github.com/spf13/viper`（支持 YAML 热更新）
- **文件监听**：`github.com/fsnotify/fsnotify`

**避免的技术选择：**
- ❌ DHT（如 libp2p）：过于复杂，边缘小规模集群不需要
- ❌ 独立数据库（MySQL/PostgreSQL）：增加运维复杂度
- ❌ 微服务架构：单体服务更适合边缘资源受限场景

---

### 架构考虑

**1. 存储适配层设计**

```go
// 抽象接口
type StorageAdapter interface {
    ListImages() ([]ImageMetadata, error)
    GetManifest(repo, tag string) ([]byte, error)
    GetBlob(digest string) (io.ReadCloser, error)
}

// Docker overlay2 实现
type DockerOverlay2Adapter struct {
    rootPath string // /var/lib/docker
}

// Containerd 实现（Phase 2）
type ContainerdAdapter struct {
    rootPath string // /var/lib/containerd
}
```

**关键决策点：**
- Docker overlay2 的 imagedb 包含完整的 manifest 信息，可直接解析
- Layer digest 到文件路径的映射需要通过 chainID 计算（参考 Spegel 实现）
- 只读访问，不修改容器运行时存储

**2. HTTP Proxy 实现**

```go
// 代理架构
Client (docker pull)
  → HTTP_PROXY=localhost:8080
  → OutpostImage Proxy
      ├─ 解析请求（registry、repo、digest）
      ├─ 查询本地索引
      │   ├─ 有缓存 → 路由到 P2P 节点
      │   └─ 无缓存 → 回退到原始 registry
      └─ 返回响应
```

**技术细节：**
- 实现标准 HTTP CONNECT 隧道（支持 HTTPS registry）
- 识别 OCI Registry API 路径模式（`/v2/*/manifests/*`、`/v2/*/blobs/*`）
- 维持连接复用（Keep-Alive）降低延迟

**3. 节点发现与元数据同步**

**MVP：静态配置**
```yaml
# config.yaml
nodes:
  - host: 192.168.1.10
    port: 5000
  - host: 192.168.1.11
    port: 5000
```

**Phase 2：mDNS**
```go
// 服务广播
mdns.Publish("_outpostimage._tcp", 5000)

// 服务发现
peers := mdns.Discover("_outpostimage._tcp")
```

**元数据交换协议（简化版）：**
- 每个节点维护本地 digest 列表
- 定期向已知节点查询：`GET /api/v1/digests`
- 返回 JSON：`{"digests": ["sha256:abc...", "sha256:def..."]}`
- 无需同步完整镜像元信息，按需查询

---

### 仓库结构

```
outpostimage/
├─ cmd/
│  └─ outpostimage/          # 主程序入口
├─ pkg/
│  ├─ storage/               # 存储适配层
│  │  ├─ adapter.go          # 接口定义
│  │  ├─ docker.go           # Docker overlay2 实现
│  │  └─ containerd.go       # Containerd 实现（Phase 2）
│  ├─ proxy/                 # HTTP Proxy
│  │  ├─ server.go
│  │  └─ router.go
│  ├─ registry/              # OCI Registry API
│  │  ├─ handler.go
│  │  └─ blobs.go
│  ├─ discovery/             # 节点发现
│  │  ├─ static.go           # 静态配置
│  │  └─ mdns.go             # mDNS（Phase 2）
│  ├─ metadata/              # 元数据索引
│  │  ├─ index.go
│  │  └─ sync.go
│  └─ metrics/               # 监控指标
│     └─ prometheus.go
├─ config/
│  └─ config.yaml            # 配置文件示例
├─ deploy/
│  ├─ docker-compose.yaml    # Docker 部署
│  └─ kubernetes/            # K8s DaemonSet
│     └─ daemonset.yaml
├─ docs/                     # 文档
├─ test/                     # 集成测试
└─ README.md
```

---

### 部署架构

**Docker Compose 部署（开发/测试）：**
```yaml
services:
  outpostimage:
    image: outpostimage:latest
    volumes:
      - /var/lib/docker:/var/lib/docker:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports:
      - "5000:5000"   # OCI Registry API
      - "8080:8080"   # HTTP Proxy
      - "9090:9090"   # Metrics
    environment:
      - HTTP_PROXY=http://localhost:8080
```

**Kubernetes DaemonSet 部署（生产）：**
```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: outpostimage
spec:
  template:
    spec:
      hostNetwork: true
      volumes:
        - name: docker-storage
          hostPath:
            path: /var/lib/docker
            type: Directory
      containers:
        - name: outpostimage
          image: outpostimage:latest
          volumeMounts:
            - name: docker-storage
              mountPath: /var/lib/docker
              readOnly: true
```

---

### 安全考虑

**MVP 阶段（局域网信任模型）：**
- ❌ 不实现认证/鉴权
- ❌ HTTP 明文传输
- ⚠️ 假设边缘局域网是可信环境

**Phase 2+ 安全增强：**
- mTLS：节点间加密通信
- Token 认证：访问 OCI Registry API 需要 token
- 网络隔离：绑定到内网网卡，不暴露到公网

**已知风险缓解：**
- 只读访问容器存储，不会破坏运行时
- 自动回退机制保证可用性
- 监控指标暴露异常行为

## Constraints & Assumptions

### 约束条件

**预算约束：**
- **开发资源**：2-3 名工程师，3 个月 MVP 开发周期
- **基础设施成本**：开源项目，无需云服务成本（本地测试环境）
- **硬性约束**：必须保持零额外存储开销（核心价值主张）

**时间约束：**
- **MVP 交付**：3 个月内完成核心功能
- **首个生产用户**：6 个月内获得至少 1 个外部组织试用
- **市场窗口**：边缘容器化快速增长期（2025-2026），需尽快建立市场认知

**技术约束：**
- **只读访问**：只能读取容器运行时存储，不能修改（避免破坏运行时）
- **Linux 限定**：暂不支持 Windows 容器（边缘场景主要是 Linux）
- **局域网依赖**：节点间必须网络可达（不支持 NAT 穿透）
- **文件系统依赖**：需要直接访问宿主机的 `/var/lib/docker` 或 `/var/lib/containerd`

**资源约束：**
- **内存上限**：单节点服务内存占用 < 500MB（边缘设备资源有限）
- **存储上限**：元数据索引 < 100MB（1000 layers 场景）
- **CPU 上限**：idle 时 < 5% CPU（不影响业务容器）

---

### 关键假设

**技术假设：**

1. **Docker overlay2 结构稳定性**
   - 假设：Docker overlay2 的 imagedb/layerdb 结构在 19.03-24.x 版本间保持相对稳定
   - 验证方法：测试覆盖 Docker 19.03、20.10、24.0 三个主要版本
   - 失效后果：需要重新设计存储适配层或改用 Docker API

2. **OCI 规范兼容性**
   - 假设：Docker/containerd 严格遵循 OCI Distribution Spec v2
   - 验证方法：使用 OCI conformance test 验证兼容性
   - 失效后果：某些边缘情况可能无法正确处理

3. **ChainID 计算可复现**
   - 假设：通过公开的算法可以准确计算 Docker 的 layer chainID
   - 验证方法：对比计算结果与 Docker 实际存储路径
   - 失效后果：无法定位 layer 文件，功能完全失效

**场景假设：**

4. **边缘集群规模**
   - 假设：典型边缘集群 10-100 个节点，不超过 1000 个节点
   - 依据：边缘计算场景的实际部署规模（单个站点）
   - 影响：简化了节点发现和元数据同步设计

5. **内网带宽优势**
   - 假设：边缘集群内网带宽 ≥ 1Gbps，远大于出口带宽（100Mbps-1Gbps）
   - 依据：现代数据中心/边缘站点的标准网络配置
   - 影响：P2P 加速效果显著（10x+ 速度提升）

6. **镜像复用率**
   - 假设：70%+ 的镜像拉取发生在至少一个节点已有缓存之后
   - 依据：容器化应用的典型部署模式（多副本、滚动更新）
   - 影响：决定了 P2P 命中率目标设定

**用户假设：**

7. **技术能力**
   - 假设：目标用户熟悉 Docker/Kubernetes 基础概念和命令行操作
   - 依据：边缘计算运维团队的技术背景
   - 影响：文档可以假设一定的技术基础，无需从零讲解容器概念

8. **局域网信任模型**
   - 假设：边缘集群的局域网是相对可信的环境（非公网、非多租户）
   - 依据：边缘计算场景的典型网络拓扑
   - 影响：MVP 阶段可以不实现认证和加密

9. **配置简单性优先**
   - 假设：用户更看重"开箱即用"，而非复杂的配置选项
   - 依据：边缘场景的运维资源有限
   - 影响：设计决策倾向于约定优于配置

**生态假设：**

10. **Docker 仍是主流**
    - 假设：边缘场景中 Docker 仍占主导地位（vs containerd）
    - 依据：边缘设备的软件更新较慢，Docker 生态更成熟
    - 影响：MVP 优先支持 Docker，containerd 放到 Phase 2

11. **K3s/KubeEdge 增长**
    - 假设：边缘 Kubernetes（K3s、KubeEdge）在未来 2 年内快速增长
    - 依据：CNCF 边缘计算趋势报告
    - 影响：产品定位为 K8s 生态组件，而非独立工具

12. **开源社区协作**
    - 假设：开源社区能够贡献代码、报告问题、提供测试环境
    - 依据：容器生态的开源文化
    - 影响：项目采用开源协议（Apache 2.0/MIT），积极建设社区

---

### 假设验证计划

**高风险假设的验证优先级：**

1. **立即验证（在 MVP 开发前）：**
   - ChainID 计算可复现性（技术 POC）
   - Docker overlay2 结构在多版本的稳定性（环境测试）

2. **MVP 期间验证：**
   - 内网带宽优势的实际加速效果（性能测试）
   - 镜像复用率假设（模拟真实部署场景）

3. **生产环境验证：**
   - 局域网信任模型的安全可接受性（用户反馈）
   - 边缘集群规模假设（真实用户数据）

## Risks & Open Questions

### 关键风险

**1. Docker 存储结构变化风险** ⚠️ 高风险
- **描述**：Docker overlay2 的内部结构可能在版本升级时变化，导致存储适配层失效
- **影响**：核心功能完全失效
- **缓解措施**：
  - 兼容性测试覆盖 Docker 19.03、20.10、24.0
  - 设计版本检测机制，不支持的版本提前告警
  - 预留 Docker API 作为备选方案
- **优先级**：立即处理（MVP 开发前验证）

**2. ChainID 计算准确性** ⚠️ 高风险
- **描述**：Layer digest 到文件路径的映射依赖 chainID 计算，算法必须与 Docker 完全一致
- **影响**：拉取到错误的 layer 数据或找不到文件
- **缓解措施**：
  - 参考 Docker moby 源码实现
  - 对比 Spegel 的实现进行交叉验证
  - 详尽的单元测试和端到端验证
- **优先级**：立即处理（技术 POC 阶段）

**3. Kubernetes 环境集成复杂度** ⚠️ 高风险
- **描述**：HTTP_PROXY 模式需要修改所有 Pod 的环境变量，用户体验差
- **影响**：采用率低，用户流失
- **缓解措施**：
  - 提供 Mutating Webhook 自动注入环境变量
  - Phase 2 优先实现 DNS 劫持模式（零配置）
  - 详细的 K8s 集成文档
- **优先级**：Phase 2 必须解决

**4. 并发拉取性能瓶颈** 中风险
- **描述**：多节点同时从同一节点拉取，可能导致带宽或磁盘 I/O 瓶颈
- **影响**：性能下降，P2P 加速效果减弱
- **缓解措施**：
  - 简单负载均衡（随机选择节点）
  - 限制单节点并发连接数
  - 监控指标暴露节点负载
- **优先级**：Phase 2 优化

**5. 局域网信任模型的安全隐患** 中风险
- **描述**：HTTP 明文 + 无认证，容易被局域网内恶意节点攻击
- **影响**：安全敏感场景无法使用
- **缓解措施**：
  - 文档明确说明适用场景和安全假设
  - 提供网络隔离建议（VLAN、防火墙）
  - Phase 2+ 提供 mTLS 可选项
- **优先级**：Phase 2+ 根据用户反馈决定

---

### 开放问题

**技术实现问题：**

1. **Docker overlay2 的 chainID 计算是否有官方文档？**
   - 当前状态：只有源码，无官方文档
   - 需要行动：深入研究 moby 源码，编写技术笔记
   - 负责人：存储适配层开发者

2. **HTTP Proxy 如何处理自签名证书的 registry？**
   - 当前状态：未确定方案
   - 候选方案：
     - 支持 insecure-registry 配置
     - 支持自定义 CA 证书路径
     - 完全跳过证书验证（仅测试环境）
   - 需要决策：MVP 是否支持，还是要求用户使用公共 registry

3. **元数据同步的一致性如何保证？**
   - 当前状态：MVP 采用最终一致性（定期轮询）
   - 开放问题：是否需要强一致性？
   - 影响：节点新拉取镜像后，其他节点可能有 5-10 秒的延迟才能发现

4. **如何处理镜像删除场景？**
   - 当前状态：未设计
   - 问题：用户通过 `docker rmi` 删除镜像后，元数据索引如何更新？
   - 候选方案：
     - 文件监听（inotify）检测删除事件
     - 定期全量扫描（简单但低效）
     - 不主动更新，请求时返回 404（lazy 模式）

5. **是否支持私有 registry 的认证传递？**
   - 当前状态：MVP 不支持
   - 问题：用户拉取私有镜像时，P2P 节点如何获取认证信息？
   - 影响：私有镜像场景无法使用 P2P 加速
   - 可能方案：Phase 2 支持 Docker config.json 的认证信息读取

**产品定位问题：**

6. **是否应该支持 Docker Swarm？**
   - 当前状态：未考虑
   - 问题：Swarm 模式下的集成方式是否不同？
   - 决策依据：Swarm 的市场份额和社区活跃度

7. **是否应该支持 Podman？**
   - 当前状态：未考虑
   - 问题：Podman 的存储结构与 Docker 是否兼容？
   - 优先级：取决于边缘场景的 Podman 采用率

8. **监控指标的粒度应该多细？**
   - 当前状态：MVP 提供基础指标（总拉取次数、字节数）
   - 问题：是否需要 layer 级别、镜像级别、registry 级别的细粒度指标？
   - 权衡：指标越细，内存占用越高

**生态协作问题：**

9. **是否应该主动联系 K3s/KubeEdge 社区？**
   - 当前状态：未行动
   - 问题：何时联系？MVP 完成后还是 Phase 2 完成后？
   - 影响：社区认可度和采用率

10. **开源协议应该选择 Apache 2.0 还是 MIT？**
    - 当前状态：未决策
    - 考虑因素：
      - Apache 2.0：专利保护更强，企业友好
      - MIT：更简洁，采用门槛更低
    - 需要决策：项目启动前确定

11. **是否需要 CNCF Sandbox 项目申请？**
    - 当前状态：未考虑
    - 问题：加入 CNCF 的利弊？
    - 利：提升知名度、社区资源
    - 弊：治理开销、需要满足 CNCF 要求

---

### 需要进一步研究的领域

**1. 存储适配层技术调研**（优先级：最高）
- 研究内容：
  - Docker overlay2 的 imagedb/layerdb 详细结构
  - ChainID 计算算法的源码分析
  - Containerd content store 结构（Phase 2 准备）
- 预计时间：3-5 天
- 输出：技术调研报告 + POC 代码

**2. 竞品深度分析**（优先级：高）
- 研究对象：Spegel、Dragonfly、Kraken
- 重点关注：
  - Spegel 的 containerd 集成方式
  - Dragonfly 的性能数据
  - 各自的优缺点和适用场景
- 预计时间：2-3 天
- 输出：竞品对比文档

**3. 边缘场景实地调研**（优先级：中）
- 调研对象：实际的边缘计算用户
- 问题清单：
  - 实际的节点规模和网络环境
  - 当前的镜像管理痛点
  - 对安全性的要求
- 预计时间：1-2 周
- 输出：用户访谈报告

**4. DNS 劫持技术预研**（优先级：中）
- 研究内容：
  - CoreDNS 插件开发
  - dnsmasq 集成方式
  - 动态 DNS 记录更新机制
- 预计时间：2-3 天
- 输出：技术方案文档

## Appendices

### A. 研究总结

本项目简报基于以下研究输入：

**头脑风暴会议成果**（2025-10-02）
- 文档：`docs/brainstorming-session-results.md`
- 使用方法：第一性原理分析、假设反转、SCAMPER、形态分析
- 核心产出：
  - 30+ 技术决策点
  - 存储复用架构验证
  - 渐进式演进路线（静态配置 → 超级节点 → 零配置）
  - MVP 功能范围明确（6 个核心功能 + 8 个排除功能）

**关键洞察：**
- 复用 OCI 本地存储是核心差异化优势
- DNS 劫持 + mDNS 组合实现零配置体验
- 边缘场景的局域网信任模型允许简化安全设计
- 抽象层设计为未来多运行时支持奠定基础

---

### B. 参考竞品

**1. Spegel**
- GitHub: https://github.com/spegel-org/spegel
- 特点：深度集成 containerd，适合 K3s 环境
- 优势：零配置、性能优秀
- 劣势：仅支持 containerd，不支持 Docker
- 启示：containerd 集成方式可参考，但需要抽象层支持多运行时

**2. Dragonfly**
- GitHub: https://github.com/dragonflyoss/Dragonfly2
- 特点：企业级 P2P 文件分发系统，支持容器镜像
- 优势：功能完善、大规模验证
- 劣势：独立缓存层导致存储翻倍、配置复杂
- 启示：监控指标和性能优化方面可参考

**3. Kraken (Uber)**
- GitHub: https://github.com/uber/kraken
- 特点：Uber 内部的 P2P Docker 镜像分发系统
- 优势：大规模验证（数万节点）
- 劣势：架构复杂，不适合边缘小规模场景
- 启示：P2P 网络拓扑设计可参考

---

### C. 相关标准与规范

**OCI Distribution Specification v2**
- 文档：https://github.com/opencontainers/distribution-spec/blob/main/spec.md
- 重要性：OutpostImage 必须完全遵循此规范
- 核心 API：
  - `GET /v2/` - API 版本检查
  - `GET /v2/<name>/manifests/<reference>` - 获取镜像 manifest
  - `GET /v2/<name>/blobs/<digest>` - 获取 layer blob
  - `HEAD /v2/<name>/blobs/<digest>` - 检查 blob 是否存在

**Docker Image Specification**
- 文档：https://github.com/moby/moby/blob/master/image/spec/v1.2.md
- 重要性：理解 Docker overlay2 存储结构的基础
- 关键概念：
  - imagedb：镜像元信息存储
  - layerdb：layer 元信息和 chainID 映射
  - overlay2 存储驱动：实际 layer 数据存储

## Next Steps

### 立即行动（项目启动前，1-2 周）

**1. 技术 POC 验证**
- **任务**：ChainID 计算算法验证
- **负责人**：技术负责人
- **交付物**：
  - 可运行的 POC 代码（Go）
  - 验证报告（覆盖 nginx、alpine、ubuntu 等常见镜像）
  - 技术笔记（chainID 计算逻辑详解）
- **时间**：3 天

**2. 存储适配层技术调研**
- **任务**：深入研究 Docker overlay2 结构
- **负责人**：存储适配层开发者
- **交付物**：
  - 技术调研报告
  - Docker 19.03/20.10/24.0 的兼容性测试结果
  - 存储适配层接口设计文档
- **时间**：5 天

**3. 项目仓库初始化**
- **任务**：创建 GitHub 仓库，设置基础结构
- **负责人**：项目负责人
- **交付物**：
  - GitHub 仓库（outpostimage）
  - 基础目录结构（参考技术考量部分的仓库结构）
  - README.md、LICENSE、CONTRIBUTING.md
  - CI/CD 配置（GitHub Actions）
- **时间**：2 天

**4. 开源协议确定**
- **任务**：决策 Apache 2.0 vs MIT
- **负责人**：项目负责人
- **决策标准**：
  - 企业采用友好度
  - 社区贡献门槛
  - 专利保护需求
- **时间**：1 天

---

### Phase 1: MVP 开发（3 个月）

**里程碑 1：存储适配层完成（4 周）**
- 实现 Docker overlay2 存储适配
- 单元测试覆盖率 > 80%
- 验收标准：能够读取本地镜像的 manifest 和 blob

**里程碑 2：OCI Registry API 实现（4 周）**
- 实现符合 OCI Distribution Spec v2 的 API Server
- 支持核心 endpoints（/v2/, manifests, blobs）
- 验收标准：`docker pull` 能从本地 Registry 拉取镜像

**里程碑 3：HTTP Proxy 实现（3 周）**
- 实现 HTTP Proxy 请求拦截
- 实现路由逻辑（本地 vs 外网）
- 验收标准：设置 HTTP_PROXY 后能自动从 P2P 网络拉取

**里程碑 4：节点发现与元数据同步（3 周）**
- 实现静态配置文件
- 实现元数据内存索引
- 实现基础的节点健康检查
- 验收标准：多节点环境下能正确发现和路由

**里程碑 5：监控与集成测试（2 周）**
- 实现 Prometheus 监控指标
- 编写端到端集成测试
- 编写部署文档（Docker Compose + Kubernetes）
- 验收标准：通过 MVP 成功标准（本文档中定义）

---

### Phase 2: 功能增强（3 个月）

**优先级排序：**
1. **Containerd 支持**（6 周）- 扩大用户群体
2. **DNS 劫持模式**（4 周）- 零配置体验
3. **mDNS 节点发现**（2 周）- 消除静态配置
4. **性能优化**（持续）- 智能路由、并行传输

---

### PM 交接

本项目简报提供了 OutpostImage 的完整背景和技术方案。下一步建议：

1. **PRD 生成**：基于本简报创建详细的产品需求文档（PRD）
   - 详细的用户故事和验收标准
   - API 设计文档
   - 部署和运维手册

2. **技术架构文档**：深化技术设计
   - 详细的模块设计
   - 数据流图和时序图
   - 错误处理和边界情况

3. **项目计划**：制定详细的开发计划
   - Sprint 规划（2 周一个 Sprint）
   - 资源分配
   - 风险管理计划

**关键成功因素：**
- ✅ 专注核心价值（存储复用 + P2P 加速）
- ✅ 快速迭代，尽早发布 MVP
- ✅ 积极参与边缘 K8s 社区
- ✅ 保持技术简洁，避免过度设计

---

*本项目简报由 Business Analyst Mary 创建，基于 2025-10-02 的头脑风暴会议成果。*
