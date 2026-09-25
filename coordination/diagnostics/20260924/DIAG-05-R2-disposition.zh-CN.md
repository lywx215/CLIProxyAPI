# DIAG-05 R2 修订交接（待独立复审）

本轮根据 Claude Opus 5.5 对准确 HEAD `bb410f92433ada96614caee23d5fb385510e43c5` 的
`request_changes`（0 P1 / 2 P2）继续修订。原报告只读位置：
`G:/code/gemini30/CLIProxyAPI/coordination/diagnostics/20260924/reviews/DIAG-05-R1/review.md`。
本报告所在提交即待复审 R2；完整新 HEAD 由最终交接消息给出。没有发起自审、Claude 调用、
新任务、推送、合并或部署，也没有读取生产凭据或调用模型。

## P2-1：真实流读错误在 server 封存前结算

Gemini ExecuteStream 和 Antigravity ExecuteStream 的既有 errScan 分支，现在在发送
`StreamChunk{Err: errScan}` 前调用 `diag.Finish(errScan)`。仅在上下文仍有效时提前结算；
调用方取消仍保留 deferred cleanup 和 R1 pending/interrupted 机制。已有 sync.Once
保证后续 defer 不重复记录、也不重复释放 pending 登记。

没有改变 scanner、错误对象、retry、body/payload 发送、DONE 翻译、Close、usage 发布、
通道关闭或取消顺序；没有等待执行器、排空 Chunks、额外读取或代关业务流。

真实回归 `TestDIAG05GinReadErrorBeforeExchangeCleanup` 覆盖 12 种组合：
Gemini / Antigravity × 原生 Gemini / OpenAI chat / OpenAI Responses × bootstrap 首读失败 / 部分帧后失败。
采用真实 Gin、Manager/conductor、executor、translator、ForwardStream 和中间件，
仅将 HTTP transport 替换为内存 fixture。Body.Close 被通道阻塞，测试在释放 Close 前等待
真实 handler 返回，要求恰好一条 `error/read` attempt，`eofSeen=false`，且早于 server 终局；
释放 Close 后不新增记录。不依赖 Sleep，不由测试排空流。

保留 Gemini DONE 的具体证据：原生和 chat 转换器对 DONE 返回空；Responses 转换器在已有输出后
会产生 `response.completed` 尾帧。完整 `forwardResponsesStream` 和 `ForwardStream` 表明，
完成帧本身不会让 handler 返回，它仍接收后续 Err；既有 framer 在已有 terminalEvent 时抑制
第二个 wire error。本轮用例要求该完成尾帧保持存在，诊断仍明确记录上游读错误。
Antigravity error 分支不产生 clean tail，用例禁止新增 DONE/response.completed。
因此无需拆分 upstream/conversion 结算；Err 前的最小修复足够。外部客户端在尾帧背压期间取消
仍可中断观测，R1 的 interrupted 机制继续诚实覆盖这种情况。

复现原生产逻辑时，最终正确 harness 的 12 个用例全部因 attempt 缺失 / interrupted 失败：
`DIAG-05-R2-read-reproduction-final.txt`（退出 1）。早期两份 reproduction 日志保留了
harness 修正过程：错误响应已被业务脱敏，chat 不会输出字面 DONE，Antigravity 首次使用会启动
进程级 signature-cache 清理 goroutine。最终 harness 将该单例初始化放在 synctest bubble 外，
并按实际 wire 行为断言；最终复现没有死锁。最终取消回归也通过，未用放宽 R1 断言消除失败。

## P2-2：观测限额和未知语义不归责上游解析

`inspectJSON` 区分实际 malformed 与本地 limited；candidate 身份超过 64 同样标记 limited。
4 MiB、深度 64、candidate 64 上限不变，不修改业务 parser 或冻结 schema。

- limited：没有更强失败证据时，result/origin/stage/error 都为 unknown；parserFinishOk 和
  terminalSeen 为 null。上游和转换产物的输出聚合全部置为 null，不能把先前帧或部分 candidate
  的计数冒充全量。EOF 仍独立反映实际读取结果。已实际观察到的显式 usage 快照保留，不能据此
  推断看到了最终 usage。
- 未识别的字符串 finishReason：保留该 candidate 的终止证据与可验证计数；结果保守 unknown，
  不判 success、不记 parse_error。非字符串 finishReason 也视为不支持语义，不冒充 JSON 解析失败。
- 真正坏 JSON / 非对象：继续 incomplete/upstream/parse/parse_error。
- 取消、传输/读取错误、HTTP/错误帧证据仍先于以上分类；没有因为 local limit 将这些失败变成成功。

`TestSemanticObservationLimitsAndUnknownFinish` 共 56 个组合：合法超过 4 MiB 的 inlineData、
合法深层 JSON、65 candidates、MALFORMED_FUNCTION_CALL、未来合法字符串、坏 JSON、非对象，
分别测试流/非流及 clean/read-error/cancel/error-frame。合法 fixture 另经标准 JSON validator
断言；流式用例先提供正常帧，确认后续超限会撤回不完整的聚合计数。修复前失败日志：
`DIAG-05-R2-limits-reproduction.txt`；修复后相关测试通过。

## P3 逐项取舍

| 建议 | 本轮处理 |
| --- | --- |
| transformations 的 other@index0 噪声 | 仅比较两侧 contents 的对应位置；model/config/envelope 变化不产生 transformation。变化记录使用真实对应 index，最多 16 条。仍只是位置差异，不猜清理原因或编辑操作；另有针对性断言。 |
| attemptNo scope | scope 改为 conductor_gemini_family，不扩展未接线 provider 的 attempt 计数。混合 provider 上下文测试和 conductor 测试更新；HTTP send 仍不分配业务 attempt。 |
| compaction 文档 | 更正矩阵：递归 summary e.Execute 会产生普通 exchange，沿用外层 attempt 身份；capsule/wrapper 没有独立语义记录。不得按多条 exchange 推断新派发。 |
| capability 与路由 | 明确交接 DIAG-06：process capability 是实现能力，不证明任意路由事件完整性；需结合覆盖矩阵。其他路由可以 enabled_throughout 但无语义事件。 |
| 全局 epoch | 文档明确改变 logger/NotifyDebugDisabled 的测试不得 t.Parallel；本轮未更换全局 epoch 机制，相关测试保持串行。 |
| empty conversion | 保留 unknown。仅有零输出计数不足以证明各下游协议完整结束；上游 empty 结论仍独立可用。 |

## 验证与环境记录

| 命令 / 检查 | 结果 |
| --- | --- |
| gofmt -w . | 退出 0；Go 文件换行归一后仅保留预期差异 |
| go test -timeout 120s ./internal/runtime/executor -run '^TestDIAG05GinReadErrorBeforeExchangeCleanup$'（修复后） | 退出 0，read-targeted 日志 |
| go test -timeout 120s ./internal/diagnostics -run 'TestSemantic'（分类初版） | 退出 0，limits-targeted 日志 |
| go test -timeout 180s ./internal/diagnostics ./internal/runtime/executor ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/api/handlers/gemini ./sdk/api/handlers/openai（最终源码） | 整体退出 1；diagnostics/executor/auth/gemini/openai 通过，handlers 的 exe 被 Windows Application Control 拦截，原始 related-tests 日志保留 |
| go test -timeout 10m ./...（最终源码，整轮回归） | 退出 0；全部包通过，含上述 handlers。完整 full-go-test 日志保留 |
| go build -o .diag05-R2-server.exe ./cmd/server | 退出 0；本地验证 exe 已删除，build 日志保留 |
| python contracts/diagnostics/v1/validate.py | 退出 0；3 schemas / 53 fixtures / 9 example lines / 242 vectors |
| python internal/diagnostics/testdata/validate_records.py 两份 R2 synthetic.jsonl | 退出 0；294 条真实 Go 输出记录通过冻结 schema 和语义检查 |
| 冻结目录与 DIAG-04 基线逐文件二进制比较 | 73 个文件逐字节相同；manifest SHA-256 不变 |

归档文本仅统一 LF 并去除行尾空白，保留所有测试结果和错误内容。

专项拦截的精确错误：
`fork/exec C:\Users\lywx2\AppData\Local\Temp\go-build1049650364\b469\handlers.test.exe: An Application Control policy has blocked this file.`
没有更名二进制、改策略或循环重试；随后运行的是要求的完整 Go 回归。
race/Linux 验证仍受 Windows、CGO=0 和既有环境限制，未宣称已运行。原有未跟踪
`.diag05-validation/` 保留，仅用于离线 Python 依赖，没有加入提交。

`DIAG-05-R2-verification.json` 记录摘要：read 样例 70 行，limits 样例 224 行，
最大行分别 1885 / 1827 字节；冻结 manifest SHA-256：
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。

## 完整源码复审材料

[DIAG-05-R2-source-evidence.md](DIAG-05-R2-source-evidence.md) 包含 21 个完整文件、每文件 LF
SHA-256：Gemini executor、Antigravity executor/execute/stream、ForwardStream、handlers_stream、
Gemini/chat/Responses handlers、conductor stream/execution/Home、两个实际 DONE 转换器、
diagnostics 生命周期与分类、新旧真实 Gin 回归。没有只截 diff hunk。该附件是源码证据，不是审核结论；
独立复审仍需对本轮准确 HEAD 给出意见。
