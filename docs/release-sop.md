# OutpostImage Release SOP

Standard Operating Procedure for OutpostImage product releases. Covers code freeze through production deployment verification.

---

## Overview

OutpostImage 发布流程分为 6 个阶段：代码冻结、Landing Page 准备、部署配置、内容审核、部署验证、正式发布。每个阶段有明确的准入/准出条件。

---

## Phase 1: Code Freeze & Quality Gate

**准入条件**: 所有计划功能已合并到 `main`

| 步骤 | 操作 | 验收标准 |
|------|------|----------|
| 1.1 | 确认所有功能 PR 已合并 | `main` 分支包含所有 release scope 内的 commit |
| 1.2 | CI 流水线全绿 | GitHub Actions `CI` workflow 通过（test + lint） |
| 1.3 | 测试覆盖率 >= 60% | `go tool cover -func=coverage.out` 总覆盖率达标 |
| 1.4 | 运行完整测试矩阵 | Docker + containerd 组合测试通过 |
| 1.5 | 编写/更新测试报告 | `docs/test-report-*.md` 包含所有 VP 验证结果 |

**关键命令**:

```bash
# 本地验证
make ci            # lint + test-coverage
make build         # 确认编译通过

# 完整测试（需要多节点环境）
# 参考 docs/ 下的 test-report 文档获取测试场景
```

**准出条件**: CI 全绿，测试报告完整，无 critical/high bug

---

## Phase 2: Landing Page 准备

**准入条件**: Phase 1 通过

| 步骤 | 操作 | 验收标准 |
|------|------|----------|
| 2.1 | 创建/更新 `site/index.html` | 包含产品简介、架构示意图、核心功能、测试结果 |
| 2.2 | 创建/更新 `site/style.css` | 样式简洁，与 OutpostOS 系列产品风格一致 |
| 2.3 | 内容审核 — How It Works | 使用文字 + ASCII 示意图（参考 OutpostNet 风格），不使用复杂 CSS 图表 |
| 2.4 | 内容审核 — Test Results | 使用英文编写，包含各 VP 验证详情和性能数据 |
| 2.5 | 本地预览 | 在浏览器中打开 `site/index.html` 确认渲染正常 |

**风格规范**:

- 参考 OutpostNet landing page (https://net.outpostos.com/) 的简洁风格
- Inline CSS 优先，避免过度工程化
- ASCII diagram 优先于 CSS box diagram
- 测试结果使用英文，技术指标需包含具体数据

**准出条件**: Landing page 内容完整，风格与系列产品一致

---

## Phase 3: 部署配置

**准入条件**: Phase 2 通过

| 步骤 | 操作 | 验收标准 |
|------|------|----------|
| 3.1 | 配置 `wrangler.jsonc` | `assets.directory` 指向 `site` |
| 3.2 | Cloudflare Pages 项目设置 | 关联 GitHub repo，Build command 留空，Build output directory 设为 `site` |
| 3.3 | 自定义域名（如需要） | 在 Cloudflare Dashboard 绑定域名 |

**wrangler.jsonc 模板**:

```jsonc
{
  "$schema": "node_modules/wrangler/config-schema.json",
  "name": "outpostimage",
  "compatibility_date": "2025-09-27",
  "observability": { "enabled": true },
  "assets": { "directory": "site" },
  "compatibility_flags": ["nodejs_compat"]
}
```

**Cloudflare Pages 配置要点**:

- **Build command**: 留空（纯静态站点，无需构建步骤）
- **Build output directory**: `site`
- 不要使用 `npx wrangler deploy` 作为 build command（会导致 Workers 误检测错误）

**准出条件**: Cloudflare Pages 项目已创建并关联 repo

---

## Phase 4: 内容审核

**准入条件**: Phase 2 + Phase 3 完成

| 步骤 | 操作 | 审核人 |
|------|------|--------|
| 4.1 | 架构示意图准确性 | CEO / Tech Lead |
| 4.2 | 功能描述与实际能力一致 | Dev Lead |
| 4.3 | 测试数据真实性 | QA / Test Author |
| 4.4 | 与竞品对比表准确性 | CEO |
| 4.5 | 部署文档链接有效 | Dev |

**常见审核问题**:

- "How It Works" 部分是否使用了过度工程化的 CSS 图表 → 改为文字 + ASCII 示意图
- 测试报告是否只是占位文字 → 需包含实际 VP 验证数据
- PR 是否真正合并（不只是提交） → 在 GitHub 确认 PR 状态

**准出条件**: 所有审核项通过，修改已推送到 `main`

---

## Phase 5: 部署验证

**准入条件**: Phase 4 通过，代码已推送到 `main`

| 步骤 | 操作 | 验收标准 |
|------|------|----------|
| 5.1 | 触发 Cloudflare Pages 部署 | 推送到 `main` 自动触发，或在 Dashboard 手动触发 |
| 5.2 | 检查部署状态 | Cloudflare Pages Dashboard 显示 deployment success |
| 5.3 | 访问 Landing Page | `https://<project>.pages.dev/` 正常加载 |
| 5.4 | 验证所有页面段落 | Hero、How It Works、Features、Test Results、Footer 均正常显示 |
| 5.5 | 自定义域名验证（如有） | `https://<custom-domain>/` 正常加载，HTTPS 证书有效 |
| 5.6 | 匿名仓库可达性验证 | 剥离本机凭证后，仓库可匿名读取（见下节）。镜像由 Phase 6 构建，故此处只验证仓库 |

**匿名仓库可达性**（发布准出门禁）:

Landing Page 是公开页面，但其引用的仓库未必公开。必须在**剥离本机凭证**的状态下验证——本机已 `gh auth login` 或配置了 credential helper 时，普通 `git ls-remote` 会用存储凭证成功，从而掩盖私有仓库的问题：

```bash
# 仓库可匿名访问（期望 200；404 表示仓库仍为 private）
curl -s -o /dev/null -w '%{http_code}\n' https://github.com/outpostos/outpostimage

# 仓库可匿名读取：禁用交互提示并清空 credential helper（期望成功；要求凭证即失败）
GIT_TERMINAL_PROMPT=0 git -c credential.helper= ls-remote https://github.com/outpostos/outpostimage HEAD
```

> **若产品决定保持私有**（第 6 阶段选项 b）：不要跳过本门禁，而应从公开 Landing Page 移除对应的 `git clone` / 镜像拉取安装命令，改为「联系获取」等表述。门禁验证的是「页面宣传的东西外部用户确实拿得到」，私有本身不是失败，公开宣传私有产物才是。

**故障排查**:

- **部署失败 — "Could not detect static files"**: 检查 `wrangler.jsonc` 中 `assets.directory` 是否正确指向 `site`
- **部署成功但内容未更新**: 清除 Cloudflare CDN 缓存，或等待几分钟自动刷新
- **Build command 报错**: 确认 build command 为空（纯静态站），不要使用 `npx wrangler deploy`

**准出条件**: Landing page 在线且内容与 `main` 分支一致，仓库可匿名读取

---

## Phase 6: 正式发布

**准入条件**: Phase 5 通过

| 步骤 | 操作 | 验收标准 |
|------|------|----------|
| 6.1 | Docker Image 构建 | GitHub Actions `Release` workflow 自动构建并推送到 `ghcr.io` |
| 6.2 | 确认 GHCR 镜像可**匿名**拉取 | 使用空 Docker 配置拉取最新 tag 成功（见下） |
| 6.3 | 创建 GitHub Release（可选） | Tag 版本号，附 changelog |
| 6.4 | 更新 README 安装信息 | Quick Start 中的镜像 tag 与 release 一致 |
| 6.5 | 通知相关方 | 在 Paperclip 关闭发布 issue，评论发布结果 |

**匿名镜像拉取**（Phase 6.2，必须在 6.1 推送完成之后执行）:

5.6 只验证仓库可达；镜像在 6.1 才构建推送，因此镜像可达性必须在此处验证。`docker logout` 在不同 credential store 下未必彻底，使用一个空的 `DOCKER_CONFIG` 目录确保不带任何本机凭证：

```bash
DOCKER_CONFIG=$(mktemp -d) docker pull ghcr.io/outpostos/outpostimage:latest
```

**Docker Release 流水线** (自动，push to `main` 触发):

- 构建 Docker image
- 推送到 `ghcr.io/outpostos/outpostimage`
- Tag: `latest`, `main`, `sha-<commit>`

> **GHCR 包可见性不会随仓库自动变化。** 由 private 仓库推送的镜像包默认是 private；即使仓库改为 public，也必须在 Package settings → Change visibility 手动把包设为 public。否则外部用户执行 Quick Start 会得到 `unauthorized`。

**准出条件**: Docker image 可**匿名**拉取，Landing page 在线，release issue 关闭

---

## Checklist 速查表

```
Pre-release:
[ ] 所有功能 PR 已合并到 main
[ ] CI 全绿 (test + lint + coverage >= 60%)
[ ] 测试报告已编写 (docs/test-report-*.md)
[ ] 多节点 P2P 测试通过

Landing Page:
[ ] site/index.html 内容完整
[ ] How It Works 使用文字 + ASCII 示意图
[ ] Test Results 使用英文，包含实际数据
[ ] 风格与 OutpostNet 一致

Deployment:
[ ] wrangler.jsonc 配置正确
[ ] Cloudflare Pages 项目已关联 repo
[ ] Build command 为空，output dir 为 site
[ ] 部署成功，Landing page 在线可访问
[ ] 仓库可匿名读取（curl 200 / `GIT_TERMINAL_PROMPT=0 git -c credential.helper= ls-remote` 成功）

Release:
[ ] Docker image 已推送到 GHCR
[ ] GHCR 包可见性为 public（空 `DOCKER_CONFIG` 下仍可 pull）
[ ] GitHub Release 已创建（如需要）
[ ] 发布 issue 已关闭
```

---

## 经验教训 (Lessons Learned from 1.0)

1. **不要使用 `npx wrangler deploy` 作为 Cloudflare Pages build command** — 会导致 Workers 自动检测错误，纯静态站不需要 build command
2. **PR 合并需二次确认** — 在 GitHub UI 确认 PR 状态为 "Merged"，不要仅凭 commit hash 判断
3. **Landing page 风格对齐** — 先参考同系列产品（OutpostNet），再动手开发，避免过度工程化后返工
4. **测试报告用英文** — 技术产品面向国际社区，测试数据和性能指标统一使用英文
5. **Cloudflare Pages vs Workers 区分** — `wrangler.toml` 会触发 Workers 自动检测，使用 `wrangler.jsonc` 配合 `assets` 字段更明确
6. **公开页面不能指向私有产物** — 发布验证必须在**剥离本机凭证**的会话中执行安装命令：仓库检查用 `GIT_TERMINAL_PROMPT=0 git -c credential.helper= ls-remote`，镜像检查用空的 `DOCKER_CONFIG`。登录态的 `pull` / 带 credential helper 的 `ls-remote` 成功会掩盖权限问题：仓库或 GHCR 包为 private 时，外部用户第一步就失败，而维护者本地验证一切正常
