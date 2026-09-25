# DIAG-04 本地交付：待审核

- 项目：CLIProxyAPI。
- worktree：`C:\Users\lywx2\.codex\worktrees\a88f\CLIProxyAPI`。
- 分支：`codex/diag-04-cpa-tracing`。
- 锁定基线／契约来源：`bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`。
- 最终 HEAD：由本地提交后的任务回报提供完整 SHA；本文件属于同一提交，避免写入自引用摘要。
- 契约：`contracts/diagnostics/v1`，`ai-proxy-diagnostics/1`，`1.0.0-rc.1`。
- SHA256SUMS 原始字节 SHA-256：`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。
- 基线检查：新隔离 worktree，初始 HEAD 精确一致、工作区干净；随后创建指定分支。未覆盖既有工作。
- 冻结目录：没有修改；工作区逐文件清单验证通过，工作区／暂存区清单字节摘要一致，暂存区冻结目录相对批准基线没有差异。

## 实现与主体流程接点

完整说明、逐项入口／执行器／client 覆盖矩阵、真实头来源清单位于
[internal/diagnostics/README.md](../../../internal/diagnostics/README.md)。

新增独立模块 `internal/diagnostics`：

| 文件 | 职责 |
| --- | --- |
| `headers.go` | 框架多值头解析、W3C Level 1＋冻结本地限制、caller/peer ID 校验、已确认自动来源过滤、响应替换 |
| `peers.go` | 严格 JSON、重复键／重叠拒绝、精确 origin/path、IPv6 地址等价但不合并 IPv4 authority |
| `records.go` | 本地资源／worker 身份、进程／server 基础记录、上下文、封闭终局、4 KiB 上限、来源作用域 |
| `transport.go` | 最终 client 副本装饰、逐跳 call、具体请求副本拥有权、正文 EOF/Close/error/cancel 单次终结、能力保留 |
| `gin.go` | 正常响应提交点与流式／错误观察、原有请求 ID 复用、裸升级及隐式提交未知状态 |
| `coverage.go` | 按共享契约判定完整性证据，不把未知采集或终局存根升级为 full |
| `contract_test.go`、`runtime_test.go` | 188 项适用共享向量及实际 Go／Gin／本地 HTTP 边界测试 |
| `testdata/validate_records.py` | 仅离线校验 Go 生成的脱敏样例，不是生产实现 |
| `README.md` | 配置、生命周期、完整性、覆盖矩阵、头来源与限制 |

既有代码接点共 11 个文件，主体代码合计增加 40 行、删除 11 行：

| 文件 | 修改及原因 |
| --- | --- |
| `internal/api/server.go` | 加一个 Gin 中间件；统一建立上下文，不逐路由改造 |
| `internal/api/server_middleware.go` | 在原有 CORS 暴露列表添加两个诊断响应头，不变更允许源 |
| `internal/api/server_reload.go` | 现有配置重载时原子替换 peer 快照 |
| `internal/logging/global_logger.go` | 私有类型识别，使 `@diag` 通过同一 writer 且无旧文本前缀 |
| `internal/runtime/executor/helps/proxy_helpers.go` | 最终出口装饰；为 Antigravity 保留尚待配置的 base builder；Devin 所有返回分支在类型断言后装饰 |
| `internal/runtime/executor/helps/utls_client.go` | 完成受保护主机／fallback transport 后装饰 |
| `internal/runtime/executor/helps/usage_helpers.go` | 既有 usage owner 只标注 callKind=model，保持 usage 外层及 MarkUpstreamAttempt 原调用一次 |
| `internal/runtime/executor/antigravity_executor.go` | HTTP/1.1、代理／缓存配置完成后统一装饰；无新增工具函数堆放在 executor 根目录 |
| `internal/util/header_helpers.go` | 只过滤实际动态 `$Header` 入站复制；保留字面配置业务追踪头 |
| `sdk/api/handlers/gemini/gemini-cli_handlers.go` | 已证实整集合入站复制处过滤；现有 SetProxy 完成后装饰；仅携带诊断上下文，不变更取消父节点 |
| `sdk/api/handlers/handlers.go` | 在既有 cancellation parent 选择之后携带诊断身份，不改变业务 context／取消策略 |

另新增日志适配器 `internal/logging/diagnostics.go`，及以下边界专项测试：

- `internal/logging/diagnostics_test.go`
- `internal/runtime/executor/helps/diagnostics_test.go`
- `internal/runtime/executor/antigravity_diagnostics_test.go`
- `internal/util/header_diagnostics_test.go`
- `sdk/api/handlers/handlers_diagnostics_test.go`

没有重写路由、模型／凭证选择、重试、协议转换、usage 计费、限速或流式发送。
没有更改 Go 版本、Go 依赖、旧 ID／seq／X-Request-Id／X-Trace-Id／X-CPA-TRACE-ID 含义。

## 关键边界与实际覆盖

- 普通 HTTP/SSE：OpenAI-compatible、Gemini API/CLI/Vertex、Kimi、Meta、xAI、Claude、Codex、Antigravity、Devin 的共享最终 client 已覆盖。
- 所有通用／Devin 返回分支（默认、context Transport、自定义 RT、auth/config/request proxy、无效代理回退）以及 Antigravity 默认／代理／回退／context／自定义／typed-nil 均以本地假上游验证。
- Go `ireq` 的显式业务 traceparent → 第一跳 peer 注入 → 同源路径外／跨 origin 非 peer：保留原始业务 trace，不共享拥有权；peer→peer 新建 call。未改变 redirect policy。
- IPv6 展开／压缩／mapped 合法写法等价；`http://[::ffff:127.0.0.1]` 不匹配 `http://127.0.0.1`。不修改共享向量，新增本项目边界测试。
- 只在已确认自动来源过滤；显式管理 API、插件 API、NewHttpRequest 可选 headers、字面 provider attrs 不被笼统认定为泄漏来源。
- 响应头不预读正文；EOF、Close、read/transport error、cancel 只结算一次。保留 Reader/Writer/WriterTo、Gin streaming/hijack 与可用的 CloseIdleConnections。
- 64 并发 calls、两个真实本地接收实例、重复短 requestId、新 boot、独立根、正文等价、usage tracker/TTFT 均有测试。
- 基础记录沿用普通 INFO access-log 门控，独立于 DEBUG；仓库 `request-log` 是旧正文日志开关，不借它开启或关闭基础日志。
- WS/raw upgrade、AIStudio relay/browser 派发、插件自建 WireProfile client、management APICall、脱离入站 context 的后台／独立 SDK client 不声称出站全覆盖。历史文档中的 AMP 目录在本基线不存在。

## 验证记录

所有模型路径专项验证均用本地假上游；无生产模型、真实凭证库或远程配置调用。

| 命令 | 结果 |
| --- | --- |
| `go version` | `go1.26.0 windows/amd64`，未调整工具链 |
| `gofmt -w .` | 0；随后 Git 仅保留本任务实质 diff，其他 Go 文件没有内容差异 |
| 初次 `go test ./internal/diagnostics ./internal/logging ./internal/util ./internal/runtime/executor/helps` | 1；logging 测试启动被 Windows 应用控制拦截，其他已运行包通过；不将该次 logging 标为通过 |
| `go test -timeout 120s ./internal/diagnostics` | 0；共享向量和运行时测试通过 |
| `go test -timeout 120s ./internal/runtime/executor/helps -run TestDiagnosticsFinalClientBranchesAndUsage` | 0 |
| `go test -timeout 120s ./internal/runtime/executor -run TestAntigravityDiagnosticsFinalBranches` | 0 |
| `go test -timeout 120s ./internal/diagnostics ./sdk/api/handlers ./internal/logging ./internal/util ./internal/runtime/executor/helps ./internal/runtime/executor` | 0；新增实际测试代码后的正常构建运行，包括 logging，通过 |
| 第一轮 `go test -timeout 10m ./...` | 1；仅 `sdk/cliproxy/usage` 启动被应用控制拦截，其余包通过 |
| 后续正常源码修订后的 `go test -timeout 10m ./...` | 0；包括 `sdk/cliproxy/usage` 在内完整通过，没有改名或策略绕过 |
| 升级观察修订后 `go test -timeout 120s ./internal/diagnostics ./internal/api ./internal/logging ./internal/runtime/executor ./internal/runtime/executor/helps ./sdk/api/handlers ./sdk/api/handlers/gemini ./internal/util` | 0 |
| 最后一次 `go test -timeout 10m ./...` | 0；最终源代码完整通过，见 DIAG-04-validation.txt |
| `go build -o .diag04-server.exe ./cmd/server` | 0；最后源代码修订后再次编译通过，临时产物删除 |
| `DIAG_TEST_RECORDS=… go test -count=1 -run TestSyntheticRecordArtifact ./internal/diagnostics` | 0；由真实 Go 边界生成三类基础记录 |
| `python contracts/diagnostics/v1/validate.py` | 0；3 schemas、53 fixtures、9 example lines、242 oracle vectors，清单摘要吻合；不作为 Go 实现通过的替代 |
| `python internal/diagnostics/testdata/validate_records.py coordination/diagnostics/20260924/DIAG-04-synthetic.jsonl` | 0；Go 生成的 3 行通过冻结 schema 和跨字段语义 |
| 工作区／index SHA256SUMS 原始字节校验；`git diff --cached --exit-code bb291667f7b6bd7a1dab6f9b7f906b5871d1306c -- contracts/diagnostics/v1` | 0；摘要一致、冻结目录无差异 |

原始拦截错误均为：`An Application Control policy has blocked this file.`
首次 logging 路径为 `C:\Users\lywx2\AppData\Local\Temp\go-build3463477620\b338\logging.test.exe`；
第一轮全量中的 usage 路径为 `C:\Users\lywx2\AppData\Local\Temp\go-build2760769788\b1089\usage.test.exe`。
没有改名、解除封锁、调整策略、安装系统组件或使用批准绕过。
检查现有替代环境：WSL 未安装；Docker `desktop-linux` daemon pipe 不存在，无法开展 Linux 回归。
`CGO_ENABLED=0`，没有可用 gcc/clang，未运行 race instrumentation；普通并发专项已通过。

契约 Python 固定测试依赖仅安装到本 worktree 的 `.diag04-validation/python` 临时目录；自动审批两次拒绝相关递归删除（仅返回 `blocked by policy`，没有更具体理由），因此该目录保留、未纳入 Git。临时 server 二进制及已归档测试日志已用非递归操作删除。首次系统证书适配器访问异常的安装进程已停止，使用 pip 的正常 CA bundle 验证路径完成安装，没有关闭 TLS 证书校验。

## 脱敏样例与证据范围

[DIAG-04-synthetic.jsonl](DIAG-04-synthetic.jsonl) 是 Go 测试生成的 process/call/server 三行，只使用合成资源标签和本地测试上下文，不是生产采样。三行均小于 2 KiB。

188 项 Go 共享用例：headers 59、http-ingress 6、peers 52、outbound 8、redirects 4、peer-response 11、copy-source 2、resources 8、source-scope 12、coverage 26。Go 资源算法验证 worker/restart 行为，服务常量仍固定为 cliproxyapi，不因外国服务向量改变生产 service。graph/count/Aito 语义由契约 oracle／后续对应任务负责，本任务不新增离线调用图分析器。

## 已知限制／后续交接

1. 现有 logger 没有终身开关变更通知、collector 丢弃确认，所以 `accessCapture=unknown`、`sinkDroppedTotal=null`，不冒充 full。Engine 只计序列化／超限失败和自定义 sink 明确返回的错误；生产 logrus sink 恒返回 nil，实际 writer／队列／collector 写失败不可见。
2. DIAG-05 的四类 DEBUG 语义尚未实现；本任务 `debugCapture=none`，只有 HTTP 能力。DIAG-05 添加事件时需扩展共享 span 序号，保持终局 sealing 和 expectedLastLogSeq 规则。
3. 现有认证主体可能是 API Key，未输出／hash；callerAlias=null、scope=unknown；没有安装新的可信入站 peer adapter。
4. Gin status-only 的最终隐式提交发生在中间件返回后；本任务准备本地 ID 但不强制提交，wireStatus/delivery 明确未知。Hijack 原始升级不推断 200/101，也不改 WS 协议。
5. 自定义 RT 内部的私有重试、URL 重写或嵌套网络发送不是外层最终边界可见信息。WS、特殊自建 client 的具体缺口见完整矩阵。
6. 尚未验证真实部署多 worker stdout collector；4 KiB 不是跨平台原子写保证。由部署级 DIAG-07 验证，或使用每进程独立文件／收集身份。
7. 未做 Linux／race 回归；环境限制如上。Windows 完整回归以最后验证结果为准，不抹去较早被拦截的执行记录。

没有创建子任务、跨项目写入、部署、合并、推送、生产账户变更或自行 Claude 审核。只做本地提交，等待协调窗口检查及 Claude 精确 HEAD 审核；修订继续在本任务。


## R1 退回修订

原审核目标为 `baf745a579667828afa21fd53a4573a23a374cce`，0 P1／3 P2，结论 request_changes。
修订逐项结论、新增证据、日志消费方的实际兼容修复及 P3 取舍见
[DIAG-04-R1-disposition.zh-CN.md](DIAG-04-R1-disposition.zh-CN.md)；最终新完整 SHA 由本任务交付消息提供。
本节之前的“最后一次”指初次提交验证；修订轮的最终结果以 R1 disposition 和 R1 validation 为准，早期应用控制拦截记录保留。
