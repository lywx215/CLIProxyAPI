# Fork Synchronization Guide

此文档记录了如何维护本项目（Fork 版本），使其既能保留自定义修改（如 Zeabur 部署配置、COS 修复），又能同步原项目的最新功能。

> **Last updated**: 2026-06-02 — 基于 upstream `v7.1.40` 校验

> [!CAUTION]
> ## 🔒 核心原则：禁止覆盖本地修改
>
> **同步上游时，严禁自动覆盖本地的任何自定义修改。** 具体规则：
>
> 1. **Rebase 遇到冲突时，始终优先保留本地修改**（即本 fork 的自定义代码），除非用户明确指令覆盖
> 2. **第 4 节「自定义修改清单」中列出的所有文件**，在同步过程中必须保持本地版本不变
> 3. **禁止使用** `git checkout --theirs` 或 `git reset --hard upstream/main` 等会丢弃本地修改的命令
> 4. 只有在用户明确说「使用上游版本」或「覆盖本地修改」时，才可以采纳上游的变更覆盖本地文件
> 5. 如果不确定是否应该覆盖某个文件，**必须先询问用户确认**，不得擅自决定

## 1. 分支说明 (Branch Overview)

本 fork 维护 **两个分支**：

| 分支 | 说明 |
|------|------|
| `main` | 包含所有自定义修改（含 Model Version 别名重写功能） |
| `no-model-version` | 与 `main` 相同，但**去除了 Model Version 别名重写功能**（4 个 executor 文件与上游一致） |

> [!IMPORTANT]
> **每次同步上游时，必须同时更新两个分支。**

### `no-model-version` 与 `main` 的差异

仅以下 4 个文件不同（`no-model-version` 中与上游保持一致）：

| 文件 | `main` 分支 | `no-model-version` 分支 |
|------|-------------|------------------------|
| `internal/runtime/executor/antigravity_executor.go` | 调用 `RewriteResponseModelVersion` / `RewriteSSEModelVersion` | 直接传递原始 payload（与上游一致） |
| `internal/runtime/executor/gemini_cli_executor.go` | 同上 | 同上 |
| `internal/runtime/executor/gemini_executor.go` | 同上 | 同上 |
| `internal/runtime/executor/helps/payload_helpers.go` | 包含 `RewriteResponseModelVersion` / `RewriteSSEModelVersion` 函数 | 不包含这些函数（与上游一致） |

## 2. 初始设置 (Initial Setup)

如果我们在换了新环境，或者还没配置过 upstream，请执行：

```powershell
# 查看当前远程仓库
git remote -v
# 如果没有 upstream，则添加原仓库地址
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git
```

## 3. 日常同步流程 (Routine Sync)

当原项目有更新时，请按照以下步骤同步 **两个分支**：

### 第一步：获取更新
```powershell
git fetch upstream
```

### 第二步：同步 `main` 分支
```powershell
git checkout main
git rebase upstream/main
```

> **注意**：如果出现冲突（Conflict），Git 会提示您。
> 1. 打开冲突文件，手动保留需要的代码。
> 2. `git add <file>`
> 3. `git rebase --continue`

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

### 第四步：推送 `main` 到 GitHub
```powershell
git push -f origin main
```

### 第五步：同步 `no-model-version` 分支
```powershell
git checkout no-model-version
git rebase upstream/main
```

> [!NOTE]
> `no-model-version` 分支没有 Model Version 重写的 commit，所以它的 rebase 应该更简单。
> 如果上游修改了 4 个 executor 文件，`no-model-version` 通常不会冲突（因为它与上游一致）。

### 第六步：验证 `no-model-version` 编译
```powershell
go build ./...
```

### 第七步：推送 `no-model-version` 到 GitHub
```powershell
git push -f origin no-model-version
```

### 第八步：切回 `main` 并更新本文档
```powershell
git checkout main
```
更新本文件顶部的 `Last updated` 日期和版本号，然后提交推送：
```powershell
git add SYNC_GUIDE.md
git commit -m "docs: update SYNC_GUIDE last-synced date to vX.X.X"
git push origin main
```

### 第九步：验证同步结果
```powershell
# 确认 main 与上游的差异文件列表正确
git diff upstream/main..main --stat

# 确认 no-model-version 的 4 个 executor 文件与上游一致
git diff upstream/main no-model-version -- internal/runtime/executor/antigravity_executor.go internal/runtime/executor/gemini_cli_executor.go internal/runtime/executor/gemini_executor.go internal/runtime/executor/helps/payload_helpers.go
# 上面这条命令应该输出为空（无差异）
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

| 文件 | 修改内容 | 分支 |
|------|----------|------|
| `cmd/server/main.go` | 支持 `OBJECTSTORE_PREFIX` 环境变量，实现多服务器对象存储隔离 | 两个分支共有 |
| `config.example.yaml` | 与 `cmd/server/main.go` 配合的配置项调整；新增 `api-key-rate-limit` 配置段 | 两个分支共有 |
| `internal/api/middleware/ratelimit.go` | 新增 per-API-key 滑动窗口速率限制中间件 | 两个分支共有 |
| `internal/api/middleware/ratelimit_test.go` | 速率限制中间件单元测试 | 两个分支共有 |
| `internal/config/config.go` | 新增 `APIKeyRateLimit` 配置结构体 | 两个分支共有 |
| `internal/watcher/diff/config_diff.go` | 新增 rate limit 配置变更检测 | 两个分支共有 |
| `internal/runtime/executor/antigravity_executor.go` | 在 non-stream 和 stream 响应中重写 modelVersion 为客户端请求的别名 | **仅 `main`** |
| `internal/runtime/executor/gemini_cli_executor.go` | 同上，适用于 Gemini CLI executor | **仅 `main`** |
| `internal/runtime/executor/gemini_executor.go` | 同上，适用于 Gemini executor | **仅 `main`** |
| `internal/runtime/executor/helps/payload_helpers.go` | `RewriteResponseModelVersion` / `RewriteSSEModelVersion` 辅助函数 | **仅 `main`** |
| `internal/api/handlers/management/usage.go` | 恢复完整的“使用统计”管理 API 端点及导入/导出逻辑 | 两个分支共有 |
| `internal/api/handlers/management/handler.go` | `Handler` 结构体注入 `usageStats` 状态追踪机制 | 两个分支共有 |
| `internal/api/server.go` | 重新绑定并注册管理端 `/usage` 相关路由，并加载开关状态 | 两个分支共有 |

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
