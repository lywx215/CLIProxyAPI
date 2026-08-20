# Fork Synchronization Guide

本文档定义后端 `CLIProxyAPI` 与自定义管理前端 `Cli-Proxy-API-Management-Center` 的上游同步规则。

> Last updated: 2026-08-20
> Backend upstream: `85d2fadd` / `v7.2.137`
> Management frontend upstream: `6586f888` / `v1.22.6`

## 1. 核心原则

1. **本地优先是行为级约束，不是逐字保留旧文件。** 必须保留下文列出的 fork 行为，同时采用上游的新架构、新修复和新测试。
2. 使用普通 `merge --no-ff` 保留双方历史；不使用 rebase，不重写已有 fork 提交，不强推。
3. 合并前先用固定 SHA 执行 `merge-tree` 预演，并按功能组说明冲突文件、双方意图和推荐结果。
4. 用户确认前不得执行真实 merge 或编辑冲突。若出现预演外冲突、语义取舍变化或无法同时保留双方行为，必须暂停并再次确认。
5. 禁止用整文件 `ours`/`theirs` 快捷覆盖。应以上游新结构为落点，逐项移植本地行为。
6. 依赖文件以上游版本为基线，只补回实际使用的 fork 依赖，再由包管理器生成锁文件。
7. 同步分支完成验证之前，不更新 `main`、不推送、不创建 tag、不发布。
8. 仅暂存明确路径；禁止 `git add .` 和 `git add -A`。

## 2. 仓库与远程

| 仓库 | origin | upstream |
| --- | --- | --- |
| 后端 | `https://github.com/lywx215/CLIProxyAPI.git` | `https://github.com/router-for-me/CLIProxyAPI.git` |
| 管理前端 | `https://github.com/lywx215/Cli-Proxy-API-Management-Center.git` | `https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git` |

后端和前端获取上游时都必须使用 `--no-tags`，防止把上游 tag 带入 fork 的发布命名空间：

```powershell
git remote add upstream <upstream-url> # 仅在 remote 不存在时执行
git fetch upstream --no-tags
```

不要在同步任务中追逐后续提交。开始时记录目标 SHA，后续所有预演、合并和祖先校验都使用这个固定 SHA。如果需要改用更新目标，应重新开始冲突审计并获得确认。

## 3. 可恢复同步流程

每次同步使用两个本地分支：

- `codex/backup-main-<fork-short-sha>`：同步前的本地保护引用。
- `codex/sync-upstream-<version>`：实际合并和验证分支。

在后端工作区维护忽略文件 `.codex/sync-upstream-progress.md`，记录检查点、两个仓库的分支和 HEAD、固定 upstream SHA、merge 状态、未解决冲突组、已运行命令、测试结果和下一条安全操作。

恢复任务时先执行：

```powershell
Get-Content .codex/sync-upstream-progress.md
git status --short --branch
git rev-parse HEAD
git rev-parse -q --verify MERGE_HEAD
```

前端也执行同样的 `status`、`HEAD` 和 `MERGE_HEAD` 核验。若 merge 正在进行，继续当前冲突组；不得自动 abort、reset 或重新开始。

### 3.1 前置保护与冲突预演

```powershell
git status --short
git branch codex/backup-main-<fork-short-sha> <fork-sha>
git switch -c codex/sync-upstream-<version> <fork-sha>
git merge-tree --write-tree --messages <fork-sha> <fixed-upstream-sha>
```

按配置/依赖、认证/存储、executor/限速、管理 API、模型/translator、测试/CI 等功能组整理后端冲突；前端按依赖、Config、Quota/AuthFiles、OAuth、路由/状态、i18n 整理。确认必须针对行为结果，而不是简单选择某一侧文件。

### 3.2 执行真实合并

用户批准冲突方案后：

```powershell
git merge --no-ff --no-commit <fixed-upstream-sha>
```

逐组处理冲突并只暂存已审查的明确路径。全部冲突解决、测试通过后再创建 merge commit。完成的 merge commit 必须有两个父提交，且 fork 基线和固定 upstream SHA 都是其祖先：

```powershell
git show -s --format='%H%n%P%n%s' HEAD
git merge-base --is-ancestor <fork-sha> HEAD
git merge-base --is-ancestor <fixed-upstream-sha> HEAD
```

如最后决定合入 `main`，应在另一次明确授权后使用 `--ff-only`；同步任务本身不执行该操作：

```powershell
git switch main
git merge --ff-only codex/sync-upstream-<version>
```

普通 merge 的回滚方式是对 merge commit 执行 `git revert -m 1 <merge-sha>`。不要为了备份创建 tag，因为 tag 可能触发发布 workflow。

## 4. 后端行为契约

同步后必须保留以下 fork 行为。上游拆分文件时，应把行为迁移到新的对应模块，不要求旧文件继续存在。

### 4.1 流量、响应与模型

- `api-key-rate-limit`：滑动窗口、默认 RPM、按 key overrides、热更新和 `/v1`/`/v1beta` 中间件。
- `speed-throttle`：最大并发、队列、首个完整 SSE data frame 的首包延迟，以及后续 token 节流。
- 模型名中的 `-Nm` 输入 token 限制。
- Gemini、Gemini CLI、Antigravity 等响应中的 `ModelVersion`/alias 回写。
- 模型目录显式 `display-name` 优先；未配置时展示客户端可见 alias，而不是上游内部 name。
- OpenAI、Gemini、Claude、Responses/WebSocket 等错误路径返回固定安全消息，不泄露上游响应、URL、凭证或内部细节。
- 保留 fork 的日志降噪和结构化日志行为。

### 4.2 Antigravity、Usage 与认证

- `antigravity-credits-force`、Credits 请求选择、`CreditsUsed` 记录和全部 credit 类型统计。
- Usage 与 Antigravity Stats 管理端点、导入/导出/重置和统计日志。
- Gemini CLI OAuth、配额与 usage 解析。
- Vertex ADC `authorized_user` 导入、刷新与 executor 支持，同时保留 service account。
- Claude translator 不转发 `temperature`，采用上游兼容性修复。
- 保留上游自包含、无外部敏感 fixture 的 Claude Code sentinel 测试。

### 4.3 存储、配置与部署

- 腾讯云 COS 的 AWS SDK v2 实现和 `OBJECTSTORE_PREFIX`。
- 禁止每个请求高频写回 auth，避免 COS 上传/日志循环；同时必须保留上游独立的 cooldown 状态持久化。
- `.gitignore` 必须为 UTF-8、无 NUL、去重，并保留本地敏感/构建目录规则。
- Zeabur、Docker、GHCR workflow 和 fork 自托管管理页。
- `config.example.yaml` 中的所有 fork 配置示例。

依赖以上游 `go.mod` 为基线，补回 COS 仍引用的 AWS SDK，然后执行 `go mod tidy`；不得手工拼接旧 `go.sum`。

## 5. 管理前端行为契约

前端采用上游 `src/features/*` 架构，删除被新架构替代的重复旧页面，但必须迁移其本地功能：

- Usage 和 Antigravity Stats 页面、API、store、图表、路由和菜单。
- API-key Rate Limit、Speed Throttle、CreditsForce 的类型、默认值、dirty tracking、YAML 解析/写回、搜索索引和编辑 UI。
- Gemini CLI OAuth `project_id`、配额 provider、分桶聚合和重置时间。
- Antigravity tier 与全部 credit 类型的汇总显示。
- Vertex ADC `authorized_user` 凭证导入。
- `/usage`、`/antigravity-stats` 路由及 Usage store 导出。
- Windows 构建兼容逻辑、前后端独立 build time。
- 四种语言的所有本地独有键；同名等价键采用上游最新措辞，语义变化必须再次确认。

`package.json` 使用上游的 `0.0.0`，正式版本由 Git tag/VERSION 注入；依赖以上游为基线，保留 `chart.js` 和 `react-chartjs-2`，使用 Bun 重新生成 `bun.lock`。

## 6. 验证

### 6.1 后端

Go 变更必须格式化，并审查是否产生无关格式漂移：

```powershell
gofmt -w .
go mod tidy
git diff --check
go test -count=1 ./...
go build ./...
go build -o .codex/build/cli-proxy-api.exe ./cmd/server
```

还应运行配置、限流、Speed Throttle、watcher、COS、管理 API、executor、handlers、cliproxy auth/service 的定向测试，并交叉编译 Linux amd64/arm64 server。Windows Application Control 阻止生成测试程序或 `.git` 临时目录操作时，应记录准确包、测试名、错误和复现结果；断言失败不能作为环境阻断跳过。

本轮 Windows 验证不代替发布前的 Linux 全量/race 测试和 Docker 构建。

### 6.2 管理前端

```powershell
bun install
bun install --frozen-lockfile
bun run verify
bun run type-check
bun run lint
bun run build
git diff --check
```

必须额外检查配置 YAML 解析/写回、Usage/Antigravity Stats、Quota、OAuth、路由、菜单和四个语言文件。

### 6.3 嵌入管理页

前端构建成功后，将单文件产物复制到后端并核验哈希：

```powershell
Copy-Item -LiteralPath ..\management-center\dist\index.html -Destination .\static\management.html -Force
Get-FileHash -Algorithm SHA256 ..\management-center\dist\index.html
Get-FileHash -Algorithm SHA256 .\static\management.html
```

两个 SHA-256 必须完全一致。前端构建时间会改变产物内容，因此应在最后一次成功构建后再复制和暂存。

## 7. 最终审计与交付

1. 对比 `upstream/main..同步分支`，逐项验证第 4、5 节行为契约仍存在。
2. 检查同步分支同时包含 fork 基线和固定 upstream SHA。
3. 检查没有 `.env`、`config.yaml`、`auths/`、凭证 JSON、私钥、部署密钥或其他敏感内容进入提交。
4. 检查两个工作区干净，`main` 和远程引用未变化。
5. 输出冲突决策、完整测试结果、环境阻断、未执行的发布前检查以及两个本地提交 SHA。
6. 只有两个本地同步分支均已提交并完成验证后，才能把持久同步目标标记为完成。

推送、tag、GHCR、Docker 发布和 `main` 更新均属于独立操作，必须获得单独明确授权。

## 8. 2026-08-20 同步记录

- 后端从 fork `5f510e0b` 合并固定 upstream `85d2fadd` (`v7.2.137`)，使用本地分支 `codex/sync-upstream-v7.2.137`。
- 管理前端从 fork `28a3deec` 合并固定 upstream `6586f888` (`v1.22.6`)，使用本地分支 `codex/sync-upstream-v1.22.6`。
- 后端 22 个、前端 24 个预测冲突均按功能组确认后手工解决；未使用整文件 `ours`/`theirs`。
- 前端采用上游 feature 架构并迁移全部 fork 行为，版本为 `0.0.0`，保留图表依赖。
- 本次只交付本地同步分支；没有更新 `main`、推送、创建 tag 或发布。
