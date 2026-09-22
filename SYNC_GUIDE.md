# Fork Synchronization Guide

本文档定义后端 `CLIProxyAPI` 与自定义管理前端 `Cli-Proxy-API-Management-Center` 的上游同步规则。

日常前后端功能联动开发可参考 [前后端联动开发指南](INTEGRATION_GUIDE_CN.md)。该指南为单人开发提供建议性清单，不替代本文的 fork 同步安全规则。

> Last updated: 2026-09-22
> Backend upstream: `555662940411a07460e9d24d14477a5f50dffdb5`
> Management frontend upstream: `bbac79d2222a0f345458203a5ab92d859f30ff30`

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
- `speed-throttle`：首个完整 SSE data frame 的首包延迟，以及后续 token 节流。最大并发/队列目前只有前端读写控件，基线后端未实现，不能宣称已生效；详见第 10 节。
- 模型名中的 `-Nm` 输入 token 限制。
- Gemini、Gemini CLI、Antigravity 等响应中的 `ModelVersion`/alias 回写。
- 模型目录显式 `display-name` 优先；未配置时展示客户端可见 alias，而不是上游内部 name。
- OpenAI、Gemini、Claude、Responses/WebSocket 等错误路径返回固定安全消息，不泄露上游响应、URL、凭证或内部细节。
- 保留 fork 的日志降噪和结构化日志行为。

### 4.2 Antigravity、Usage 与认证

- `antigravity-credits-force`、Credits 请求选择、`CreditsUsed` 记录和全部 credit 类型统计。
- Antigravity 使用上游单请求、quota signal 和 retry-round 语义；不得重新引入已移除的跨 endpoint fallback。
- Usage 与 Antigravity Stats 管理端点、导入/导出/重置和统计日志。
- Gemini CLI OAuth、配额与 usage 解析。
- `enable-gemini-cli-endpoint` 独立开关，以及 Gemini/Gemini CLI provider alias 归一化；OAuth 登录路由需要相应插件提供。
- Vertex ADC `authorized_user` 导入、刷新与 executor 支持，同时保留 service account。
- Claude translator 不转发 `temperature`，采用上游兼容性修复。
- Claude `fingerprint-profile=claude-code-cli`、request-scoped errors 和稳定的配置/默认设备指纹；显式配置优先，未配置时不得泄露宿主机 OS/Arch。
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
- Usage queue 保留默认 `count=100`、参数校验与 RawMessage 数组契约；上游 queue 的新增字段不等同于本地 Usage snapshot 的展示字段。
- API-key Rate Limit、Speed Throttle、CreditsForce 的类型、默认值、dirty tracking、YAML 解析/写回、搜索索引和编辑 UI。
- Gemini CLI OAuth `project_id`、配额 provider、分桶聚合和重置时间。
- Antigravity tier 与全部 credit 类型的汇总显示。
- Vertex ADC `authorized_user` 凭证导入。
- `/usage`、`/antigravity-stats` 路由及 Usage store 导出。
- Windows 构建兼容逻辑、前后端独立 build time。
- 四种语言的所有本地独有键；同名等价键采用上游最新措辞，语义变化必须再次确认。
- Claude provider 的 `fingerprint-profile=claude-code-cli` 配置、展示和 YAML 往返，以及上游最新 Codex 配额 User-Agent。
- Codex `identity-confuse`、Gemini CLI endpoint 开关的 UI、类型、搜索和 YAML 往返；编辑时保留上游及未知嵌套字段。
- Code0 的四协议聚合、创建/编辑/删除、快速接入、品牌图标及注册入口；随上游 sponsor/provider 结构迁移，不保留孤立旧模块。

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

前端构建成功后，将单文件产物复制到后端并核验哈希。不要假设两个仓库具有固定盘符或相邻目录名称；先把实际仓库路径赋给变量：

```powershell
$FrontendRepoPath = '<frontend-repository-path>'
$BackendRepoPath = '<backend-repository-path>'
$FrontendAssetPath = Join-Path $FrontendRepoPath 'dist/index.html'
$BackendAssetPath = Join-Path $BackendRepoPath 'static/management.html'

Copy-Item -LiteralPath $FrontendAssetPath -Destination $BackendAssetPath -Force
Get-FileHash -Algorithm SHA256 $FrontendAssetPath
Get-FileHash -Algorithm SHA256 $BackendAssetPath
```

macOS/Linux 可使用：

```bash
FRONTEND_REPO_PATH='<frontend-repository-path>'
BACKEND_REPO_PATH='<backend-repository-path>'

cp "$FRONTEND_REPO_PATH/dist/index.html" "$BACKEND_REPO_PATH/static/management.html"
sha256sum "$FRONTEND_REPO_PATH/dist/index.html"
sha256sum "$BACKEND_REPO_PATH/static/management.html"
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

## 9. 2026-08-30 同步记录

- 后端从 fork `d00c5927` 合并固定 upstream `f0de1d00`（位于 `v7.2.145` 之后），使用本地分支 `codex/sync-upstream-20260830-f0de1d00`。
- 管理前端从 fork `869b9deb` 合并固定 upstream `d249ff00`（`v1.22.9`），使用本地分支 `codex/sync-upstream-v1.22.9`。
- 后端预演和真实合并均产生 5 个冲突：配置、Antigravity credits 测试、execute、stream 与 conductor cooldown；前端为 0 个冲突。
- 后端冲突按用户确认采用上游 quota signal、单请求/no-fallback 和 retry-round 架构，同时恢复 `CreditsForce`、`CreditsUsed`、Usage 统计与 ModelVersion 回写；conductor 不进行每请求 auth 持久化，但保留 cooldown 持久化。
- 前端采用上游 Claude fingerprint profile 和 Codex 配额 User-Agent，同时保留全部 fork 配置、Usage/Stats、Quota/OAuth、Vertex ADC、路由、图表和四语言行为。
- Windows 验证中，gitstore 的 3 个损坏仓库恢复子测试因 Windows 拒绝重命名临时 `.git` 目录而阻断；另有部分测试二进制被 Application Control 阻断。其余已运行测试和全部构建通过。
- 本次只交付本地同步分支；没有更新 `main`、推送、创建 tag 或发布。Linux 全量/race 测试与 Docker 构建仍是发布前阻断项。

## 10. 2026-09-08 同步记录

### 10.1 固定提交与本地范围

| 仓库 | fork 基线 | 固定 upstream 目标 | 同步分支 |
| --- | --- | --- | --- |
| 后端 | `92409807bd5a80436ecc729933ffc97560c0cbff` | `ba7e55836dee959e93ec6d41395865d9ec535086` | `codex/sync-upstream-20260908-ba7e5583` |
| 前端 | `290fb4c20ec7d8284698ac8b5123c469c71fd66f` | `cb917b3111196487549f7a43e3afce4801f5d0f0` | `codex/sync-upstream-20260908-cb917b3` |

- 通过附加目录与 Git remote 核对实际路径；两边 `origin/main` 均等于 fork 基线，无需更新 `main`。两边保留对应的 `codex/backup-main-<fork-short-sha>`。
- 后端上游新增 126 个提交（118 个非 merge），前端新增 23 个（15 个非 merge）。用户确认预演后，分别使用上述完整 SHA 执行 `git merge --no-ff --no-commit`，不追逐后续上游提交。
- 前端本地 merge commit：`e699ef52351577e2fc2d02f135e8df0f89be46cb`，两个父提交精确等于该行基线及固定目标。后端本次 merge commit 的最终 SHA 和双方祖先核验结果写入本地 `.codex/sync-upstream-progress.md`，避免在提交自身中嵌入循环引用。
- 本次只交付本地同步分支，不更新 `main`、不推送、不打 tag、不发布。环境阻断按用户要求单独记录，未标记为测试通过。

### 10.2 冲突决策与定制行为

- 后端真实合并与预演一致，冲突为 `server_reload.go`、`server_test.go`、`config_diff_test.go`、`handlers.go`、`conductor_cooldown.go` 共 5 个文件。合并双方独立 imports/测试/配置断言；采用上游运行期 `Generation`/`UpdatedAt` 和 scheduler 结构，保留安全错误响应、Stats 与禁止逐请求保存凭证的行为。
- 前端真实合并与预演一致，冲突为 `AGENTS.md`、`useVisualConfig.ts`、`visualConfig.ts`、ru/zh-CN/zh-TW 共 6 个文件。采用上游规则与 feature 结构，保留跨仓库指南、动态路径发现、Codex identity、Gemini CLI 开关及全部定制 YAML 字段，并加入上游 Antigravity sensitive-words。逐块合并，未整文件选择 ours/theirs。
- Home/上游错误保留内部状态码、cause、凭证失效与诊断信息，对客户端输出固定安全消息；移除通用 DirectResponse 绕过路径，保留明确的可信插件 `RequestTerminatedError` 响应契约。Codex alpha-search 的上游错误也统一脱敏；401 内部报告与可信 429 Retry-After 保留。
- 普通及 availability-neutral 成功/失败结果均更新运行期 generation/计数，不逐请求保存凭证；新增回归测试验证 Save 次数。独立 cooldown 持久化和显式凭证更新/刷新仍保留。
- Antigravity 采用上游 compaction、quota signal、minimum cooldown 与 disable-cooling 结构，保留强制 credits、CreditsUsed、usage 和客户端 alias。修复新增 compaction 流式/非流式响应遗漏 alias；新增四种组合回归验证单次 summary 请求、credits、alias 和 token 统计。
- 零 Retry-After 的旧测试更新为上游最小 cooldown 语义，仍验证两次调用只实际执行一次；disable-cooling 的独立测试保留。旧 Home 原文错误断言同步改为已确认的固定安全消息，而内部原始诊断断言保留。
- API-key 限流、已有 token 节流/首帧延迟、token cap、模型别名、COS/AWS SDK v2 与 OBJECTSTORE_PREFIX、Claude fingerprint、ADC/service account、Gemini CLI 配额、Usage/Stats、图表、路由和 Windows 单文件构建保留。配置示例补充实际支持的 credits-force、Gemini CLI endpoint 和 speed-throttle 字段。
- 前端 Credits 缺省值统一为后端零值及 YAML 缺字段解析的 false；模板显式 true 仍正常解析。YAML 回归同时修改定制和上游字段，验证 connection-pool、orphan-delegation、session-affinity-subagents 及未知字段不丢失。四语言保留全部上游键和 fork 独有键、清除重复 ru 键，keepalive 文案明确覆盖 SSE 与 WebSocket Ping。
- `go mod tidy`、`bun install` 和 frozen-lockfile 安装成功；依赖与锁文件不需额外变化，保留实际引用的 AWS SDK、chart.js 和 react-chartjs-2。Go 全量格式化仅留下两处很小的额外空白修正；前端清理上游引入的 EOF/Markdown 空白后重新完成 verify/build。

### 10.3 验证结果

环境：Git 2.55.0.windows.3、Go 1.26.0 windows/amd64、Bun 1.3.14。

| 检查 | 结果 |
| --- | --- |
| `gofmt -w .`、`go mod tidy` | 已完成；无额外依赖/锁文件差异 |
| API/auth 定向复测、Antigravity compaction 新回归 | 通过；旧安全错误/cooldown 断言已按确认行为修正 |
| `go test -count=1 -json ./...` | 命令退出 1：91 个包通过，33 个包无测试，1 个包因系统策略无法执行；8948 个测试/子测试通过，8 个跳过，0 个断言失败。不能标记全量通过 |
| `go build ./...`、Windows server 编译 | 通过 |
| Linux amd64/arm64 server 交叉编译（CGO_ENABLED=0） | 均通过；不等同于 Linux 运行测试 |
| `bun install`、`bun install --frozen-lockfile` | 通过 |
| 最终 `bun run verify` | 436 测试通过，0 失败，1419 断言；lint、TypeScript 编译和生产 build 均通过 |
| `bun run type-check` | 通过；最后仅空白清理后的 verify 也重新包含 TypeScript 编译 |
| 四语言 JSON/键/占位符检查 | 无重复键，上游键及 fork 独有键完整；各语言总键数差异为原有翻译覆盖差异 |
| 隔离配置 HTTP 检查 | 23 项通过：Management 鉴权、YAML 读写/非法输入、Usage 导入去重/导出、Stats 三种 view/重置、空 auth-files、queue 参数、OAuth 状态/回调校验、ADC 缺文件校验及静态页内容 |
| 工作区及暂存区 `git diff --check` | 通过；无未解决冲突 |

全量测试覆盖了配置、API-key 限流、Speed Throttle、watcher、COS、management、executor/helps、各协议 handlers、auth/service、插件 OAuth/Quota 与 ADC 回归。`internal/store` 本轮通过，不沿用上轮 Windows gitstore 失败结论。

`sdk/cliproxy/usage` 在首轮、定向复测和最终全量中均遭 Windows Application Control 拦截。最终原始错误：`fork/exec C:\Users\lywx2\AppData\Local\Temp\go-build3761561945\b1062\usage.test.exe: An Application Control policy has blocked this file.` 此包未执行，发布前必须在允许执行的环境补测。它与已通过的 HTTP Usage snapshot 检查不同。

8 个跳过测试：6 个签名原生语料/catalog 测试缺少外部样本；`TestClaudeCodeTLSClientHelloCapture` 缺少 `CPA_TLS_FP_PROXY`；`TestResolveGitHubToken/GITHUB_TOKEN_has_highest_priority` 因 Windows 环境变量大小写不敏感跳过。没有为通过测试而加载真实凭据或采集敏感 fixture。

隔离服务使用临时目录、空 auths、随机本地端口、合成管理/API key、显式配置和 `--local-model`，关闭插件及管理页自动更新，运行目录不读取真实 `.env`。用例结束后服务退出，auths 仍为空。OAuth 未知/过期 state 返回 404，检查脚本已修正早期错误的 400 预期。此验证覆盖参数/失败路径，不冒充真实 OAuth 登录或付费上游请求验证。

### 10.4 产物与发布前缺项

- 最后一次成功 `bun run verify` 构建后，将前端 `dist/index.html` 复制至后端 `static/management.html`。使用 Windows `Get-FileHash -Algorithm SHA256` 核验两者一致：`0D5042C9076114660DF4EB3CE9079AA4FEFF6BD9C21E1CA6C215F12B9FED93EA`。未手工编辑生成 HTML；隔离 HTTP 服务实际返回的页面也与该文件一致。
- 浏览器工具在启动内核阶段失败：`failed to write kernel assets: 系统找不到指定的路径。 (os error 3)`。尚未完成 Config、Usage/Stats、Quota、OAuth/ADC、provider 分组、侧栏及连接切换的真实浏览器交互/截图检查。
- Docker CLI 存在，但 `dockerDesktopLinuxEngine` 命名管道不存在；WSL/Linux 运行环境不可用。Linux 全量/race、Docker 构建仍待发布前补测。
- Gemini CLI 登录路由由插件提供；隔离测试关闭插件时返回 404 符合当前实现。带插件与真实测试账号的 OAuth、配额重置、ADC 刷新、credits/stream 上游联调未运行。
- 基线已有两项产品缺口留待独立功能任务：speed-throttle 的并发/队列后端实现，以及本地 Usage snapshot/UI 对上游 queue session/parent-session/stream、TTFT、cache-write 新维度的完整展示。没有在同步中擅自定义排队/拒绝/取消行为或扩展统计 schema。
- 完整预演与日志保留在两边忽略目录 `.codex/sync-20260908/`；最终状态/提交/下一步记录在后端 `.codex/sync-upstream-progress.md`。本地交付完成不表示发布前缺项已通过。

## 11. 2026-09-22 同步记录

### 11.1 固定目标与本地交付范围

| 仓库 | fork 基线 | 固定 upstream 目标 | 同步分支 |
| --- | --- | --- | --- |
| 后端 | `6cf4871ea1d99a8c92e33e1919b92f3a17f5742f` | `555662940411a07460e9d24d14477a5f50dffdb5` | `codex/sync-upstream-20260922-55566294` |
| 前端 | `e699ef52351577e2fc2d02f135e8df0f89be46cb` | `bbac79d2222a0f345458203a5ab92d859f30ff30` | `codex/sync-upstream-20260922-bbac79d2` |

- 实施前再次 `fetch origin --no-tags`；两边 origin/main 未变化，均等于基线。保留 `codex/backup-main-6cf4871e` 和 `codex/backup-main-e699ef52`。
- 后端上游新增 323 个提交（291 个非 merge）；前端新增 54 个（53 个非 merge）。实际使用完整固定 SHA 执行 `merge --no-ff --no-commit`，未追逐后续提交。
- 前端 merge commit：`fb802786583e88e047c47efa0f803307a8bda27c`。其中源码与 `dist/index.html` 均已验证并提交；两个父提交精确等于前端基线及固定目标。后端最终 SHA 记录在 `.codex/sync-upstream-progress.md`，避免循环引用。
- 本轮只交付两个本地同步分支；不更新 main、不推送、不打 tag、不发布旧版或新版，也不修改运行中的服务。

### 11.2 冲突决策与语义适配

- 8 个后端和 17 个前端冲突按已确认功能方案逐块合并，未整文件选择 ours/theirs。上游 Meta/Devin、discovery、插件 quota/priority、认证刷新/冷却、Codex 双工 steering 和 bootstrap 结构保留。
- 安全错误采用上游嵌套 error、序号、认证分类与 retryable，同时固定客户端消息并过滤额外诊断字段。新增双工非终止 error 和终止 error 原始 payload 路径也经过安全处理；保留 event_id、sequence_number、retryable 及恢复后继续生成能力。集成测试同时验证无诊断泄漏和纠正请求后成功生成。
- Claude 官方客户端允许同 major/minor 的新版 patch UA；显式配置仍优先，SDK/runtime 必须匹配测量基线，稳定化和默认平台不读取宿主 OS/Arch。上游新版 passthrough 与 fork 指纹回归均通过。
- CreditsUsed 在结果策略之后补取上下文；保留 Antigravity Stats、Usage、别名/token cap、Gemini CLI、Vertex ADC、API-key 限流、Speed Throttle、COS/AWS SDK v2 和 OBJECTSTORE_PREFIX。普通请求不保存整个 auth，独立 cooldown 持久化继续保留。
- 按实际 import 保留 AWS SDK、引入 zeroconf、移除未使用的 MinIO；`go mod tidy` 生成依赖校验文件。前端 chart.js/react-chartjs-2 和现有 Bun 锁文件保留，frozen 安装通过。
- Gemini CLI 接入新的配额缓存身份/失效机制及可取消 OAuth 流程，保留 project_id 与 callback provider 映射；保留 Meta/Devin OAuth/Quota、账户搜索、Vertex ADC、Credits 汇总和未知 YAML 节点。
- Code0 迁入当前 descriptor、adapter、sponsor definition、workbench mutation、表格及快速接入结构。新增四协议创建/编辑/删除测试，验证原配置选择、OpenAI sourceIndex 和 Claude fingerprint 不丢失；浏览器验证创建、保存前缀及删除测试资源。
- 新上游错误断言改为已确认的安全消息契约，内部错误诊断断言继续保留；Windows 日志 UI 源码测试先归一化 CRLF。`gofmt -w .` 额外调整三处上游多返回值缩进，无无关功能开发。

### 11.3 验证结果

环境：Go 1.26.0 windows/amd64、Bun 1.3.14、Node 24；测试未读取生产配置或真实认证资料。

| 检查 | 结果 |
| --- | --- |
| `gofmt -w .`、`go mod tidy`、暂存差异检查 | 通过，无未解决冲突 |
| 配置、API/限流、watcher、COS/store、Usage、auth/service、executor/helps、handlers 定向测试 | 通过；`backend-targeted-final.log` 和 `executor-retest.log` 保存结果 |
| `go test -count=1 -json ./...` | 退出 0；97 个包通过，32 个包无测试，11303 个测试/子测试通过，9 个条件跳过，0 失败 |
| `go build ./...`、Windows server 编译 | 通过 |
| Linux amd64/arm64 server 交叉编译 | 均通过，不代表 Linux 运行测试 |
| `bun install --frozen-lockfile`、type-check | 通过 |
| 最终 `bun run verify` | 714 项测试、3826 个断言通过；lint、TypeScript 和生产构建通过 |
| Code0 快速接入分组最后调整后的测试与 build | 通过；`VERSION=sync-20260922-bbac79d2`，不借用旧发布版本标识 |
| 四语言 JSON/占位符/键审计 | 无重复键，无丢失上游键或各语言 fork 独有键，无本轮新增缺译键；原有语言覆盖差异和复数形式差异保留，并非完全键集合相等 |
| 最终装配后的隔离 HTTP 冒烟 | 29 项通过：鉴权、YAML 往返/非法输入、Meta 配置、Quota providers/fetch/reset 校验、auth refresh 校验、API-key usage、Usage 导入去重/导出与 credits、Stats 三视图/重置、queue、OAuth 状态/回调、Vertex 缺文件、Gemini CLI 无插件路径、HTML 哈希 |
| 浏览器联合检查 | 登录、Code0 创建/编辑/删除、Meta/Devin/Gemini CLI OAuth 和 Vertex ADC 入口、Quota 空状态、Usage 合成数据及 Stats/配置页面已检查；无真实上游请求 |

首轮 Application Control 曾拦截部分测试程序；后续正常复测及最终全量全部执行成功，没有关闭或绕过系统策略。早期实际断言失败均经适配后复测通过，不沿用旧轮次的测试结论。

9 个条件跳过：Devin live test 未启用；6 个外部签名语料/catalog 测试缺样本；TLS capture 缺 `CPA_TLS_FP_PROXY`；1 个 GitHub token 优先级测试因 Windows 环境变量大小写规则跳过。

### 11.4 产物和发布前缺项

- 前端提交内 `dist/index.html` 与后端 `static/management.html` 的 SHA-256 相同：`45728436FC6AE656362FFE8AF5071452D069F09A8F908F081D27E0A126B83594`。未手工编辑 HTML；最终隔离服务返回内容的哈希也相同。之后未再构建。
- race 命令实际执行但被环境阻断：`go: -race requires cgo`，本机 CGO_ENABLED=0 且缺少 gcc；Linux 运行环境/WSL 不可用，Linux 全量/race 未运行。
- `docker build -t cliproxyapi-sync-local:20260922 .` 在连接 Docker 引擎时失败：`dockerDesktopLinuxEngine` 管道不存在；未构建或推送镜像。这是环境阻断，不能记为通过。
- 发布前仍需在 Linux/可用 Docker 环境完成全量、race、镜像构建，以及受控测试账号的真实 OAuth/ADC 刷新、插件配额和 credits/stream 联调。当前浏览器和 HTTP 检查使用临时配置、空 auths、随机回环端口、合成 key，关闭插件和页面自动更新，并使用 `--local-model`。
- 第 10.4 节记录的 Speed Throttle 并发/队列和 Usage queue 新维度展示缺口仍保留给独立功能任务；本轮不宣称已实现。四语言原有缺译仍使用既有回退机制；Vite 的 native config loader 未来兼容提示不影响本次构建。
- 两边 `.codex/sync-20260922/` 保存预演、验证和冒烟证据，后端 `.codex/sync-upstream-progress.md` 保存最终提交及恢复信息。本地同步完成不等于发布前检查全部完成。
