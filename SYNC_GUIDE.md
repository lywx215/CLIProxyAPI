# Fork Synchronization Guide

此文档记录了如何维护本项目（Fork 版本），使其既能保留自定义修改（如 Zeabur 部署配置、COS 修复），又能同步原项目的最新功能。

> **Last updated**: 2026-04-09 — 基于 upstream `v6.9.18` 校验

## 1. 初始设置 (Initial Setup)

如果我们在换了新环境，或者还没配置过 upstream，请执行：

```powershell
# 查看当前远程仓库
git remote -v
# 如果没有 upstream，则添加原仓库地址
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git
```

## 2. 日常同步流程 (Routine Sync)

当原项目有更新时，请按照以下步骤同步：

### 第一步：获取更新
```powershell
git fetch upstream
```

### 第二步：变基 (Rebase)
我们将本地的自定义修改"浮动"到最新的官方代码之上。
```powershell
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

### 第三步：推送到 GitHub
由于变基修改了提交历史，必须使用强制推送。
```powershell
git push -f origin main
```

## 3. 自定义修改清单 (Custom Modifications)

我们维护的自定义修改主要包括：

### 3.1 部署相关 (Deployment)

| 文件 | 修改内容 |
|------|----------|
| `Dockerfile` | 适配 Zeabur 部署；使用 `config.yaml`（而非 `config.example.yaml`）作为 Bootstrap 模板 |
| `zbpack.json` | 指定 `build_type: dockerfile` 以兼容 Zeabur 构建 |
| `.dockerignore` | 添加额外忽略项（如编辑器/Agent 目录） |

### 3.2 Bug 修复 (Fixes)

| 文件 | 修改内容 |
|------|----------|
| `internal/store/objectstore.go` | 修改 S3 客户端实现以解决腾讯云 COS 兼容性问题 |
| `internal/watcher/clients.go` | 移除 `persistAuthAsync` 调用以解决日志死循环 |
| `internal/api/server.go` | 将 `fmt.Printf` 替换为 `log.Debugf`，消除 `UpdateClients` 刷屏日志 |
| `internal/watcher/events.go` | 将增量处理日志从 `Infof` 降级为 `Debugf`，减少运行时刷屏 |
| `internal/wsrelay/manager.go` | 将 `fmt.Printf` 替换为 logrus `log.Warnf`，统一日志框架 |
| `sdk/cliproxy/auth/conductor.go` | 注释 `MarkResult` 中的 `persist` 调用，避免每次请求完成都上传凭证到 COS |

### 3.3 功能增强 (Enhancements)

| 文件 | 修改内容 |
|------|----------|
| `cmd/server/main.go` | 支持 `OBJECTSTORE_PREFIX` 环境变量，实现多服务器对象存储隔离 |
| `config.example.yaml` | 与 `cmd/server/main.go` 配合的配置项调整 |
| `internal/runtime/executor/antigravity_executor.go` | 在 non-stream 和 stream 响应中重写 modelVersion 为客户端请求的别名 |
| `internal/runtime/executor/gemini_cli_executor.go` | 同上，适用于 Gemini CLI executor |
| `internal/runtime/executor/gemini_executor.go` | 同上，适用于 Gemini executor |
| `internal/runtime/executor/payload_helpers.go` | `rewriteResponseModelVersion` / `rewriteSSEModelVersion` 辅助函数 |

### 3.4 辅助文件 (Auxiliary)

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

## 4. 已被上游采纳的修改 (Superseded)

以下修改最初由本 fork 添加，但已被上游 (upstream) 采纳，**不再需要维护**：

| 修改内容 | 采纳版本 |
|----------|----------|
| `gemini-3.1-pro-preview` 临时模型定义（静态文件） | ≤ v6.8.52（上游已改用网络动态模型目录，静态定义文件已删除） |

> [!TIP]
> 当上游合并了我们的某个修改后，下次 rebase 时冲突会自动消失。确认采纳后将其移至此表。

## 5. 本地专用文件 (Local-Only Files)

以下目录/文件仅存于本地，**不应提交到 Git**（已在 `.gitignore` 中排除）：

| 路径 | 说明 |
|------|------|
| `zeaburcli/` | Zeabur 多服务器部署工具，包含 `deploy_config.yaml`（含 API Token 等敏感信息） |
| `.env` | 环境变量配置，包含各类密钥和凭证 |
| `config.yaml` | 运行时配置，包含服务端口、数据库连接等本地设置 |

> ⚠️ **注意**：如需在新机器上使用部署脚本，请从安全渠道获取 `zeaburcli/` 目录，切勿将其推送到公开仓库。
