# Fork Synchronization Guide

此文档记录了如何维护本项目（Fork 版本），使其既能保留自定义修改（如 Zeabur 部署配置、COS 修复），又能同步原项目的最新功能。

> **Last updated**: 2026-06-15 — 基于 upstream `f33bc56b` (post v7.1.18) 校验

> [!CAUTION]
> ## 🔒 核心原则：每次同步必须保留全部自定义修改
>
> **同步上游 (rebase) 时，严禁丢失本地的任何自定义修改。** 具体规则：
>
> 1. **Rebase 遇到冲突时，始终优先保留本地修改**（即本 fork 的自定义代码），除非用户明确指令覆盖
> 2. **第 4 节「自定义修改清单」中列出的所有文件**，在同步过程中必须保持本地版本不变
> 3. **禁止使用** `git checkout --theirs` 或 `git reset --hard upstream/main` 等会丢弃本地修改的命令
> 4. 只有在用户明确说「使用上游版本」或「覆盖本地修改」时，才可以采纳上游的变更覆盖本地文件
> 5. 如果不确定是否应该覆盖某个文件，**必须先询问用户确认**，不得擅自决定
> 6. **当冲突涉及清单中的文件时**，必须同时保留「我们的自定义逻辑」和「上游新增的功能」，不得二选一丢弃任何一方
> 7. **rebase 完成后，必须执行第五步的验证命令**，确认所有自定义修改仍然存在于差异列表中

## 1. 分支说明 (Branch Overview)

本 fork 仅维护 **`main`** 分支，包含所有自定义修改（含 Model Version 别名重写功能）。

## 2. 初始设置 (Initial Setup)

如果我们在换了新环境，或者还没配置过 upstream，请执行：

```powershell
# 查看当前远程仓库
git remote -v
# 如果没有 upstream，则添加原仓库地址
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git
```

## 3. 日常同步流程 (Routine Sync)

当原项目有更新时，请按照以下步骤同步：

### 第一步：获取更新
```powershell
git fetch upstream --no-tags
```

> [!WARNING]
> **必须使用 `--no-tags`**，否则会拉取上游的 tag（如 `v7.1.40`），导致后续发布时：
> - `git tag v7.1.40` 报 "tag already exists"（指向上游 commit）
> - 推送该 tag 会触发上游的旧 workflow（DockerHub），而非我们的 GHCR workflow
> - 如果已经误拉了上游 tag，用 `git tag -d <tagname>` 删除后再创建

### 第二步：同步 `main` 分支
```powershell
git checkout main
git rebase upstream/main
```

> [!WARNING]
> **冲突处理原则 — 自定义修改优先：**
> 1. 打开冲突文件，**优先保留我们的自定义代码**
> 2. 如果上游也新增了有价值的功能（如新函数、新字段），应**同时保留双方代码**，而非二选一
> 3. 典型场景：上游在同一位置新增了 `setServiceTierMetadata`，我们有 `parseModelTokenLimit` → **两者都保留**
> 4. `git add <file>` → `git rebase --continue`
> 5. 使用 `git -c core.editor=true rebase --continue` 可跳过编辑器弹窗

> [!CAUTION]
> **处理 `go.mod` 冲突时务必小心！**
> - 冲突标记之间可能同时包含**我们要删除的依赖**和**上游新增的依赖**
> - 仔细检查每一行，确保不要误删上游需要的包（如 `utls`、`minio-go` 等）
> - 解决冲突后建议运行 `go build ./...` 验证编译是否通过
> - 如果构建失败提示缺少包，使用 `go get <package>` 添加后再提交

### 第三步：验证 `main` 编译
```powershell
go build ./...
```

### 第四步：验证自定义修改完整性（必须执行）

> [!IMPORTANT]
> **此步骤为强制步骤，不得跳过。** 必须确认所有自定义修改仍保留在差异中。

```powershell
# 1. 查看与上游的差异文件列表
git diff upstream/main..main --stat
```

**必须确认以下关键文件出现在差异列表中（缺少任何一个说明自定义修改丢失）：**

| 必须出现的文件 | 自定义功能 |
|---------------|------------|
| `Dockerfile` | Zeabur 部署适配 |
| `zbpack.json` | Zeabur 构建类型 |
| `config.example.yaml` | 自定义配置项 |
| `internal/store/objectstore.go` | 腾讯云 COS 兼容 |
| `internal/watcher/clients.go` | persistAuth 禁用 |
| `internal/watcher/events.go` | 日志降级 |
| `sdk/cliproxy/auth/conductor.go` | persist 禁用 + CreditsUsed |
| `internal/api/middleware/ratelimit.go` | Rate Limit 中间件 |
| `internal/config/config.go` | APIKeyRateLimit + CreditsForce 配置 |
| `internal/runtime/executor/antigravity_executor.go` | ModelVersion 重写 |
| `internal/runtime/executor/gemini_cli_executor.go` | ModelVersion 重写 |
| `internal/runtime/executor/gemini_executor.go` | ModelVersion 重写 |
| `internal/runtime/executor/helps/payload_helpers.go` | Rewrite 辅助函数 |

```powershell
# 2. 快速验证 ModelVersion 重写函数是否存在
Select-String -Pattern "RewriteResponseModelVersion" internal/runtime/executor/helps/payload_helpers.go
# 应输出函数定义行，如果无输出说明丢失！

# 3. 快速验证 Rate Limit 中间件是否存在
Test-Path internal/api/middleware/ratelimit.go
# 应输出 True
```

> 如果发现任何自定义修改丢失，**立即执行 `git rebase --abort` 或从 reflog 恢复**，不要推送！

### 第五步：推送 `main` 到 GitHub
```powershell
git push -f origin main
```

### 第六步：发布新版本（必须执行）

> [!IMPORTANT]
> **每次同步完成并推送后，必须创建并推送版本 tag 以触发 GHCR Docker 镜像构建。**

```powershell
# 1. 查看上游最新 tag
git ls-remote --tags upstream | Select-Object -Last 5

# 2. 创建与上游相同的版本 tag（指向我们的 main HEAD）
git tag v7.x.x

# 3. 推送 tag 到 origin，触发 GitHub Actions 构建
git push origin v7.x.x
```

> [!WARNING]
> - Tag 名称应与上游最新版本保持一致（如上游发布 `v7.2.4`，我们也打 `v7.2.4`）
> - 如果本地已存在同名 tag（从上游误拉），先用 `git tag -d v7.x.x` 删除再重建
> - 推送 tag 后检查 GitHub Actions 页面确认 workflow 正常触发

### 第七步：更新本文档
更新本文件顶部的 `Last updated` 日期和版本号，然后提交推送：
```powershell
git add SYNC_GUIDE.md
git commit -m "docs: update SYNC_GUIDE last-synced date to vX.X.X"
git push origin main
```

## 4. 自定义修改清单 (Custom Modifications)

我们维护的自定义修改主要包括：

### 4.1 部署相关 (Deployment)

| 文件 | 修改内容 |
|------|----------|
| `Dockerfile` | 适配 Zeabur 部署；使用 `config.yaml`（而非 `config.example.yaml`）作为 Bootstrap 模板 |
| `zbpack.json` | 指定 `build_type: dockerfile` 以兼容 Zeabur 构建 |
| `.dockerignore` | 添加额外忽略项（如编辑器/Agent 目录） |
| `config.example.yaml` | 默认追加 `disable-auto-update-panel: true` 防止 Zeabur 覆盖本地前端产物 |

### 4.2 Bug 修复 (Fixes)

| 文件 | 修改内容 |
|------|----------|
| `internal/store/objectstore.go` | 修改 S3 客户端实现以解决腾讯云 COS 兼容性问题 |
| `internal/watcher/clients.go` | 移除 `persistAuthAsync` 调用以解决日志死循环 |
| `internal/api/server.go` | 将 `fmt.Printf` 替换为 `log.Debugf`，消除 `UpdateClients` 刷屏日志；集成 rate limit 中间件 |
| `internal/watcher/events.go` | 将增量处理日志从 `Infof` 降级为 `Debugf`，减少运行时刷屏 |
| `internal/wsrelay/manager.go` | 将 `fmt.Printf` 替换为 logrus `log.Warnf`，统一日志框架 |
| `sdk/cliproxy/auth/conductor.go` | 注释 `MarkResult` 中的 `persist` 调用，避免每次请求完成都上传凭证到 COS |
| `internal/managementasset/updater.go` | 修改 fallback URL 为自托管地址，简化 fallback 逻辑 |

### 4.3 功能增强 (Enhancements)

| 文件 | 修改内容 |
|------|----------|
| `cmd/server/main.go` | 支持 `OBJECTSTORE_PREFIX` 环境变量，实现多服务器对象存储隔离 |
| `config.example.yaml` | 与 `cmd/server/main.go` 配合的配置项调整；新增 `api-key-rate-limit` 配置段 |
| `internal/api/middleware/ratelimit.go` | 新增 per-API-key 滑动窗口速率限制中间件 |
| `internal/api/middleware/ratelimit_test.go` | 速率限制中间件单元测试 |
| `internal/config/config.go` | 新增 `APIKeyRateLimit` 配置结构体 |
| `internal/watcher/diff/config_diff.go` | 新增 rate limit 配置变更检测 |
| `internal/runtime/executor/antigravity_executor.go` | 在 non-stream 和 stream 响应中重写 modelVersion 为客户端请求的别名 |
| `internal/runtime/executor/gemini_cli_executor.go` | 同上，适用于 Gemini CLI executor |
| `internal/runtime/executor/gemini_executor.go` | 同上，适用于 Gemini executor |
| `internal/runtime/executor/helps/payload_helpers.go` | `RewriteResponseModelVersion` / `RewriteSSEModelVersion` 辅助函数 |
| `internal/api/handlers/management/usage.go` | 恢复完整的"使用统计"管理 API 端点及导入/导出逻辑 |
| `internal/api/handlers/management/handler.go` | `Handler` 结构体注入 `usageStats` 状态追踪机制 |
| `internal/api/server.go` | 重新绑定并注册管理端 `/usage` 相关路由，并加载开关状态 |

> [!NOTE]
> **前端依赖同步**：在使用统计功能方面，`Cli-Proxy-API-Management-Center` 的 `src/i18n/locales/*.json` (特别是 `zh-CN.json` 和 `en.json`) 中的 `usage_stats` 国际化翻译也为我们独有功能，同步前端仓库时也需务必保留。

### 4.4 辅助文件 (Auxiliary)

| 文件 | 修改内容 |
|------|----------|
| `.gitignore` | 添加 `zeaburcli/` 及编辑器/Agent 目录忽略规则 |
| `go.mod` / `go.sum` | 随上述代码修改引入的依赖变更 |
| `SYNC_GUIDE.md` | 本文档（仅存在于 fork） |
| `assets/cubence.png` | 自定义品牌资源 |
| `test/config_migration_test.go` | 配置迁移测试 |
| `sdk/cliproxy/auth/persist_policy_test.go` | 持久化策略测试 |

> [!IMPORTANT]
> **同步后请检查**：每次 rebase 后，运行 `git diff upstream/main..main --stat` 确认差异文件列表与上述清单一致。如果有新增或消失的差异，请更新本文档。

## 5. 已被上游采纳的修改 (Superseded)

以下修改最初由本 fork 添加，但已被上游 (upstream) 采纳，**不再需要维护**：

| 修改内容 | 采纳版本 |
|----------|----------|
| `gemini-3.1-pro-preview` 临时模型定义（静态文件） | ≤ v6.8.52（上游已改用网络动态模型目录，静态定义文件已删除） |

> [!TIP]
> 当上游合并了我们的某个修改后，下次 rebase 时冲突会自动消失。确认采纳后将其移至此表。

## 6. 本地专用文件 (Local-Only Files)

以下目录/文件仅存于本地，**不应提交到 Git**（已在 `.gitignore` 中排除）：

| 路径 | 说明 |
|------|------|
| `zeaburcli/` | Zeabur 多服务器部署工具，包含 `deploy_config.yaml`（含 API Token 等敏感信息） |
| `sshcli/` | 包含 SSH/VPS 部署相关脚本和 `vps_deploy_config.yaml` 配置文件等敏感信息 |
| `.env` | 环境变量配置，包含各类密钥和凭证 |
| `config.yaml` | 运行时配置，包含服务端口、数据库连接等本地设置 |

> ⚠️ **注意**：如需在新机器上使用部署脚本，请从安全渠道获取 `zeaburcli/` 或 `sshcli/` 目录，切勿将其推送到公开仓库。如果在同步 (rebase) 期间上游删除了这些文件夹或文件（例如为了清理敏感信息），务必**不要**在本地物理删除它们，确保从备份中恢复它们并维持在 `.gitignore` 列表中。

## 7. 前端管理面板同步 (Frontend Management Center Sync)

前端仓库 `Cli-Proxy-API-Management-Center` 也是 fork，包含大量自定义功能（使用统计页面、Antigravity 积分统计、Rate Limit 配置 UI 等），**每次后端同步后也必须同步前端**。

> [!CAUTION]
> ## 🔒 前端同步核心原则
>
> **与后端同步规则完全一致：严禁丢失本地的任何自定义修改。**
>
> 1. **Rebase 遇到冲突时，始终优先保留本地修改**
> 2. **上游删除的文件，如果本地仍有自定义内容，不可以跟随删除** — 必须手动恢复
> 3. 特别注意：上游可能重构或删除了我们仍在使用的组件（如 `PluginsPage`、`usage` 目录），这些必须保留
> 4. 冲突中如果涉及 i18n 翻译文件，必须合并双方翻译键，不可丢弃我们新增的键

> [!WARNING]
> ## ⚠️ Rebase 中 `--ours` / `--theirs` 语义反转
>
> **在 `git rebase` 中，`--ours` 和 `--theirs` 的含义与 `git merge` 相反！**
>
> | 命令 | Rebase 中的含义 |
> |------|----------------|
> | `git checkout --ours <file>` | **上游 (upstream)** 的版本 — 会丢失我们的自定义修改！ |
> | `git checkout --theirs <file>` | **我们的 (fork)** 本地修改 — 这才是要保留的 |
>
> **2026-06-15 教训**：使用 `git checkout --ours` 解决 `src/types/visualConfig.ts` 冲突时，
> 实际采用了上游版本，导致 `ApiKeyRateLimitConfig`、`SpeedThrottleConfig` 等自定义类型定义丢失，
> `useVisualConfig.ts` 中的 `rateLimit`/`parsedOverrides` 变量声明和 `quotaAntigravityCreditsForce` 字段也被删除，
> 最终 `bun run build` 失败并需要手动补回所有丢失的类型和字段。
>
> **正确做法**：
> - 需要保留本地修改时，使用 `git checkout --theirs <file>`
> - 或者手动编辑冲突文件，合并双方内容
> - **严禁对包含自定义类型定义的文件使用 `--ours`**

### 7.1 初始设置

```powershell
cd D:\code\gemini\2fa\CLIProxy\Cli-Proxy-API-Management-Center

# 查看远程仓库
git remote -v
# 如果没有 upstream，则添加
git remote add upstream https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git
```

### 7.2 同步流程

```powershell
# 第一步：获取上游更新
git fetch upstream --no-tags

# 第二步：Rebase
git checkout main
git rebase upstream/main
# 冲突处理规则与后端完全相同：优先保留本地，合并上游新功能

# 第三步：构建验证
bun install
bun run build
# 确保构建成功，无 TypeScript 编译错误

# 第四步：验证自定义修改完整性
git diff upstream/main..main --stat
# 确认下方清单中所有文件都出现在差异列表中

# 第五步：推送
git push -f origin main
```

### 7.3 前端自定义修改清单

> [!IMPORTANT]
> **以下文件/目录必须在差异列表中出现，缺少任何一个说明自定义修改丢失：**

#### 使用统计页面（完全自定义，上游不存在）

| 文件/目录 | 说明 |
|-----------|------|
| `src/pages/UsagePage.tsx` | 使用统计主页面 |
| `src/pages/UsagePage.module.scss` | 使用统计样式 |
| `src/components/usage/` （整个目录） | 统计图表组件：CostTrendChart、StatCards、ModelStatsCard 等 |
| `src/utils/usage.ts` | 使用统计数据处理工具 |
| `src/utils/usage/` （整个目录） | 图表配置、延迟计算等工具 |
| `src/stores/useUsageStatsStore.ts` | 使用统计状态管理 |
| `src/services/api/usage.ts` | 使用统计 API 接口 |
| `src/types/usage.ts` | 使用统计类型定义 |

#### Antigravity 积分统计（完全自定义）

| 文件 | 说明 |
|------|------|
| `src/pages/AntigravityStatsPage.tsx` | Antigravity 积分统计页面 |
| `src/pages/AntigravityStatsPage.module.scss` | 积分统计样式 |
| `src/services/api/antigravityStats.ts` | 积分统计 API |

#### Rate Limit 与 Speed Throttle 配置 UI

| 文件 | 说明 |
|------|------|
| `src/components/config/VisualConfigEditor.tsx` | 新增 Rate Limit / Speed Throttle 编辑器集成 |
| `src/components/config/VisualConfigEditorBlocks.tsx` | Rate Limit 配置块 + Speed Throttle 配置块 |

#### 类型定义（极易在冲突解决中丢失！）

> [!CAUTION]
> **此文件是冲突高发区**，`--ours` 误用会直接导致以下自定义类型全部丢失，构建必定失败。

| 文件 | 必须包含的自定义内容 |
|------|---------------------|
| `src/types/visualConfig.ts` | 必须导出 `ApiKeyRateLimitOverride`、`ApiKeyRateLimitConfig`、`SpeedThrottleConfig` 接口 |
| `src/types/visualConfig.ts` | `VisualConfigValues` 类型中必须包含 `apiKeyRateLimit: ApiKeyRateLimitConfig` 和 `speedThrottle: SpeedThrottleConfig` 字段 |
| `src/types/visualConfig.ts` | `DEFAULT_VISUAL_VALUES` 中必须包含 `apiKeyRateLimit` 和 `speedThrottle` 的默认值 |

#### Hook 逻辑（极易在冲突解决中丢失！）

> [!CAUTION]
> **此文件与 `visualConfig.ts` 配套**，每次冲突解决后必须验证以下内容完整。

| 文件 | 必须包含的自定义内容 |
|------|---------------------|
| `src/hooks/useVisualConfig.ts` | YAML 解析部分必须有 `const rateLimit = asRecord(parsed['api-key-rate-limit'])` 变量声明 |
| `src/hooks/useVisualConfig.ts` | 必须有 `parsedOverrides` 变量（解析 `rateLimit.overrides` 数组） |
| `src/hooks/useVisualConfig.ts` | `newValues` 对象中必须包含 `quotaAntigravityCreditsForce` 字段 |
| `src/hooks/useVisualConfig.ts` | `newValues` 对象中必须包含 `apiKeyRateLimit` 和 `speedThrottle` 配置对象 |
| `src/hooks/useVisualConfig.ts` | `applyVisualChangesToYaml` 中必须有 rate limit YAML 序列化逻辑（`api-key-rate-limit` 部分） |

#### 国际化翻译（关键）

| 文件 | 说明 |
|------|------|
| `src/i18n/locales/zh-CN.json` | 中文翻译（含 `usage_stats`、`antigravity_stats`、`api_key_rate_limit`、`speed_throttle` 等键） |
| `src/i18n/locales/en.json` | 英文翻译（同上） |
| `src/i18n/locales/zh-TW.json` | 繁体中文翻译 |

#### 路由与布局

| 文件 | 说明 |
|------|------|
| `src/router/MainRoutes.tsx` | 必须同时包含 plugins 路由（上游）和 usage/antigravity-stats 路由（本地自定义） |
| `src/components/layout/MainLayout.tsx` | 侧边栏必须同时包含 plugins 菜单（上游）和自定义菜单项 |

#### 其他自定义

| 文件 | 说明 |
|------|------|
| `src/features/providers/sheets/forms/BaseProviderForm.tsx` | Antigravity credits-force 切换开关 |
| `src/hooks/useVisualConfig.ts` | 自定义配置项扩展（Rate Limit / Speed Throttle / Credits Force） |
| `src/pages/SystemPage.tsx` | 包含插件系统相关 UI + 自定义版本显示逻辑 |
| `vite.config.ts` | 构建配置调整 |

### 7.4 构建与类型验证（必须执行）

> [!WARNING]
> 前端同步完成后，**必须先构建验证，再提交**。构建失败说明类型定义或字段丢失。

```powershell
# 1. 构建前端（包含 TypeScript 类型检查）
bun run build
# 如果构建失败，检查以下常见丢失项：
#   - src/types/visualConfig.ts 中是否缺少 ApiKeyRateLimitConfig / SpeedThrottleConfig
#   - src/hooks/useVisualConfig.ts 中是否缺少 rateLimit / parsedOverrides 变量
#   - newValues 对象中是否缺少 apiKeyRateLimit / speedThrottle / quotaAntigravityCreditsForce

# 2. 快速验证关键类型是否存在
Select-String -Pattern 'ApiKeyRateLimitConfig|SpeedThrottleConfig' src/types/visualConfig.ts
# 应输出至少 4 行（接口定义 + 字段引用），如果无输出说明类型丢失！

Select-String -Pattern 'rateLimit|parsedOverrides|quotaAntigravityCreditsForce' src/hooks/useVisualConfig.ts
# 应输出多行，如果无输出说明解析逻辑丢失！

# 3. 提交构建产物
git add dist/
git commit -m "build: update bundled index.html after upstream sync"
git push origin main
```

### 7.5 同步到后端仓库

构建完成后，需要将 `dist/index.html` 复制到后端仓库的 `static/management.html`：

```powershell
# 复制构建产物到后端
Copy-Item -Path "dist/index.html" -Destination "D:\code\gemini\2fa\CLIProxy\CLIProxyAPI\static\management.html" -Force

# 在后端仓库提交
cd D:\code\gemini\2fa\CLIProxy\CLIProxyAPI
git add static/management.html
git commit -m "build: update management.html from frontend sync"
git push origin main
```

