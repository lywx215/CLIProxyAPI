# DIAG-05 协调方 R1 修订：待审核

## 范围与身份

- 项目：CLIProxyAPI；任务：DIAG-05。
- worktree：`C:/Users/lywx2/.codex/worktrees/15cf/CLIProxyAPI`。
- 分支：`codex/diag-05-cpa-diagnostics`。
- 本轮起点：`a8298639d0e1cc22b7372a57d66baceead5cb459`；进入时 tracked clean，仅保留上一轮未提交的 `.diag05-validation/`。
- 原冻结开发基线：`50d335a18a2495bd2fbbb30ac47d09476a8a14b6`。
- 新准确 HEAD 由本地提交后的任务消息给出；本文件属于该提交，避免自引用。
- 契约来源：`bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`；manifest 原始字节 digest 仍为 `ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。

本轮是协调方发现问题后的修订，并非 Claude 审核通过。没有自行调用 Claude、推送、合并、部署、子任务、跨仓库写入或生产操作。未读或复制 gcli 的 Python holder 实现；采用 Go 实际生命周期和冻结契约。

## 1. 提前封存：已复现并修复覆盖声明

新增 `TestDIAG05GinCancellationBeforeExchangeCleanup` 经过真实 Gemini Gin handler、Manager、GeminiExecutor 和 GinDiagnostics。假 Transport 不发网络请求：Read 在取消时结束，Body.Close 用两个 channel 明确通知“进入清理”和“允许清理返回”。测试直到 `router.ServeHTTP` 完全返回、server 终局可读，才释放 Body.Close。

两个子场景分别是等待第一帧时取消，以及已有第一帧、handler 正在限速时取消。测试没有读取/排空 executor.Chunks，没有由测试关闭业务流，没有 Sleep/GC 定序，也没有添加生产等待。修订前两者均表现为：

- `upstream.attempt_finished`/`response.converted` 尚未构造；清理后因 server sealed 也不会再构造。
- server 终局却为 `debugCapture=enabled_throughout`，`expectedLastLogSeq=3`、自定义确认 sink 的 `droppedForSpan=0`。
- 干净复现退出1，仅因上述两条不诚实的 capture 断言失败；见 `DIAG-05-R1-reproduction-clean.txt`。

采用“明确可证明采集中断”方案，而非合成 attempt：

1. `NewExchange` 在 server mutex 内检查 DEBUG/sealed 并登记 `pendingExchanges`，与 server 封存原子排序。只保存一个计数，不建立异步队列或保存 response。
2. `Exchange.Finish` 使用 `sync.Once`；完成或被现有门控抑制后都只注销一次。并发重复 Finish 不能重复日志或误减其他 exchange。
3. server 封存时若计数非零，设置 `debugCapture=interrupted`。server 不读取 exchange 的可变 summary、不等待其 Finish/Body.Close/reader/channel。
4. 之后的清理仍不追加语义记录、不修改已发终局、不凭空声明 EOF 或 attempt 结果。

`droppedForSpan` 仍只统计已构造事件的已知写入损失，不因未构造的语义记录增加虚构 drop；expectedLastLogSeq 仍是实际 terminal seq，不给未知未来事件预留虚构序号。即使同 span 序号连续、测试 sink 回报0已知丢失，冻结覆盖规则也因 interrupted 判定 partial，不能宣称 attempt 完整。生产无写入确认 sink 的 null 与 accessCapture=unknown 规则不变。

unit `TestSemanticPendingExchangeCoverageAndOnceSettlement` 覆盖：两个未完成 exchange、8次并发重复 Finish、全部结束后正常封存、有一个未结束时封存、封存后拒绝新登记、late cleanup 不追加记录、AssessCoverage 得出 partial/terminalMissing=false。本轮仅改独立 diagnostics 模块，不改 executor/handler 生产控制流。

## 2. 辅助 call 的 attempt：核验后按已有 owner 标签限缩

契约明确有 `unless the owner truly associates them` 例外，因此没有把辅助关联一律当作禁止。具体调用链证据为：

- `conductor_execution.go`/`conductor_stream.go`/`conductor_home_execution.go` 给整个 Execute/ExecuteStream 参数附加 `conductor_executor` context，覆盖调用的动态范围；这本身没有声明内部每次 HTTP 都属于 model attempt。
- `helps/usage_helpers.go` 的 `usageTTFTRoundTripper.RoundTrip` 是现有明确 model 发送 owner：先保持原有 MarkUpstreamAttempt 调用，再仅给发送副本标 `WithCallKind(...,"model")`。
- `helps/antigravity_grounding_urls.go:resolveAntigravityGroundingURL` 同一 ctx 创建另一个 client，HEAD 解析 URL，CheckRedirect=ErrUseLastResponse；没有 usage wrapper/model owner 标记。
- `antigravity_executor_auth.go:refreshTokenSingleFlight` 同一 ctx 建 client 发鉴权请求，也没有 usage wrapper/model owner 标记。

据此，当前辅助路径只有 context 继承，没有契约例外所要求的明确关联证据。修订仅在 transport 最终公共 call 投影判断**改写 callKind=redirect 前**的既有来源标签是否 model：是才 applyAttempt。所有 HTTP 仍分配独立 callNo；model redirect 保留 model owner；other/auth/metadata redirect 不被提升为 model。

`TestDIAG05ModelAttemptProjectionAndAuxiliaryRedirects` 在同一实际 ExecutorAttempt ctx 上调用真实 helper client/usage wrapper：model/other/auth/metadata 各直接与 Go redirect 两种路径，加上真实 grounding HEAD helper，共13次内存发送。修订前9个辅助/redirect投影和实际grounding投影均误继承；反例见 `DIAG-05-R1-aux-reproduction.txt`。修订后检查 method、body/status、redirect策略、usage marker、callKind、连续callNo和发送总数不变，只有有model标签的call携带attempt。

没有添加新的业务标签，没有改变HTTP次数、usage计数、重试或helper逻辑。未来若某辅助 owner 明确提供独立关联证据，仍应按契约评估，不能仅靠继承 ctx 放宽。

## 3. imports

三个 conductor 文件的新 diagnostics import 已移至第三方/项目 import 组，与标准库组空行分隔。经过 `gofmt -w .`；仅分组变化，没有执行逻辑修改。

## 实际验证

| 命令/阶段 | 结果 |
| --- | --- |
| 核对 git HEAD/status | 0，准确本轮起点 |
| 首次真实 Gin 取消复现专项 | 1；先复现 enabled_throughout 缺口；测试 fixture 还遇到全局 usage dispatcher 在 synctest bubble 中启动后不退出的问题，及不当假定取消 body 必为空，原输出保留于 `DIAG-05-R1-reproduction.txt` |
| 修正 fixture 后，同一个未修生产逻辑的 `go test -timeout 120s ./internal/runtime/executor -run '^TestDIAG05GinCancellationBeforeExchangeCleanup$'` | 1；两个预期 lifecycle 断言失败，无测试框架死锁；`DIAG-05-R1-reproduction-clean.txt` |
| 生命周期修复后 `go test -timeout 120s ./internal/diagnostics ./internal/runtime/executor -run 'TestSemantic|TestDIAG05GinCancellationBeforeExchangeCleanup'` | 0；`DIAG-05-R1-targeted.txt` |
| 辅助修复前 `go test -timeout 120s ./internal/runtime/executor/helps -run '^TestDIAG05ModelAttemptProjectionAndAuxiliaryRedirects$'` | 1；真实辅助投影反例；`DIAG-05-R1-aux-reproduction.txt` |
| 两项修复后 `go test -timeout 120s ./internal/diagnostics ./internal/runtime/executor ./internal/runtime/executor/helps ./sdk/cliproxy/auth -run 'TestSemantic|TestDIAG05|TestGoRedirectsPreservePristineBusinessHeaders|TestDiagnosticsFinalClientBranchesAndUsage'` | 1；diagnostics/auth通过，executor/helps 的测试 exe 启动被应用控制拦截；`DIAG-05-R1-targeted-final.txt` |
| 本轮必需完整 `go test -timeout 10m ./...` | 退出0；全部包通过，包括本轮专项启动曾被拦的executor/helps；完整原始输出 `DIAG-05-R1-full-go-test.txt` |
| `go build -o .diag05-R1-server.exe ./cmd/server` | 0；临时编译产物删除，没有运行常驻/生产server |
| `gofmt -w .`；Git diff --check | 0；tracked实质diff仅本轮模块、imports、tests/docs |
| 复用上一轮 worktree 临时依赖，Python3.12.10：`python contracts/diagnostics/v1/validate.py` | 0；3 schemas、53 fixtures、9 example lines、242 vectors，无改契约 |
| 同环境 `python internal/diagnostics/testdata/validate_records.py coordination/diagnostics/20260924/DIAG-05-R1-cancel-synthetic.jsonl` | 0；10行真实测试生成JSONL，schema/语义/4096字节上限通过 |
| 工作区/Git HEAD/index manifest 原始字节 SHA-256 与 frozen-dir diff | 0；三份digest一致，冻结目录未改 |

本轮应用控制准确原文仍为 `An Application Control policy has blocked this file.`，路径：

- `C:/Users/lywx2/AppData/Local/Temp/go-build1794385888/b290/executor.test.exe`
- `C:/Users/lywx2/AppData/Local/Temp/go-build1794385888/b464/helps.test.exe`

未改安全策略、二进制名称、运行权限，未单独循环重试被拦包。随后只执行任务要求的完整回归，不隐去这次专项失败。此前父窗口/初始DIAG-05通过不能替代新HEAD验证。WSL/Docker/race的既有环境限制不变，未安装组件。

fixture修正仅把已经存在的全局 usage dispatcher 提前在 synctest bubble 外启动，及允许已有取消路径返回错误 JSON；没有改生产实现来消除 fixture 死锁或强制取消 body 形态。

## 新脱敏样例与交接

`DIAG-05-R1-cancel-synthetic.jsonl` 是最终修订源码的真实回归生成：10行（2×process/normalized/call/throttle/server），最大1338字节；SHA-256 `909bc06bf44febcd93de2821185ab228b279eb577574357ba51d04cf23db6705`。

两条 server 都是 client_cancel、seq=expectedLastLogSeq=3、debugCapture=interrupted；没有 attempt/converted 终局。这就是已知采集中断证据，不是 attempt 完整样例。测试 sink 的 droppedForSpan=0 只陈述已构造事件未发生可见sink损失；它不能覆盖未构造语义事件的缺失。

DIAG-07 独立复现入口为该真实Gin测试及辅助测试（仅内存Transport，不需要真实凭证或网络）：

```powershell
$env:DIAG05_R1_TEST_RECORDS = Join-Path (Get-Location) 'coordination/diagnostics/20260924/DIAG-05-R1-cancel-synthetic.jsonl'
go test -timeout 120s ./internal/runtime/executor -run '^TestDIAG05GinCancellationBeforeExchangeCleanup$'
go test -timeout 120s ./internal/runtime/executor/helps -run '^TestDIAG05ModelAttemptProjectionAndAuxiliaryRedirects$'
```

只本地提交并等待协调窗口审核。修订后的HEAD不继承此前验证或任何尚未产生的Claude批准；不得推送。
