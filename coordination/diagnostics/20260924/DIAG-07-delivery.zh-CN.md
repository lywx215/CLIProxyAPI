# DIAG-07 交付候选

**状态：待代码审核；整体验收被 Windows 策略阻塞。DIAG-07 未完成，不放行推送或整体发布。**

R1 审核后的代码修订见 [R2 处理说明](DIAG-07-R2-disposition.zh-CN.md)。新增范围隔离与双边断言仅做既有导出重放，没有新增 CPA live 证据；下文历史运行及其原始制品保持原有含义。

任务 `01a0d7d6-ae7f-71f2-ab65-6732c27975d7`，隔离 worktree `da3a/CLIProxyAPI`，
分支 `codex/diag-07-integration`。基线 `9422a853a222aef0dbf67815888c53ef6f1ede77`；
开工时 Git 根、HEAD、tracked clean 全部符合派发。交付 HEAD 由提交后的消息给出，避免文档自引用。

现行协调记录见 [脱敏状态快照](DIAG-07-coordinator-snapshot/DELIVERY_STATUS_CN.md)、
[manifest 快照](DIAG-07-coordinator-snapshot/manifest.json)及其原始/脱敏字节摘要
[sources.json](DIAG-07-coordinator-snapshot/sources.json)。继承的旧任务包“候选/待批”状态
不是本次依赖依据。没有复制完整 Claude 输入或其他仓库业务源码。

## 冻结依赖和修改范围

| 输入 | 准确版本 |
|---|---|
| DIAG-00 | `bb291667f7b6bd7a1dab6f9b7f906b5871d1306c` |
| DIAG-02 gcli | `0b3a07e003ead2ba7a9f7827426c09f8ff996813` |
| DIAG-03 Aito | `a2d51383bc91751f23bc0f8927c19ef736593cea` |
| DIAG-05 CPA | `4996e7ae12b2af38f3bdc490eedf32e1887aeed6` |
| DIAG-06 / 本任务基线 | `9422a853a222aef0dbf67815888c53ef6f1ede77` |

契约 `ai-proxy-diagnostics/1`、`1.0.0-rc.1`；SHA256SUMS 原始字节 SHA-256
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。
三仓每次服务启动前核对批准 HEAD/clean、清单摘要、72 成员内容及含清单的 73 文件集合。
另以既有核验脚本检查本仓 Git/index/worktree 字节未变。

**既有主体流程修改：无。** 全部新增路径：

- `cmd/diag-integration/`：隔离真实 Gin/Manager/Gemini executor/handler/translator/HTTP 入口；合成认证注册、已知模型注册、精确 loopback dial allowlist、临时端口及控制端点。
- `scripts/diag-integration/`：独立调用方/提供方、批准 gcli/Aito 入口 wrapper、逐响应 ID 语义断言、离线故障注入检查、运行与回退说明。
- `coordination/diagnostics/20260924/DIAG-07-*`：实际脱敏诊断 JSONL、合成请求/响应、离线报告、失败历史、命令结果、审计文本快照和本文。

没有编辑运行时、路由、转换器、队列、ACK、重试、usage、背压、代理、取消、new-api、面板版本或生产配置。
未派发子任务、调用审核模型、推送、合并、部署或读取真实密钥/DB/Volume。

## 实际结果

逐条验收见 [矩阵](DIAG-07-matrix.zh-CN.md)，逐次请求见
[run-06 requests](DIAG-07-run-06/requests.json)，340 项按响应 ID/trace ID 对应真实文件行号的
[语义断言](DIAG-07-run-06/semantic-checks.json)全部通过。

- run-06 是 **2 gcli + 2 Aito 的独立 live 子集**，不是六实例目标链路。原生 Gemini/OpenAI 流、非流、gcli 内部流转非流均实际执行；同 instance 多进程与同 instance 重启保留不同 boot。
- 先前四进程 smoke 是 **2 CPA + 2 Aito**，8 次真实请求；离线工具核验 4 条 CPA→Aito 双边唯一受控边。
  [原始结构化日志](DIAG-07-aito-smoke/)和[受控双边报告](DIAG-07-offline-02/controlled-pair.json)保留。
  该早期 CPA 构建使用固定 1000 token/s、100ms 配置，不能代替随后加入参数的拒绝构建。
- run-06 导出 355 条记录，9 个进程文件，DIAG-06 接受全部、0 quarantine：81 个 server、83 个 call、83 个已知 attempt。
  51 个 attemptFailures 包含批准 gcli 的 incomplete/local/read，不表示 51 次业务请求失败或上游故障。
- gcli OpenAI 流在 DONE 后未消费 HTTP EOF 时，可以同时出现 converted success 和 attempt incomplete/local/read。
  独立 call 比 server 晚到的情况实际存在：gcli1 12 条、gcli2 1 条；最终每个已观察 server 声明的 callCount 都有对应 call。
  **未隔离证明这些迟到事件恰在 shutdown_asyncgens 阶段产生**；这一特定时序仍列缺口。
- gcli 嵌套续写/内层重试产生真实 attemptNo `[1,2,1,2]`、4 个独立 attemptId/HTTP call；按 ID 计数。
  CPA 跨 gcli 实例的外层重试尚未执行，不能拿此单服务嵌套结果代替。
- 首个 finish 帧带 87 candidate + 13 reasoning 时，gcli 实际 OpenAI 输出87；Aito 输出100且包含reasoning。
  单独只有尾帧usage时，gcli上游仍87/13，实际客户端没有usage，delivered保持null。
  重复累计帧没有被诊断求和；usage缺失/显式0独立。所有数值来自合成场景，不证明历史计费平台算法。
- gcli 400清理使用实际 no-prefill 范围内的 `gemini-3.7-flash`，验证连续空/纯空白/末尾model/工具上下文，
  全空保持空contents并由合成提供方返回既有400，没有注入虚构用户文本。
- Aito正常关闭走批准 `fixture.close()`，收到close/dispatch计数回执，再等待exit0。
  受控硬杀前观察到浏览器dispatch=2，文件仅保留先前完整请求；未证明kill瞬间响应仍活动，也未隔离缺失记录的flush时序。
  不虚构terminalMissing节点。另按已知采集缺口显式输入 `-known-loss aito4`，
  [报告](DIAG-07-run-06/analysis-known-hard-kill.json)没有verified边。

三仓没有生产入站信任适配器。实际callerAlias为null/unknown；同外部ID保留多个候选，
DIAG_PEERS不认证调用方。只由受控导入和唯一双边证据验证边，未使用时间差推导因果。

## Windows 拒绝和逐轮历史

被拒绝文件原位为 `%TEMP%/diag07-driver.exe`，SHA-256
`da12f45f2719a4e199eb160036caa6dc94eda4bc0cc3fdf40cf1fa5c4fb7c71f`。
其源文件 `cmd/diag-integration/main.go` SHA-256
`393abcdcaab6af5b449d2777debd180dbee49a70bb4cb9caee878bc49e3842e0`，基线加该独立入口，
既有生产文件无diff。拒绝后源Go和该binary均未改变、重建或再执行；管理员精确本地路径已单独交协调方。
[脱敏对象记录](DIAG-07-validation/blocked-binary.json)。未改安全策略、改名程序或通过修改代码换hash规避。
该历史记录只有main.go对应可执行文件，Python/CJS属于当时支持脚本快照；见[旁注](DIAG-07-validation/blocked-binary-note.md)。

| 运行 | 退出码 | 事实及修订 |
|---|---:|---|
| run-01 | Python 2；外层PowerShell最初返回1 | 冻结gcli目录已有额外pyc，严格成员集合预检阻断。原02任务删除该非契约缓存并复核73文件，本任务未跨仓删除。 |
| aito-smoke | 0 | 2Aito+2CPA、8次真实请求、正常close。早期入口没有rate/delay参数；不是6实例。 |
| run-02 | 2 | 三仓预检通过；2gcli+2Aito启动；CPA新构建Popen WinError4551；已启动服务全部正常关闭。 |
| run-03 | 2 | 下游子集39次请求，读错注入触发caller未捕获IncompleteRead。仅修Python夹具保留partial body/readError。 |
| run-04 | 1 | 69项请求/行为检查，全空预期400但假provider错误地接受空contents；no-prefill场景还误选2.5模型。仅修场景模型和假provider。 |
| run-05 | 3；随后语义checker 1 | 72项请求/行为检查；331语义检查有1项期望错误：尾usage被误认为实际交付87。保留失败，再将首帧usage和尾usage分成两个场景。 |
| run-06 | 3；语义checker 0 | 73项请求/行为检查，340语义检查通过；3明确表示验收有强制缺口。 |
| offline-01 | 1 | 重复父节点夹具误选无父直接请求，未制造预期歧义。保留原结果，仅改夹具选择真实双边receiver。 |
| offline-02 | 0 | 10项受控边/重复导入/未受信/known-loss/匿名ID/偏时钟/重复父/坏行/历史样例/近容量检查通过。 |

run-06 的实际执行夹具和语义checker原始字节见
[文本审计快照映射](DIAG-07-source-snapshots/run-06/sources.json)。后续最终runner只增加了
自动调用同一语义checker和硬杀known-loss元数据；相同checker已独立执行，known-loss也以实际CLI补跑。
协调预审后检查器还区分CPA正常HTTP结束/协议错误帧与gcli读断，不再把下游重试数量、HTTP502的空输出分类或usage口径套在CPA上。2项基于批准R2实际记录的checker组件回归通过；最终同一340项下游检查再次通过，见semantic-checks-final.json。受阻CPA仍未live验证，不能据此放行。
最终支持文件摘要另见 [final-support-sources](DIAG-07-validation/final-support-sources.json)。
更早失败轮没有逐轮保存完整源快照，不能伪称存在精确源hash链；其实际日志、退出码、失败及修订差异独立保留。
审计源码用 `.go.txt/.py.txt/.cjs.txt` 后缀，不形成额外编译包。

## 命令与验证

完整联调参数和后续命令见 [运行说明](../../../scripts/diag-integration/README.md)。
本机Go1.26.0、Node24.19.0、gcli现有venv、已有临时QA依赖；所有Python跨仓校验设置
PYTHONDONTWRITEBYTECODE=1，Go使用GOPROXY=off保留checksum验证，不改依赖。

| 命令 | 退出结果/证据 |
|---|---|
| `gofmt -w .` | 0；仅新增入口保留格式。既有Go仅产生换行/stat标记，确认内容diff为空后恢复；最终无既有Go变更。 |
| `go build -o %TEMP%/diag07-driver.exe ./cmd/diag-integration` | 0；早期及加入rate/first-delay后均编译成功。第二个binary运行遭策略阻止，编译成功不等于可执行。 |
| `go build -o %TEMP%/diag07-analyze.exe ./cmd/diag-analyze` | 0；该工具实际用于各轮离线报告。 |
| `go build -o %TEMP%/diag07-server.exe ./cmd/server` | 0；[server-build](DIAG-07-validation/server-build.exit.txt)，未启动普通server。 |
| `go test -timeout 10m ./...` 第1轮 | 1；[full-go-test-01](DIAG-07-validation/full-go-test-01.txt)，executionregistry.test.exe遭ApplicationControl，其余通过/无测试。 |
| 同命令，业务必要rate/TTFT改动后的第2轮 | 0；[full-go-test-02](DIAG-07-validation/full-go-test-02.txt)。不抹去首轮失败，也不代表受阻driver已获准执行。测试开始时审计副本尚有.go后缀，被列为无测试包；已改文本后缀，最终go list确认仅新增真实driver包。 |
| `go test -v -timeout 120s ./internal/diagnostics ./internal/runtime/executor ./sdk/api/handlers -run 'Test(DiagnosticRealExecutorEquivalence\|DIAG05\|CallTerminalLifecycle\|GoRedirectsPreservePristineBusinessHeaders\|BodyInterfacesAndFinalizerIdempotence\|BasicAccessGateIsIndependentOfDebug\|SemanticBoundedParsingAndCumulativeUsage)'` | 0；[component-tests](DIAG-07-validation/component-tests.txt)。真实executor/handler组件、读错、取消、背压、辅助call、观察上限、动态门控、token来源；不替代六进程。 |
| `go test -v -timeout 120s ./sdk/api/handlers -run 'Test(SpeedThrottleNonStreamingDuration\|NonStreamingHandlersThrottleReportedOutput\|SpeedThrottleSlowRequestDoesNotThrottleAgainOnFinalUsage\|SpeedThrottleFirstChunkKeepsRequestStartForCumulativeRate)$'` | 0；[throttle-components](DIAG-07-validation/throttle-components.txt)，真实handler组件及确定时间/已耗时抵扣。 |
| `python contracts/diagnostics/v1/validate.py` | 0；[contract](DIAG-07-validation/contract.txt)，242 vectors；oracle不是六实例证据。 |
| `python internal/diagnosticanalyzer/testdata/verify_artifacts.py` | 0；[artifacts](DIAG-07-validation/artifacts.txt)，73 Git/index/worktree冻结文件及373既有样例摘要。 |
| `python scripts/diag-integration/check_evidence.py .../DIAG-07-run-06` | 0；340项；协调方另独立复跑，本文不代替其审核回执。 |
| `python scripts/diag-integration/check_offline.py --analyzer ... --live .../DIAG-07-run-06 --pair .../DIAG-07-aito-smoke --output .../DIAG-07-offline-02` | 0；[checks](DIAG-07-offline-02/checks.json)。 |
| `diag-analyze -input <all run-06 exports> -trust <all aliases> -known-loss aito4 -format json` | 0；[显式缺口命令/结果](DIAG-07-run-06/analysis-known-hard-kill-command.json)。 |
| 实际CPA/gcli原始stdout直接`diag-analyze` | 两者1；[raw summary](DIAG-07-raw-analysis/summary.json)，严格隔离控制JSON，见下段。 |
| UTF-8 `ast.parse` 全部新增Python；`node --check .../aito-wrapper.cjs` | 各0；[syntax](DIAG-07-validation/python-syntax.txt)。首次手工检查用系统GBK读取UTF-8源导致Python1，随后Node0使外层显示0；未将其记为通过，改显式UTF-8并分开保存退出码。 |

CPA原始stdout 20,134字节/35行：12诊断记录、22 legacy行、1个fixture readiness JSON被隔离，
相关覆盖保守partial；gcli stdout 98,559字节/911行：906 legacy行、1个wrapper close JSON被隔离，
无公共诊断记录，因为它们实际写单独sidecar。原文保留Temp，报告保存完整字节hash及源行号。
这不是生产诊断丢失，也没有据此修改06的JSON规则。人工长legacy/普通JSON场景仍是synthetic-log，
没有把人工输入称作本轮生产者真实超长行。混合历史样例的R1文件是**早期候选阶段导出文件，
作为历史资料被批准基线保留**，不是一个实际运行的批准旧生产版本。

补充独立命令 `python scripts/diag-integration/check_aito_controls.py --aito <批准Aito目录> --node <Node24> --output <新目录>` 退出0：
[两项实际检查](DIAG-07-aito-controls/checks.json)验证Aito伪流交付87+13=100、dispatch握手后关闭DEBUG产生interrupted。
`python scripts/diag-integration/test_checker.py` 退出0，两项属于component；其中客户端wire fixture明确为synthetic，不是CPA live。

## 未完成门槛和下一步

六实例同时运行、CPA→gcli双边边/跨实例嵌套429/503、原生非流式tokens/rate主导的真实限速全程、
跨服务开关前后请求/响应帧/发送顺序/代理/背压/取消/既有重试统计完整等价，仍然未完成。
POSIX fork、CGO race、实际历史CPA滚动、gcli shutdown_asyncgens特定迟到时序、Home/bootstrap读错也未完成。
在合规环境中使用原位获准binary运行不带 `--downstream-only` 的完整命令，再执行语义/离线检查；
补齐矩阵中的组件限定与未执行项之后，由协调窗口实际审核代码、测试和Opus5.5整体一致性。

gcli 16MiB/boot硬停、无轮转/目录配额是明确交付限制；近阈值只是疑似。
Aito没有生产HttpBoundary caller，不宣称浏览器HTTP/images/VNC覆盖。
CPA已有Gemini读错后DONE/response.completed的业务行为保留，诊断error和转换结果分开展示。
当前回退仅停用这套独立夹具/撤销支持提交；未改变生产行为，不需要数据或配置回迁。
