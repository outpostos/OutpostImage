**Session Date:** 2025-10-02
**Facilitator:** Business Analyst Mary
**Participant:** 项目团队

# Executive Summary

**Topic:** 边缘计算场景下的容器镜像 P2P 共享网络方案

**Session Goals:**
设计一个边缘计算场景下的容器镜像本地共享网络方案，解决边缘节点内网带宽远大于出口带宽的痛点，避免传统 P2P 方案（如 Dragonfly）的存储翻倍和缺乏指向性问题。

**Techniques Used:**
- 第一性原理分析 (30 分钟)
- 假设反转 (20 分钟)
- SCAMPER 方法 (40 分钟)
- 形态分析 (20 分钟)

**Total Ideas Generated:** 30+ 核心技术决策点

**Key Themes Identified:**
- 基于 OCI 规范的本地存储复用（避免缓存翻倍）
- 插件化架构设计（containerd 风格，适配边缘场景）
- DNS 劫持 + HTTP Proxy 的双模式请求拦截
- 渐进式演进路线（静态配置 → 超级节点 → 零配置）
- 统一抽象层 + 具体实现分离

---

# Technique Sessions

## 第一性原理分析 - 30 分钟

**Description:** 通过拆解问题到最基本的真理，验证方案的底层逻辑可行性

**Ideas Generated:**

1. **容器镜像拉取的本质三阶段：**
   - 认证阶段
   - 获取元信息（manifest）阶段
   - 拉取二进制 layer 阶段
   - **关键洞察：三阶段全部可本地化，缓存后完全离线可行**

2. **最小可行组件（MVP）：**
   - 每个节点 = OCI Registry（暴露本地镜像）
   - 简单节点发现（静态配置/Leader-based）
   - 直接元信息共享（无需复杂 DHT）
   - 无数据库设计（从本地 OCI 存储实时重建，避免状态不一致）

3. **核心价值主张：**
   - 复用容器运行时本地存储（Docker/containerd）
   - 遵循 OCI Distribution Spec v2
   - 无需额外存储开销（vs 传统 P2P 翻倍存储）

**Insights Discovered:**
- 本地 OCI 存储已经是标准化的，只需要适配层转换为 Registry API
- "无数据库"设计避免了数据一致性陷阱
- 完全离线能力是边缘场景的核心需求

**Notable Connections:**
- 与 Spegel 的对比：Spegel 深度依赖 containerd 特定 API，我们的方案通过抽象层支持多运行时
- 边缘场景特点（小范围、高内网带宽）与方案设计的完美契合

---

## 假设反转 - 20 分钟

**Description:** 挑战核心假设，发现潜在技术陷阱

**Ideas Generated:**

1. **反转假设：本地 OCI 存储可直接暴露**
   - 挑战成功：需要存储适配层
   - Docker overlay2 vs containerd content store 结构不同
   - MVP 选择一个主流存储引擎（如 overlay2）
   - 决策：统一抽象层架构

2. **反转假设：HTTP_PROXY 可拦截所有请求**
   - 验证结果：已测试可行
   - 证书方案：insecure-registry 或自签名证书
   - **关键优势发现：HTTP Proxy 可代理多个 registry（vs Mirror 单一限制）**

3. **反转假设：元信息无需数据库**
   - 优化方案：启动时扫描 + 文件监听/事件触发增量更新
   - 必须有内存索引/轻量缓存
   - Gossip 协议用于集群间元信息同步

**Insights Discovered:**
- 存储适配是不可避免的技术挑战
- HTTP Proxy 的多 registry 支持是意外的架构优势
- 内存索引是必需品，但可以是轻量级的

**Notable Connections:**
- 证书处理方案与边缘场景的"局域网信任"模型契合
- 文件监听机制可以复用容器运行时的事件系统

---

## SCAMPER 方法 - 40 分钟

**Description:** 系统化优化架构的每个维度

### S - Substitute (替换)

**Ideas Generated:**
1. **Gossip → mDNS + gRPC** ✨
   - 更轻量、无依赖
   - 适合小规模边缘集群

2. **内存索引方案：**
   - BoltDB（嵌入式 KV）
   - SQLite in-memory
   - 需注意数据一致性

3. **拦截方式探索：**
   - iptables DNAT（需 root，复杂）
   - FUSE filesystem（过于复杂，不推荐）
   - **决策：HTTP Proxy 已足够好**

### C - Combine (组合)

**架构发现：整体边缘计算平台的一部分**

**Ideas Generated:**
1. **插件化架构设计（类 containerd）：**
   ```
   EdgeOS Core (单进程)
   ├─ Core Layer (公共基础)
   │  ├─ Discovery (mDNS)
   │  ├─ Communication (gRPC)
   │  ├─ Auth Service
   │  ├─ Metadata Store (统一元数据)
   │  └─ Plugin Manager
   ├─ Image Plugin (镜像服务)
   ├─ Mesh Plugin (网络服务)
   └─ Scheduler Plugin (调度服务)
   ```

2. **服务间数据共享：**
   - 统一元数据存储
   - 每个插件维护自己的领域数据
   - 镜像服务：节点镜像信息
   - Mesh 服务：节点服务信息
   - 调度服务：Pod 信息

3. **进程内通信，启动注册**
   - 简化边缘部署复杂度
   - 避免微服务的运维开销

### A - Adapt (调整)

**Ideas Generated:**
1. **元数据结构：** 按现有设定（digest → 节点映射）
2. **插件间协作：** 架构预留接口，当前不实现
3. **降级策略：** 自动回退到外网拉取

### M - Modify (优化)

**Ideas Generated:**
1. **Layer 传输优化：**
   - ✅ 必须支持断点续传（HTTP Range）
   - ⏸️ 分块传输、压缩算法优化（非优先）

2. **缓存策略：**
   - ❌ 不需要独立缓存
   - **核心优势：复用 OCI 本地存储，无存储翻倍**

3. **请求路由策略：**
   - ✅ 简单随机选择节点
   - 未来可优化：网络延迟、负载感知

### P - Put to other uses (扩展)

**Ideas Generated:**
- 专注边缘容器镜像场景
- 其他场景（模型分发、固件更新）后续考虑

### E - Eliminate (简化 MVP)

**MVP 功能范围决策：**

**砍掉的功能：**
- ❌ 认证/鉴权（局域网信任模型）
- ❌ 加密传输（HTTP 明文）

**保留的功能：**
- ✅ 同时支持 Docker + containerd
- ✅ 配置文件 + 热更新
- ✅ 自动重试 + 指数退避
- ✅ 基础监控指标（传输速度、成功率）

### R - Reverse (反向思考)

**Ideas Generated:**
1. **Push vs Pull：** 不考虑 Push 模式
2. **超级节点模式：** 可选方案，核心仍是本地存储转 Registry
3. **预加载：** 业务层决策，不在本层实现

**Insights Discovered:**
- MVP 范围清晰，避免过度设计
- 监控指标对边缘场景的调试至关重要
- 超级节点作为可选架构模式保留

---

## 形态分析 - 20 分钟

**Description:** 探索不同技术实现组合的最优方案

**Ideas Generated:**

### 维度 1：节点发现机制
**演进路线（分阶段）：**
1. **Phase 1 (MVP)：** 静态配置文件
2. **Phase 2：** 超级节点模式（可选）
3. **Phase 3：** mDNS 零配置
4. **不考虑：** etcd/Consul（额外依赖）

### 维度 2：元信息同步
**双层抽象架构：**
```
应用层：MessageBus (消息总线)
   ↓
传输层：NetworkLayer (自定义协议 - 广播机制)
   ↓
物理层：TCP/UDP/gRPC
```
- 上层应用消息总线
- 下层自定义传输协议
- 两者互不矛盾，分层解耦

### 维度 3：存储适配层
**决策：统一抽象层**
- 确定：需要抽象层适配不同运行时
- 📋 待探索：具体技术方案
  - Docker API
  - Image DB 读取
  - Containerd API
- 作为独立技术调研任务

### 维度 4：代理实现
**混合模式 - DNS 优先，Proxy 降级：**

**场景 1：DNS 劫持 + OCI Registry（优先）**
```
docker pull docker.io/nginx:latest
    ↓
DNS 解析 docker.io
    ↓ (被劫持，类似 iptables DNAT)
返回局域网节点 IP (192.168.1.10)
    ↓
Docker 访问 http://192.168.1.10/v2/...
    ↓
本地 OCI Registry 响应
```
**优势：**
- ✅ 完全透明，零配置
- ✅ 支持所有 OCI 客户端
- ✅ 不依赖 HTTP_PROXY

**实现：**
- 本地 DNS 服务（dnsmasq/CoreDNS）
- 或修改 `/etc/hosts`

**场景 2：HTTP Proxy 模式（降级）**
```
docker pull docker.io/nginx:latest
    ↓
HTTP_PROXY=http://localhost:8080
    ↓
本地代理拦截 → P2P 网络
```

**降级逻辑：**
- DNS 劫持失败/不支持 → 自动切换 HTTP Proxy

**Insights Discovered:**
- DNS 劫持是最优雅的透明拦截方案
- 双模式提供了灵活性和容错能力
- 渐进式演进路线降低实施风险

**Notable Connections:**
- DNS 劫持借鉴了 iptables DNAT 的思想
- 与 Kubernetes CoreDNS 插件机制可复用

---

# Idea Categorization

## Immediate Opportunities
*Ideas ready to implement now*

1. **MVP - 静态配置 + HTTP Proxy 模式**
   - Description: 最简单的可验证方案，静态配置节点列表，HTTP Proxy 拦截请求
   - Why immediate: 技术风险低，可快速验证核心价值
   - Resources needed:
     - Go 开发环境
     - Docker/containerd 测试环境
     - 基础 HTTP Server 框架

2. **存储适配层技术调研**
   - Description: 调研 Docker overlay2 和 containerd 的存储结构，确定 digest 映射方案
   - Why immediate: 是后续开发的前置依赖
   - Resources needed:
     - Docker/containerd 源码分析
     - 本地环境实验
     - 2-3 天研究时间

3. **OCI Distribution API Server 实现**
   - Description: 实现符合 OCI Distribution Spec v2 的最小 API Server
   - Why immediate: 核心组件，可独立开发测试
   - Resources needed:
     - OCI 规范文档
     - 参考 Spegel 实现
     - HTTP 框架（如 Gin/Echo）

## Future Innovations
*Ideas requiring development/research*

1. **DNS 劫持模式**
   - Description: 实现透明的 DNS 劫持 + OCI Registry 模式
   - Development needed:
     - DNS 服务集成（dnsmasq/CoreDNS）
     - 动态 DNS 记录更新机制
     - 负载均衡策略
   - Timeline estimate: 2-3 周（在 MVP 之后）

2. **mDNS 零配置发现**
   - Description: 实现 mDNS + gRPC 的自动节点发现
   - Development needed:
     - mDNS 协议实现（或使用 hashicorp/mdns）
     - gRPC 服务注册/发现
     - 健康检查机制
   - Timeline estimate: 1-2 周

3. **双层网络抽象（MessageBus + NetworkLayer）**
   - Description: 实现应用层消息总线 + 传输层协议抽象
   - Development needed:
     - 接口设计和抽象
     - Gossip/gRPC Stream 实现
     - 元信息同步机制
   - Timeline estimate: 2-3 周

4. **多运行时支持（Docker + containerd）**
   - Description: 统一抽象层同时支持 Docker 和 containerd
   - Development needed:
     - 存储适配器模式设计
     - Docker API 集成
     - Containerd API 集成
   - Timeline estimate: 3-4 周

## Moonshots
*Ambitious, transformative concepts*

1. **边缘计算统一平台（EdgeOS Core）**
   - Description: 插件化边缘计算平台，镜像服务只是第一个插件
   - Transformative potential:
     - 统一边缘节点的镜像、网络、调度服务
     - 可扩展到模型分发、固件更新等场景
     - 成为边缘计算的"容器运行时"级别基础设施
   - Challenges to overcome:
     - 插件机制设计复杂度
     - 多服务协同的状态管理
     - 边缘场景的资源限制

2. **智能调度集成**
   - Description: 镜像插件与调度插件协作，基于镜像位置优化 Pod 调度
   - Transformative potential:
     - 减少镜像拉取延迟
     - 优化边缘资源利用
     - 实现"数据本地性"调度
   - Challenges to overcome:
     - 插件间接口设计
     - 调度算法复杂度
     - Kubernetes 集成

3. **混合云镜像加速**
   - Description: 边缘 + 云端混合，云端作为"热备节点"
   - Transformative potential:
     - 边缘离线时云端兜底
     - 新镜像从云端预热到边缘
     - 边缘-云端智能路由
   - Challenges to overcome:
     - 广域网 P2P 性能
     - 网络分区处理
     - 安全性和认证

## Insights & Learnings
*Key realizations from the session*

- **复用 > 复制**: 复用 OCI 本地存储比创建 P2P 缓存更优雅，避免存储翻倍是核心优势
- **抽象的力量**: 统一抽象层（存储适配、网络协议）使方案具有长期扩展性
- **渐进式演进**: 静态配置 → 超级节点 → 零配置的演进路线降低风险，符合敏捷开发原则
- **透明性价值**: DNS 劫持模式的零配置特性是用户体验的巨大提升
- **边缘场景特殊性**: 局域网信任模型允许简化认证/加密，这是云端方案无法做到的
- **插件化思维**: 将镜像服务作为更大平台的一部分，为未来扩展预留空间
- **Spegel 的启示**: 深度依赖特定运行时是限制，抽象层是破局关键

---

# Action Planning

## Top 3 Priority Ideas

### #1 Priority: 存储适配层技术调研
- **Rationale**:
  - 是整个方案的技术前置依赖
  - 决定了后续实现的复杂度
  - 需要验证 Docker overlay2 和 containerd 的 digest 映射可行性

- **Next steps**:
  1. 分析 Docker `/var/lib/docker/image/overlay2/imagedb/` 结构
  2. 研究 containerd `/var/lib/containerd/io.containerd.content.v1.content/` 结构
  3. 测试三种方案：Image DB 读取、Docker API、Containerd API
  4. 确定统一抽象接口设计
  5. 编写技术调研报告

- **Resources needed**:
  - Docker/containerd 源码
  - 本地测试环境（多种存储驱动）
  - 1-2 名工程师

- **Timeline**: 3-5 天

### #2 Priority: MVP - HTTP Proxy + 静态配置模式
- **Rationale**:
  - 技术风险最低，可快速验证核心价值
  - 不依赖 DNS 劫持等复杂机制
  - 能够演示端到端的 P2P 镜像拉取

- **Next steps**:
  1. 实现 OCI Distribution API Server（/v2/ endpoints）
  2. 实现本地镜像扫描和索引（基于存储适配层调研结果）
  3. 实现 HTTP Proxy 请求拦截和路由
  4. 实现静态配置的节点发现
  5. 实现基础监控指标（Prometheus metrics）
  6. 编写端到端测试

- **Resources needed**:
  - Go HTTP 框架（Gin/Echo）
  - OCI Distribution Spec v2 文档
  - 2-3 名工程师

- **Timeline**: 2-3 周

### #3 Priority: 网络抽象层设计
- **Rationale**:
  - 支持未来从静态配置演进到动态发现
  - 为插件化架构的通信层奠定基础
  - 双层抽象（MessageBus + NetworkLayer）是长期架构的核心

- **Next steps**:
  1. 设计 NetworkLayer 接口（广播、订阅）
  2. 设计 MessageBus 接口（发布、监听）
  3. 实现第一版：基于 gRPC Stream 的广播
  4. 实现元信息同步机制
  5. 编写接口文档和使用示例

- **Resources needed**:
  - gRPC 框架
  - 分布式系统经验
  - 1-2 名工程师

- **Timeline**: 1-2 周（可与 MVP 并行）

---

# Reflection & Follow-up

## What Worked Well
- 第一性原理分析帮助验证了方案的底层可行性
- 假设反转发现了存储适配层的关键挑战
- SCAMPER 系统化梳理了架构的各个维度
- 形态分析明确了渐进式演进路线
- 讨论中及时发现了更大的边缘计算平台背景，调整了架构视角

## Areas for Further Exploration
- **存储适配层具体实现**: 需要深入研究 Docker/containerd 的存储结构，确定最优的 digest 映射方案
- **DNS 劫持实现细节**: 如何集成 DNS 服务（dnsmasq/CoreDNS），如何动态更新 DNS 记录
- **网络分区处理**: 当局域网分区时，如何保证元信息一致性
- **性能基准测试**: P2P vs 直接外网拉取的性能对比，量化收益
- **插件间协作机制**: 未来镜像插件如何与调度插件、网络插件协作

## Recommended Follow-up Techniques
- **五个为什么 (5 Whys)**: 深入探究存储适配层的 digest 映射问题
- **原型快速验证**: 构建 throwaway prototype 快速验证 DNS 劫持可行性
- **竞品深度分析**: 深入研究 Spegel 源码，学习其实现细节
- **场景模拟 (Scenario Planning)**: 模拟网络分区、节点故障等异常场景

## Questions That Emerged
- Docker overlay2 的 layer-id 到 digest 的映射是否有现成的 API？
- mDNS 在大规模边缘集群（100+ 节点）的性能表现如何？
- 如何处理镜像更新时的缓存失效？
- 是否需要支持私有 registry 的认证传递？
- 监控指标应该暴露哪些维度（传输速度、P2P 命中率、节点健康度）？
- 如何与现有的 Kubernetes 集群集成？

## Next Session Planning
- **Suggested topics**:
  1. 存储适配层技术调研汇报与方案评审
  2. MVP 详细设计评审（API 设计、数据流、错误处理）
  3. 网络抽象层接口设计评审

- **Recommended timeframe**:
  - 存储调研完成后（约 1 周后）
  - MVP 开发中期（约 2 周后）

- **Preparation needed**:
  - 完成存储适配层技术调研
  - 准备 OCI Distribution API 设计文档
  - 准备网络抽象层接口设计文档

---

*Session facilitated using the BMAD-METHOD™ brainstorming framework*
