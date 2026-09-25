# DIAG-04 R1 修订交付（待复审）

本轮只在原 worktree `C:/Users/lywx2/.codex/worktrees/a88f/CLIProxyAPI`、分支 `codex/diag-04-cpa-tracing` 工作。
原审核 commit `baf745a579667828afa21fd53a4573a23a374cce`；冻结基线 `bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`。
只读完整 R1 review，逐项核查源码后处置。没有推送、部署、合并、自行 Claude 审核、子代理或跨项目写入。
新完整 SHA 在交付消息给出，供协调任务及 Opus 5.5 精确 HEAD 复审。

## P2 逐项 disposition

| 项目 | 处置 | 可核对证据 |
| --- | --- | --- |
| P2-1 动态目标过滤破坏业务头 | 已修复。按实际入站 source 名排除 traceparent/tracestate/X-Diag；target 仅排除 X-Diag-*。显式 `$X-Business` 映射到 traceparent/tracestate 保留 | `internal/util/header_helpers.go`；`TestDiagnosticAutomaticHeaderReferencesVersusLiterals` 覆盖普通动态来源、大小写 source、X-Diag target、显式 trace target、literal |
| P2-2 真实 executor 证据不足 | 已补完整全仓静态清单及真实 Execute 对照；没有额外改 provider 生产代码 | `internal/runtime/executor/diagnostics_execute_test.go` 的 `TestDiagnosticRealExecutorEquivalence`；下节及 source-evidence |
| P2-3 终局 cancel 测错 context、四元组缺断言 | 已改为外部创建并取消 request context，传入 Gin；明确验证所有要求的四元组；补返回 write error 的底层 writer | `TestGinCommitErrorStreamingAndNoEarly200`、`TestGinWriteFailureTerminal`、`TestHijackPreservesUpgradeAndDoesNotInventWireStatus` |

真实 Execute 对照使用本地 httptest HTTP 上游，分别在普通 ctx 与带真实 Engine.StartServer 的 ctx 执行同一请求。
Codex、Claude、Gemini、Kimi × 200/503，共 8 组、16 次本地业务执行。
断言 POST/path/HTTP1.1、上游输入文本、原生 Codex stream=true、Gemini token cap、Kimi model normalization，
以及成功 response.Payload 字节相等／失败状态 503 和错误正文相等、既有 UpstreamAttempted 跟踪存在。
诊断分支还验证真实上游收到 peer trace、仅一个 diag.call、callKind=model、upstreamStatus 与实际一致、终局 eof。
因此 503 不被错误归类为传输失败。此测试不是直接调用 builder 冒充 executor。
Claude 实际经过 NewUtlsHTTPClient 的本地 fallback 分支；不宣称对受保护域名完成新的原生 uTLS/HTTP2 指纹握手。
原有 protected transport 测试仍在完整回归内，没有生产域名请求或真实凭证。

## 所有最终 builder 出口与后置 Transport 使用核查

[原代码摘录和完整 rg 清单](DIAG-04-R1-source-evidence.md)保留每个生产匹配的文件与行号，包括 examples/cmd/sdk/internal。
查询包括 `\.Transport\b`、三个 builder 全部引用、所有 `(*http.Transport)` 断言与 transport nil 分支、SetProxy 全部调用。
Transport 搜索采用更宽的表达式，故包含类型声明和注释；不是只搜索本次 diff。

| 范围 | 后置检查／赋值结论 |
| --- | --- |
| NewProxyAwareHTTPClient | 唯一 return 包住 Base 的整个调用，Base 的有效 proxy 早返回与 context/default 返回均先完成后装饰 |
| NewUtlsHTTPClient | 完整函数只有最后一个 return，proxy/fallback/context protected-host 路由与 timeout 设置都在该 return 之前；不存在未装饰早返回 |
| NewDevinHTTPClient | named return + deferred FinalizeClient；context `rt.(*http.Transport)`、cache Clone.DisableCompression、custom no-gzip fallback、proxy/default fallback 三类出口都在装饰之前 |
| newAntigravityHTTPClient | 已有 Base 调用是唯一需要后置具体 transport 配置的调用方；nil 检查、具体断言、typed-nil、HTTP1.1 cache 分支后由 defer 装饰。此次无需改生产实现 |
| usage_helpers.trackHTTPClient | 读取已完成的 client.Transport，nil fallback 仍只供无诊断调用；对 client copy 赋 usageTTFTRoundTripper，其 base 是整个诊断 transport，usage 保持外层，没有具体类型断言 |
| Codex HTTP/SSE/compact、Claude execute/stream/tokens/HttpRequest | 全部最终 uTLS builder 调用之后仅 usage 装饰／Do；无 Transport 类型断言、nil 分支或后置赋值 |
| Gemini/Vertex/CLI、Kimi、xAI、Meta、OpenAI-compatible、Codex images、Devin execute/stream/metadata | 三个 builder 后无 Transport 断言／赋值。Gemini CLI/Vertex 与 Kimi 的 `if httpClient != nil` 检查的是 client 指针，装饰不改变 nil 性质 |
| Gemini CLI shared wrapper / OpenAI video download / grounding URL / fetch_devin_models | wrapper 直接返回共享 builder，调用方消费 client；没有后置 Transport 配置 |
| pluginhost normal HTTP bridge | 共享 builder 后仅 client==nil 兜底；独立 WireProfile 分支在另一分支自建 transport，不消费共享 builder 返回值，仍不声称诊断覆盖 |
| util.SetProxy | 全仓调用均在自建 client 上；Gemini CLI auxiliary 在 SetProxy 后显式 Finalize。其余独立 auth、管理、updater、homeplugins 不消费已装饰 builder，无后置覆盖 |
| cmd/fetch_codex_models、cmd/fetch_antigravity_models、sdk/cliproxy/antigravity_models | 直接构造自己的 http.Client 再赋 proxy transport，不消费以上 builder；后台/独立边界不变 |
| management APICall | 自建 client.Transport=h.apiCallTransport，独立管理 API 边界不变 |
| diagnostics FinalizeClient 自身 | 唯一新增类型断言为自身 transport 幂等识别；复制 client 后赋 wrapper，没有向底层 owner 反写 |
| sdk/proxyutil、auth/claude、transport_cache、rtprovider、plugin WireProfile、examples | 自有 transport 构造／缓存／底层配置；没有对三个最终 builder 返回的 wrapper 做具体断言 |

负面结论限于当前精确仓库，不推断第三方嵌入客户端或插件私有内部行为。
原有 `TestDiagnosticsFinalClientBranchesAndUsage` 21 个 builder/branch 组合与 Antigravity HTTP1.1 分支测试继续验证。

## Gin 终局真值表和生产 ID

| 实际用例 | endReason | deliveryState | headersCommitted | wireStatus |
| --- | --- | --- | --- | --- |
| 显式 HTTP 503 正文 | finished | local_finished | true | 503 |
| 流式 Flush + 两次写 | finished | local_finished | true | 202 |
| 已恢复 panic | finished | local_finished | true | 500 |
| status-only 204，Gin 在中间件返回后提交 | finished | unknown | false | null |
| 外部已取消 ctx，handler 写 499 | client_cancel | cancelled | true | 499 |
| 底层写错误，已提交 202 | error | failed | true | 202 |
| raw Hijack，实际客户端收到 101 | unknown | unknown | false | null |

`TestNewServerDiagnosticRequestIDMatchesLegacyContextAndLog` 调用真实 NewServer（沿用 newTestServerWithOptions 的临时配置），
不手工重建简化中间件栈。新增测试路由经过生产全局中间件：GinLogrusLogger → GinDiagnostics → Recovery → CPA trace → request logging → CORS → Home/safe-mode。
验证响应诊断 ID 是 8 位 hex，等于 GetGinRequestID、GetRequestID(ctx) 以及旧 access log 的 request_id，正文/status 不变。
源摘录显示旧 ID 在 c.Next() 之前写入，无论 log 的输出发生在之后。

流式写保留既有所有权：ForwardStream 的 chunk/error/keepalive 都在同一 select 循环；非流式 keepalive 的 stop 函数关闭 channel 并 wg.Wait 后，调用方才写最终响应。
相关原函数摘录纳入 source-evidence；没有新增并发 writer、改背压或为诊断重做同步。

## 日志消费者核查与有证据的额外修复

| 消费方 | 实际行为与验证 |
| --- | --- |
| 全局 logrus writer/rotation | 安装 LogFormatter；diagnosticLine 私有类型输出原始 @diag 行；原 formatter/gate 测试继续通过 |
| Home app log mux hook | StartHomeAppLogForwarder 明确安装 LogFormatter；formatEntry 保存整行到 payload.Line。`TestDiagnosticProductionHookFormattersPreserveLine` 验证原样字节，未声称外部 Home 项目的解析兼容 |
| TUI hook | 默认构造器虽用 TextFormatter，但生产 cmd/server/main.go:763 紧接着 SetFormatter(LogFormatter)。测试复现该配置并验证仅移除换行，不出现 byte 数组/base64 |
| TUI 显示/filter | ALL/INFO 保留新 INFO 行，WARN/ERROR 排除；没有旧前缀颜色，原文显示。`TestDiagnosticBasicLogDisplayAndFilters` 验证 |
| management cursor 读取 | 逐完整行读取，不依赖旧文本前缀；测试验证原样行及 latest timestamp |
| management legacy after 时间戳 | **发现并修复实际缺陷**：新 @diag 先于本次旧 Gin log 写出，旧 parseTimestamp 返回 0 时可能继承上一条过期记录的 include=false，导致新诊断被过滤。现仅对识别的 ai-proxy-diagnostics/1 envelope 读取 RFC3339 ts，沿用原秒级 cutoff 协议；不引入整个诊断模型依赖。测试覆盖旧行→新诊断跨 cutoff、坏 JSON/schema/ts、cursor 完整行 |
| usage/statistics | internal/usage/logger_plugin.HandleUsage 消费 sdk/cliproxy/usage.Record；统计通过 typed Publish/插件分发，不从文本日志解析，因此没有将 @diag 当 usage 的路径 |
| 第三方 hook/formatter/采集器 | 未验证。生产内建路径已核查，不宣称任意嵌入程序兼容或外部聚合零丢失 |

## P3 建议取舍

1. **sink 写失败文档：接受并纠正。** Engine 内部只计序列化／超限及自定义 sink 返回错误；生产 logrus adapter 恒返回 nil，实际 writer 错误打印 stderr，Home/TUI 队列与 collector 丢弃也不可见。README/初次交付错误表述已修正，sinkDroppedTotal 保持 null。
2. **重复 peer 重载：接受小修。** 在原 processMu 内保存/比较原始环境值；相同值（包括相同无效值）不增 revision、不写 config_changed。改变到无效值仍立即替换并禁用整个 peer 集；测试验证正常→无效→恢复以及无变化事件数。环境通常只有进程重启后才更新，嵌入者修改自身 env 后可由现有 reload 边界读取；文档明确，不宣称读取父 shell 的热变更。
3. **timeout 分类：保留当前语义并说明限制。** 无新增 timeout；call cancelled 只证明 outgoing context 已结束，包含 DeadlineExceeded／Client.Timeout，不证明取消由调用者发起。live ctx 的 transport error 仍归 transport_error。契约无 timeout 枚举，本轮不引入新的推断或更改既有 client 生命周期；后续若协调方统一语义再改。
4. **Finish 持锁 I/O：接受小修。** span 内锁负责 sealing/快照，emit 移到锁外；`TestServerFinishDoesNotHoldSpanLockDuringSinkIO` 用 channel 阻塞 sink，TryLock 及真实 late RoundTrip 验证锁已释放且 span 已封闭，没有 sleep。
5. **路径日志量：保留全部 ingress server terminal，明确成本。** 原 Gin logger 仅明确抑制健康检查 GET/HEAD 成功的旧文本行；SkipGinRequestLogging 在当前生产代码无调用者。诊断仍每次一行 INFO；management/Home HTTP 也产生终局，不能将“同 INFO 门控”误写成“复用路径采样”。为保留已建立 span 的完整终局，本轮不添加路径豁免或采样。README 说明日志量和 operator INFO gate。
6. **redirect：只澄清文档。** 按协调方 DIAG-00 R3 结论，Go 用未被修改的 ireq 为新跳构造显式业务 trace，不是诊断恢复被覆盖字段；ownership 只绑定实际 clone、无共享 owner。无安全边界或代码变化。

## 本轮验证与环境限制

- `gofmt -w .`：完成，Git 只保留任务相关实质 diff。
- 第一轮专项（头、Gin 四元组、真实 executor）：退出 0。
- NewServer/日志 hook/TUI 专项：对应包退出 0；management 新测试首次因 readCompleteLogLines 参数个数错误未编译，已修正调用并纳入后续全量测试，不冒称该次通过。
- 扩展专项 `go test -timeout 120s ./internal/diagnostics ./internal/api ./internal/logging ./internal/api/handlers/management ./internal/tui ./internal/runtime/executor ./internal/runtime/executor/helps ./internal/util ./sdk/api/handlers`：退出 1，logging 和 management 测试程序被 Application Control 拦截，其他执行包通过。
  拦截路径为 `C:/Users/lywx2/AppData/Local/Temp/go-build4031567496/b598/logging.test.exe` 与 `.../b600/management.test.exe`，原文 `An Application Control policy has blocked this file.`。
- 最终 `go test -timeout 10m ./...`：**退出 1，仅未改动的 sdk/pluginhost 测试启动被 Application Control 拦截**，路径 `C:/Users/lywx2/AppData/Local/Temp/go-build518025579/b1096/pluginhost.test.exe`。包括 logging、management、API、diagnostics、executor/helps、util、TUI、SDK handlers 在内的所有实际执行包通过。本轮不记为全量通过；未针对该拦截改名或重跑以规避。由协调窗口复核精确 HEAD。
- 最终 `go build -o .diag04-server.exe ./cmd/server`：退出 0，临时二进制以非递归方式删除。
- 使用已存在的本地固定 Python 测试依赖运行契约 validate.py：退出 0，3 schemas / 53 fixtures / 9 example lines / 242 vectors；既有 Go 合成 3 行 schema 和语义校验退出 0。
- 冻结 SHA256SUMS 原字节摘要仍为 `ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`，对冻结基线的目录 diff 为 0。
- Windows CGO=0、无 Linux/WSL/可用 Docker daemon/race 工具链，延续初次报告限制。没有装系统组件、改名测试程序、改 Application Control 或推送未审分支到 CI。
- `.diag04-validation/` 保留为未跟踪测试依赖；按协调要求不再次尝试递归删除或绕过原自动审批拒绝。

完整最终测试输出见 [DIAG-04-R1-full-go-test.txt](DIAG-04-R1-full-go-test.txt)。旧 R1 前的拦截历史仍在初次交付与 validation 中。


## 本轮文件清单

生产代码仅三处实质修订：

- `internal/util/header_helpers.go`：P2-1 来源过滤修复。
- `internal/diagnostics/records.go`：相同 peer 值跳过重载；server snapshot 锁外 emit。
- `internal/api/handlers/management/logs.go`：有复现证据的诊断时间戳兼容。

新增测试：`internal/runtime/executor/diagnostics_execute_test.go`、`internal/api/server_diagnostics_test.go`、
`internal/api/handlers/management/logs_diagnostics_test.go`、`internal/tui/logs_diagnostics_test.go`。
扩充测试：`internal/diagnostics/runtime_test.go`、`internal/util/header_diagnostics_test.go`、`internal/logging/diagnostics_test.go`。
文档：更新模块 README 和初次交付；新增本 disposition、source-evidence 和完整 full-go-test 输出。
Go 版本/依赖、冻结契约目录、路由/auth/retry/translator/usage/流式生产实现均未修改。
