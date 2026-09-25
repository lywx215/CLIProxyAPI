# DIAG-05 本地交付：待审核

## 身份与冻结基线

- 任务 ID：DIAG-05；项目：CLIProxyAPI。
- worktree：`C:/Users/lywx2/.codex/worktrees/15cf/CLIProxyAPI`。
- 分支：`codex/diag-05-cpa-diagnostics`。
- 初始 HEAD 精确为 `50d335a18a2495bd2fbbb30ac47d09476a8a14b6`，工作区干净，随后创建上述分支；未复用或覆盖其他 worktree。
- 最终 HEAD：由提交后的任务回报提供完整 SHA；本文件纳入同一交付提交，避免自引用摘要。
- 契约来源 DIAG-00：`bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`。
- `contracts/diagnostics/v1`：`ai-proxy-diagnostics/1`、制品 `1.0.0-rc.1`。
- `SHA256SUMS` 原始字节 SHA-256：`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。
- 工作区、Git HEAD、Git index 三份原始字节摘要相同；冻结目录相对准确基线无 diff；没有修改 schema、向量或契约版本。

当前状态仅为“待审核”。没有调用 Claude、推送、合并、部署、生产模型调用、真实凭证读取、数据库/Volume/远程配置操作，没有子任务或跨仓库写入。没有使用 gcli02 未审实现。Aito03 仅作为派发中的计时约定参考，没有读取或依赖它的代码。

## 实现与主体接点

完整模块说明和矩阵见 [internal/diagnostics/README.md](../../../internal/diagnostics/README.md)。

| 文件/边界 | 观察内容及理由 |
| --- | --- |
| `internal/diagnostics/semantic.go`、`protocol.go` | 四类封闭 DEBUG 投影；上下文门控、bounded JSON、数值 usage/结构/输出、有限枚举、终止/EOF/解析证据；不保留正文、签名、名称、原始异常或正文 hash |
| `internal/diagnostics/attempt.go` | conductor 派发所有权；每次实际 Gemini/Antigravity executor invocation 分配本地 attemptId/attemptNo，scope=`conductor_executor`；不从 HTTP 数量推断 retry |
| `records.go`、`transport.go` | server 基础/DEBUG 共享 logSeq、sealed 终局、已知丢失和截断计数、真实 HTTP call 继承只读 attempt；各 call 自身只有基础终局，logSeq=1 |
| `internal/util/util.go` | 现有 SetLogLevel 关闭 DEBUG 前推进 epoch，捕获事件间发生的关闭再开启；不中途加入采集或补采 |
| `internal/logging/diagnostics.go` | DEBUG/INFO 对应原 logrus 门控；生产 sink 无写入确认，因此 droppedForSpan 零损失未知时为 null，sinkDroppedTotal 仍 null |
| `gemini_executor.go` | 请求规范化后、HTTP 已有发送/读取点、usage filter 前的 SSE payload、实际转换产物/原 channel 成功发送后只读观察 |
| `antigravity_executor_execute.go`、`antigravity_executor_stream.go` | 普通非流/流、既有 stream-to-nonstream collector；只看已读帧和实际转换结果，不另建 collector，不预读 |
| `sdk/cliproxy/auth/conductor_execution.go`、`conductor_stream.go`、`conductor_home_execution.go` | 仅给实际 Execute/ExecuteStream 参数添加独立 context 身份；保持原 usage tracker、MarkUpstreamAttempt、重试/选择/refresh/生命周期及 Home RPC 不变；无需伴随 CLIProxyAPIHome 协议修改 |
| `sdk/api/handlers/gemini/gemini_handlers.go` | Gemini 两个生成 handler 的 request throttle 观察生命周期；不扩其他 provider handler |
| `speed_throttle.go`、`speed_throttle_diagnostics.go` | 既有估算函数返回同一次计算选中的 provenance；已有正等待分支记录实际等待，不重算限速、不增 timer、不变等待公式/返回值；参数取既有随机选择结果 |

保持既有 proxy、Transport concrete assertions、usage wrapper 顺序、上下文取消父节点、Go 出站 clone/ireq redirect ownership。没有修改 translator 或其他提供方专属逻辑。全部 `.go` 经 `gofmt -w .`；Git renormalize 仅刷新 gofmt 造成的 LF/CRLF 工作区标记，实质 diff 均属于上述接点及测试。

## 语义及公式证据

| 证据位置 | 可复核口径 |
| --- | --- |
| `internal/diagnostics/protocol.go:89`，`usageSnapshot` | raw 数字白名单；missing=false/null、实际 0=true/0；重复累计帧替换；Gemini candidate/reasoning 分列，只有两者明确存在时才提供观察性 outputTotal=candidate+reasoning；不反哺业务 accounting |
| `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go:114`、`:321`；Gemini→Claude `gemini_claude_response.go:272`、`:312` | 未修改的转换器已将 candidates+thoughts 写入 completion/output_tokens；公共 deliveredUsage 直接读取真实产物，不再次加 reasoning；最终协议专项涵盖 chat/responses/claude 的流和非流 |
| `protocol.go` 的 `responseObservation.observe/observePayload/Finish` | 每个观察到的 Gemini candidate 都需有效终止；error/read/cancel 覆盖 success；普通文本空白字符不算有效输出，工具需合法形状，媒体有具体载荷；不能用仅有 HTTP 200/EOF 或 thought 字节声明成功 |
| `protocol.go:248`，`ObserveNormalized` | 输入与发送结构各自的 message/empty/tool counts 及最后 4 条 role/part kind/字节数；原有空 user 和最终新增空 user 可区分。冻结 operation 枚举没有 insert_empty，因此未分类 payload 变化仅记 other/other，不冒充删除原因 |
| `protocol.go` 的 `NewExchange/Upstream`；`attempt.go:18` | firstEffectiveOutputMs 从本 server start 取 monotonic 偏移，每个 exchange 重置；有 conductor owner 时 totalMs 从本次派发开始；firstByte/commit/downstream 时间未可靠观测为 null；不借用前次 attempt 或旧 wall/browser 统计 |
| `sdk/api/handlers/speed_throttle.go` | 非流 target=max(selectedTTFT, tokenCount/targetRate)，remaining=target-upstreamElapsed；流式沿用已有每块文本估算和累计分子。原业务最低 1 token clamp 不变 |
| `speed_throttle_diagnostics.go:58` | plannedWaitMs 仅累加正 residual，actualWaitMs 仅记录已有等待分支，取消记已发生等待；tokenCount 是限速使用/准备等待的分子，不是客户端已接收 token |
| `speed_throttle.go` 的 `estimateNonStreamingTokensWithSource/geminiUsageOutputTokenTotalWithSource` | 同一次业务计算带出 provider_output/provider_candidate/estimated；缺失/零 usage 触发原有估算行为时明确记 estimated；只取 candidate 的路径不伪装 reasoning-aware output |

既有限速配置没有版本计数，因此 `configRevision=unversioned`，不伪造全局 revision；rate/选定 TTFT 是实际生效快照。模型/credentialRef 均 null，不将任意合法形状的模型名、账号 ID、Key 或 signature 当公共 alias。样例的 87=7+80，没有特殊异常规则，也不解释历史平台计费。

JSON 观察上限 4 MiB、深度 64；SSE 只剥一层 data，嵌套 data 前缀当 malformed，不能递归处理任意深度。超限/不完整投影不改变业务解析；sink panic/返回错误被隔离。默认 INFO 不构造结构/usage/限速 detail。DEBUG 在 server 开始时 opt-in，中途开启不补采，关闭再开启永久 interrupted。生产 accessCapture=unknown，未观察到 sink 丢失不等于零。

## 验收与覆盖矩阵

| 测试 | 实际边界与结果范围 |
| --- | --- |
| `TestDIAG05RealSemanticBoundaries` | 40 个真实本地 HTTP 上游子用例，Gemini 普通/流、Antigravity 普通/流/内部收集；多轮、空 user、usage=7+80、missing、zero、工具/媒体、error frame、HTTP503、malformed、缺失终止；每个用例观察前后对比 response bytes/error、发送次数、业务 contents，保留 usage marker |
| `TestDIAG05StreamBackpressureCancellationAndTailUsage` | 实际 Gemini ExecuteStream + 假 Transport，deterministic synctest；下游未接收第一块时 reader 只读一次；末尾 usage/read-error/cancel、清理 error 下发送顺序与 payload 等价 |
| `TestDIAG05FinalProtocolUsage` | 实际 Gemini converter，OpenAI chat/responses/Claude × 流/非流，交付输出总量87且 reasoningIncluded=true，不双计 |
| `TestDIAG05ConductorOwnsAttemptIdentity` | 实际 Manager 两凭证失败派发，流/非流各执行2次且2个独立 attempt 身份；不改变失败/重试次数 |
| `TestSemanticLifecycle/TruncationAndSealing` | DEBUG关闭/中途打开/关闭重开、sink error/panic/无确认、共用序号/终局、oversize 同序号 stub与truncatedEvents |
| `TestSemanticBoundedParsingAndCumulativeUsage/MalformedSSEIsNotRecursive/TimingUsesServerOriginAndNewExchange` | missing/zero、重复累计、reasoning已含、bounded/深嵌套/恶意前缀、可控时间server起点与独立attempt；不使用墙钟 Sleep 排序 |
| `TestNonStreamingHandlersThrottleReportedOutput` | 真实 Gemini Gin handler，enabled/disabled/cancelled；既有stub executor返回2080；目标100t/s、TTFT3500ms、计划20800ms，实际20800/0，响应状态/body不变 |
| `TestDIAG05ThrottleStreamWaits/ThrottleTokenProvenance` | synctest 检查真实 throttler 首块/后续累计/末尾usage不再等待/取消，4+2估算token对应400+200ms；provider candidate/output与estimated区分 |

详细覆盖：Gemini/Antigravity 的 Gemini 格式结构和输出完整计数；Gemini-family 转 OpenAI chat 支持文本/usage，流式工具碎片完整性未知；转 Responses/Claude 交付 usage 已观察，详细 output counts/result 保持 unknown。Native Interactions、countTokens、compaction、browser/WS、自建 plugin client、其他 executor 语义、其他协议 handler 限速均不宣称覆盖。new-api 不改。直接 executor 调用无 conductor owner 时 attempt 字段为 null。

## 命令与结果

| 命令/阶段 | 退出结果 |
| --- | --- |
| `git rev-parse --show-toplevel`、`git rev-parse HEAD`、`git status --porcelain=v1`；`git switch -c codex/diag-05-cpa-diagnostics` | 0；准确基线、clean、新分支 |
| `go version`；`go env CGO_ENABLED` | 0；go1.26.0 windows/amd64，CGO_ENABLED=0 |
| `gofmt -w .`（各实质修改后） | 0；无无关语义 diff。一次使用 PowerShell 的 `gofmt -w .../speed_throttle*go` 未展开通配符而报 CreateFile；之后使用全仓命令修正，不作为通过记录 |
| `go test -timeout 120s ./internal/diagnostics ./internal/runtime/executor -run 'TestDiagnosticRealExecutorEquivalence|TestSyntheticRecordArtifact'` | 0 |
| `go test -timeout 120s ./internal/diagnostics ./sdk/api/handlers ./sdk/api/handlers/gemini ./internal/logging` | 0 |
| `go test -timeout 120s ./internal/runtime/executor -run TestDIAG05RealSemanticBoundaries` | 0 |
| `go test -timeout 120s ./internal/diagnostics ./sdk/api/handlers -run 'TestSemantic|TestDIAG05|TestNonStreamingHandlersThrottleReportedOutput'` | 0 |
| 同选择器的四包专项（diagnostics/executor/handlers/auth），加入 backpressure 测试时 | 0 |
| 同四包专项，加入 conductor/multi-candidate 修订时 | 1；executor 启动被 Application Control 拦截，其余包通过；准确错误如下 |
| `go test -timeout 120s ./internal/runtime/executor -run TestDIAG05FinalProtocolUsage`（新增最终协议测试后的正常构建） | 0；没有改名或策略绕过 |
| 第一轮 `go test -timeout 10m ./...`，生成样例 | 1；仅 middleware/pluginabi 启动被应用控制拦截；原始完整输出见 `DIAG-05-full-go-test.txt` |
| 解析递归硬化后的八包完整专项：`go test -timeout 120s ./internal/diagnostics ./internal/runtime/executor ./sdk/api/handlers ./sdk/api/handlers/gemini ./sdk/cliproxy/auth ./internal/logging ./internal/util ./internal/runtime/executor/helps` | 0；原始输出 `DIAG-05-targeted-go-test.txt` |
| 最终来源标注/解析硬化源码的 `go test -timeout 10m ./...` | 退出0，最终完整回归通过（包括之前被拦截的middleware和pluginabi）；原始输出 `DIAG-05-full-go-test-final.txt` |
| `go build -o .diag05-server.exe ./cmd/server`，首次和最终源码 | 均0；没有运行生产 server，临时编译产物删除 |
| `python -m pip install --disable-pip-version-check --use-deprecated=legacy-certs --target .diag05-validation/python -r contracts/diagnostics/v1/requirements.txt` | 0；Python3.12.10，依赖仅worktree临时目录，使用正常CA验证，没有关闭TLS校验 |
| `PYTHONPATH=.diag05-validation/python python contracts/diagnostics/v1/validate.py` | 0；3 schemas、53 fixtures、9 example lines、242 vectors；冻结digest相同 |
| 同 PYTHONPATH 的 `python internal/diagnostics/testdata/validate_records.py coordination/diagnostics/20260924/DIAG-05-native-synthetic.jsonl coordination/diagnostics/20260924/DIAG-05-throttle-synthetic.jsonl` | 0；244行真实Go生成记录，schema/跨字段语义/4096字节均通过 |
| Python二进制方式读取工作区、`git show HEAD:...`、`git show :...` 的manifest；Git frozen-directory diff | 0；三份digest相同、目录无diff |

准确历史拦截错误都是 `An Application Control policy has blocked this file.`：

- 专项：`C:/Users/lywx2/AppData/Local/Temp/go-build2068503766/b290/executor.test.exe`。
- 第一轮全量：`C:/Users/lywx2/AppData/Local/Temp/go-build1978201396/b882/middleware.test.exe`。
- 第一轮全量：`C:/Users/lywx2/AppData/Local/Temp/go-build1978201396/b1093/pluginabi.test.exe`。

未修改安全策略、改名测试程序或单独循环重试被拦包；后续验证由明确新增测试/真实解析修订和来源标注修订触发。DIAG-04 parent 的最终完整测试/build=0归档及更早历史拦截仍保留于其交付材料，不用其通过替代本次验收。没有安装 WSL、Docker 或工具链；沿用派发确认的 WSL未装/Docker daemon不可用限制，CGO=0未作race instrumentation。

## 脱敏产物与 DIAG-07 隔离入口

- `DIAG-05-native-synthetic.jsonl`：真实40组测试产出235行，process40、normalized40、call40、attempt40、converted35、server40；最大1975字节（含前缀/LF）。5个HTTP错误没有转换产物，因此不构造转换事件。
- `DIAG-05-throttle-synthetic.jsonl`：真实Gemini handler限速测试产出9行（开/关/取消×process/throttle/server）；最大1126字节（含前缀/LF），configRevision为unversioned。
- 上述不是生产采样，没有绝对路径、真实凭证或业务正文。样例随机trace/boot/time使每次生成字节摘要不同；契约digest不变。

DIAG-07 可以使用已经验证的隔离入口，无需启动读取用户配置/凭证目录的常驻 server：

```powershell
$env:DIAG05_TEST_RECORDS = Join-Path (Get-Location) 'coordination/diagnostics/20260924/DIAG-05-native-synthetic.jsonl'
$env:DIAG05_THROTTLE_RECORDS = Join-Path (Get-Location) 'coordination/diagnostics/20260924/DIAG-05-throttle-synthetic.jsonl'
go test -timeout 120s ./internal/runtime/executor -run 'TestDIAG05RealSemanticBoundaries|TestDIAG05FinalProtocolUsage|TestDIAG05StreamBackpressureCancellationAndTailUsage'
go test -timeout 120s ./sdk/api/handlers -run 'TestNonStreamingHandlersThrottleReportedOutput|TestDIAG05Throttle'
go test -timeout 120s ./sdk/cliproxy/auth -run TestDIAG05ConductorOwnsAttemptIdentity
```

该入口执行生产 Gemini/Antigravity executor/translator、真实 Gin handler 与 Manager 边界，所有网络目标由 httptest 绑定本机临时端口，Transport用例完全内存；auth只有源码内合成值，不加载 `.env` 或用户auth-dir。它不是已验证的真实进程跨服务拓扑：多worker stdout、受控gcli/Aito双边联调、真实常驻server配置启动由 DIAG-07 在冻结依赖齐备后负责。本任务不宣称已完成这些部署级验收。

## 剩余限制与审核交接

- 表中未覆盖协议/细节显式未知，不将进程capability清单解释为每条路由都完整覆盖。
- access/sink外部丢失未知；生产不能由日志alone声称full、远端收到或实际计费。
- 外部嵌入者若绕过 `util.SetLogLevel` 直接 off/on logrus，必须在disable时通知diagnostics；未通知且发生于两个观察点之间的开关无法可靠察觉。应用内已知配置路径均走该utility。
- request transformation仅使用冻结枚举能表达的other；不私增insert_empty字段，不借结构字节数证明内容相同。
- response converted表示本地转换/交给既有channel的产物；没有更改下游发送/背压/取消规则或声明调用方收讫。
- 最终全仓结果以退出0，最终完整回归通过（包括之前被拦截的middleware和pluginabi）及原始输出为准；环境拦截不能记为测试通过。
- 等待协调窗口代码检查及Claude Opus5.5准确最终HEAD审核；任何修订在本worktree继续，本地新提交后重新报告，不沿用旧审核。

## 最终产物字节摘要

- `DIAG-05-native-synthetic.jsonl`：SHA-256 `93cee99438806b60ff3b3bded30de0b4132dcc79197b6479fe8b66ec51e6b0d8`；235行，最大1975字节。
- `DIAG-05-throttle-synthetic.jsonl`：SHA-256 `bd559fbffd8e6eb4f59b5691da2983e8a69efc6f404e4af7be8d55fbb108a8d9`；9行，最大1126字节。

临时清理：自动审批拒绝删除本worktree的 `.diag05-validation` 目录，仅返回 `blocked by policy`，没有更具体原因。目录保留、排除Git；没有改用其他删除方式或绕过审批。


## 协调方 R1 退回修订

初始交付准确 HEAD 为 `a8298639d0e1cc22b7372a57d66baceead5cb459`，未获 Claude 审核，也未推送。
协调窗口发现 server 提前封存与辅助 call attempt 继承的边界缺口，本轮已按 Go 实际生命周期核验并修订。
此前正文的“最终”测试记录属于初始交付；当前修订与完整验证以
[DIAG-05-R1-disposition.zh-CN.md](DIAG-05-R1-disposition.zh-CN.md) 为准，新的准确提交 SHA 由任务交付消息给出。


## R2 independent-review follow-up

Claude Opus 5.5 reviewed exact R1 HEAD `bb410f92433ada96614caee23d5fb385510e43c5`
and requested changes (0 P1 / 2 P2). The R2 disposition supersedes the original
scope/coverage statements above: retryScope is now `conductor_gemini_family`, and
Antigravity compaction's recursive summary Execute is observed under the outer
attempt identity while its capsule wrapper is not separately observed.
See [R2 disposition](DIAG-05-R2-disposition.zh-CN.md) and the complete source
companion for the revisions and verification. A fresh independent review is
still required; no push or deployment is included.
