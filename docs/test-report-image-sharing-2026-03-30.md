# OutpostImage 节点间镜像共享 — 测试报告

**日期**: 2026-03-30
**版本**: v0.1.0 (commit `ba2469e`)
**测试人**: CEO Agent
**项目**: OutpostImage

---

## 1. 测试目标

验证 OutpostImage 能够实现节点间的容器镜像共享：VM 103 (openyurt-edge-node) 上的 Docker 可以从 T480 (192.168.1.200) 获取镜像。

## 2. 测试环境

| 节点 | 角色 | IP | 操作系统 | Docker 版本 | 说明 |
|------|------|-----|---------|------------|------|
| T480 | 镜像源 (Registry) | 192.168.1.200 | Proxmox VE (Linux 6.2.16) | 27.5.1 | Proxmox 宿主机，已有 11 个 Docker 镜像 |
| VM 103 | 镜像消费者 (Client) | 192.168.1.103 | Ubuntu 20.04.6 LTS | 26.1.3 | Proxmox KVM 虚拟机，2C/2G/30G，新安装 Docker |

### 网络拓扑

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

## 3. OutpostImage 配置

### T480 (镜像源)

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

启动日志：
```
docker store initialized: images=11, tags=11, blobs=22, layers=100, layer_blobs=100
```

### VM 103 (镜像消费者)

Docker daemon 配置 (`/etc/docker/daemon.json`):
```json
{
  "insecure-registries": ["192.168.1.200:15000"]
}
```

## 4. 测试用例及结果

### TC-01: OCI Registry API 连通性

| 步骤 | 操作 | 预期结果 | 实际结果 | 状态 |
|------|------|---------|---------|------|
| 1 | `curl http://192.168.1.200:15000/v2/` | 返回 `{}` | `{}` | PASS |
| 2 | `curl http://192.168.1.200:15000/api/v1/digests` | 返回 digest 列表 | 11 digests | PASS |
| 3 | 从 VM 103 访问 T480 registry | 同上 | 同上 | PASS |

### TC-02: 单层镜像拉取 (debian:bookworm-slim)

| 步骤 | 操作 | 预期结果 | 实际结果 | 状态 |
|------|------|---------|---------|------|
| 1 | 获取 manifest | schemaVersion=2, 1 layer | 返回正确的 distribution manifest | PASS |
| 2 | `docker pull 192.168.1.200:15000/debian:bookworm-slim` | Pull 成功 | 下载并校验通过 | PASS |
| 3 | `docker run --rm ... cat /etc/debian_version` | 输出 Debian 版本 | `12.13` | PASS |

Pull 输出：
```
bookworm-slim: Pulling from debian
5712ebd750aa: Pull complete
Digest: sha256:42a264b6a07c8501a0677b301d5e4510f9bdd2949c3c7e7ff412a272b6e088c0
Status: Downloaded newer image for 192.168.1.200:15000/debian:bookworm-slim
```

Manifest 详情：
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

### TC-03: 多层镜像拉取 (redis:7-alpine, 8 layers)

| 步骤 | 操作 | 预期结果 | 实际结果 | 状态 |
|------|------|---------|---------|------|
| 1 | 获取 manifest | schemaVersion=2, 8 layers | 返回正确的 8 层 distribution manifest | PASS |
| 2 | `docker pull 192.168.1.200:15000/redis:7-alpine` | Pull 成功 | 8 层全部下载并校验通过 | PASS |
| 3 | `docker run --rm ... redis-server --version` | 输出 Redis 版本 | `Redis server v=7.4.7` | PASS |

Pull 输出：
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

### TC-04: 镜像完整性验证

| 检查项 | debian:bookworm-slim | redis:7-alpine | 状态 |
|--------|---------------------|----------------|------|
| Image ID 一致性 | d1c8cb627495 | d050212bffac | PASS |
| 镜像大小合理 | 78.6MB | 41.7MB | PASS |
| 容器可正常启动 | `cat /etc/debian_version` → `12.13` | `redis-server --version` → `v7.4.7` | PASS |
| Digest 校验通过 | Docker 自动校验 Checksum | Docker 自动校验 Checksum | PASS |

## 5. VM 103 最终状态

```
REPOSITORY                   TAG             IMAGE ID       CREATED        SIZE
192.168.1.200:15000/debian   bookworm-slim   d1c8cb627495   4 weeks ago    78.6MB
192.168.1.200:15000/redis    7-alpine        d050212bffac   4 months ago   41.7MB
```

## 6. 代码变更

本次验证过程中发现并修复了一个关键缺陷：

**问题**: `pkg/oci/docker.go` 中 `GetBlob` 仅返回 image config blob，无法提供 layer 数据；`GetManifest` 返回 image config 而非 OCI distribution manifest。

**修复** (commit `ba2469e`):
- 从 overlay2 diff 目录生成 tar blob 并计算 digest
- 构建符合 OCI Distribution Spec 的 manifest (schemaVersion 2)
- 重写 image config 的 `diff_ids` 字段匹配生成的 tar digest
- 新增 `rewrittenConfigs` 内存存储服务重写后的 config blob

**影响**: +341 行 / -29 行，仅修改 `pkg/oci/docker.go`

## 7. 已知限制

| 限制 | 影响 | 优先级 |
|------|------|--------|
| Init 阶段 tar 计算耗时 ~80s (100 layers) | 启动慢，建议改为懒加载 | Medium |
| 仅支持 HTTP insecure registry 模式 | 需配置 Docker `insecure-registries` | Medium |
| HTTPS MITM 未连接 | 无法透明拦截 Docker Hub HTTPS 请求 | Low (Phase 3) |
| Layer tar 与原始 tar 元数据不同 | 需重写 config diff_ids，镜像 digest 与上游不一致 | Low |

## 8. 结论

**测试通过**。OutpostImage 成功实现了节点间的容器镜像共享：

- VM 103 的 Docker 能够从 T480 的 OutpostImage registry 拉取完整的容器镜像
- 单层镜像 (debian) 和多层镜像 (redis, 8 layers) 均通过验证
- 拉取的镜像可以正常启动容器并执行命令
- Docker 的 Checksum 校验全部通过，镜像完整性有保障

---

*报告生成: 2026-03-30 | OutpostImage v0.1.0 | Commit ba2469e*
