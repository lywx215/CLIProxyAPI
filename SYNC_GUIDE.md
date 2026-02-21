# Fork Synchronization Guide

此文档记录了如何维护本项目（Fork 版本），使其既能保留自定义修改（如 Zeabur 部署配置、COS 修复），又能同步原项目的最新功能。

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
我们将本地的自定义修改“浮动”到最新的官方代码之上。
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

1.  **Deployment**: `Dockerfile` 和 `zbpack.json` 适配 Zeabur。
2.  **Fixes**:
    *   `internal/store/objectstore.go`: 迁移至 AWS SDK v2 以解决腾讯云 COS 兼容性问题。
    *   `internal/watcher/clients.go`: 移除 `persistAuthAsync` 以解决日志死循环。
    *   `internal/watcher/events.go`: 降级日志级别。
    *   `internal/api/server.go`: 将 `fmt.Printf` 替换为 `log.Debugf` 消除刷屏。
3.  **Config**: 支持 `OBJECTSTORE_PREFIX` 环境变量实现多服务器隔离。
4.  **临时模型定义 (Upstream 优先)**:
    *   `internal/registry/model_definitions_static_data.go`: 临时添加了 `gemini-3.1-pro-preview` 模型定义（添加于 2026-02-20），分布在 `GetGeminiModels`、`GetGeminiVertexModels`、`GetGeminiCLIModels`、`GetAIStudioModels` 四个函数中。
    *   ⚠️ **同步时以 fork 源 (upstream) 为准**：当 upstream 更新了对 `gemini-3.1-pro-preview` 的支持后（可能包含更完整的参数、别名映射等），应采用 upstream 的版本，丢弃本地临时定义。冲突时选择 upstream 的代码即可。

## 4. 本地专用文件 (Local-Only Files)

以下目录/文件仅存于本地，**不应提交到 Git**（已在 `.gitignore` 中排除）：

| 路径 | 说明 |
|------|------|
| `zeaburcli/` | Zeabur 多服务器部署工具，包含 `deploy_config.yaml`（含 API Token 等敏感信息） |
| `.env` | 环境变量配置，包含各类密钥和凭证 |
| `config.yaml` | 运行时配置，包含服务端口、数据库连接等本地设置 |

> ⚠️ **注意**：如需在新机器上使用部署脚本，请从安全渠道获取 `zeaburcli/` 目录，切勿将其推送到公开仓库。
