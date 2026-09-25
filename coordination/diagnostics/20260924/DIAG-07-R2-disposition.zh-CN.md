# DIAG-07 R2 修订说明

**代码候选待独立复审；整体联调维持 hold。** 本轮只修改 DIAG-07 Python 支持脚本、说明与新增证据，没有推送、合并、部署、调用审核模型或委派代理。上一候选为 `241a2cb4079488bf682bd23745e354944095c04a`；新提交 SHA 随交付消息给出。

## R1 P2-1 处理

原问题已复现：[旧检查器记录](DIAG-07-R2-validation/old-checker-reproduction.json)显示 run-06 的340项断言全部通过、双边断言0项；含已知丢失的分析verified=0。此结果不证明完整CPA链路。

修订后的runner保存两个明确范围：

- `analysis.json`：排除声明knownLoss的导出，仅验证所列正常来源子集。
- `analysis-known-loss.json`：包含全部导出，传入真实known-loss声明。冻结分析器继续给所有边加`export_known_loss`并拒绝verified。
- `analysis-scopes.json`：两次分析的来源别名、精确内容hash、信任声明、排除项、退出码和范围限定。正常子集不得称为全局采集完整。

每个CPA请求保存独立于诊断日志的实际配置peer map。`bilateral.py`逐请求检查owner、实际call非空、声明callCount，并逐call检查configured peer、service/deployment/alias、receiver instance/environment、同trace/parent、唯一remote边、响应peerRequestId/peerTraceId（出现时）及分析器verified。未分配到受控请求的CPA call也失败。完整模式要求gcli2api/aitoapi都存在实际verified证据，并要求gcli1/gcli2/aito1/aito2四目标；零CPA或只有Aito不能通过。

业务错误与图关联分开：HTTP错误、空输出、截断等场景仍按实际双边记录核验。取消只有在调用方确已取消、配置策略为cancellation且实际边精确呈现`missing_peer`或`terminal_evidence_incomplete`时允许对应缺口；不把该缺口标成verified。歧义、身份冲突、不可信来源、known-loss不在例外内。取消若有完整双边证据仍可verified，不能因此声称业务成功或完整DEBUG覆盖。

离线检查不再写死4条边。期望来自受控请求、独立peer配置与实际call；默认要求两个服务，历史smoke必须显式指定Aito子集。新运行的known-loss来源由manifest自动识别；历史run-06需明确追加`--live-known-loss aito4`，不改旧manifest。

## 回归结果与证据等级

| 检查 | 结果 | 证据和限制 |
|---|---|---|
| 旧缺陷复现 | 340通过，0双边断言 | [复现](DIAG-07-R2-validation/old-checker-reproduction.json)，未启动服务 |
| 范围隔离/双边正负向 | 21项通过 | [replay-03](DIAG-07-R2-validation/replay-03/checks.json)；smoke/run-06真实导出重放，加明确标注的合成变换 |
| 正常来源子集 | 早期smoke的4条真实CPA→Aito边通过逐call核验 | [逐项断言](DIAG-07-R2-validation/replay-03/normal-bilateral.json)、[来源范围](DIAG-07-R2-validation/replay-03/analysis-scopes.json) |
| 全来源known-loss | 0 verified，所有边保留export_known_loss | [全量报告](DIAG-07-R2-validation/replay-03/analysis-known-loss.json) |
| 既有语义 | 原340项均保留通过；新增5项后345通过 | [下游重放](DIAG-07-R2-validation/downstream-replay/semantic-checks.json)；完整模式对此输入产生6项预期拒绝 |
| 离线检查 | 10项通过 | [offline-02](DIAG-07-R2-validation/offline-02/checks.json)，显式保留run-06的aito4 known-loss |
| pair自带known-loss清单 | 11项通过 | [offline-03](DIAG-07-R2-validation/offline-03-loss-manifest/checks.json)，临时组合smoke与历史aito4，验证离线正常子集和全量报告同时保留 |
| Python checker回归 | 4项通过 | [最终验证](DIAG-07-R2-validation/final-validation.json)，含状态码不同/正文不同的等价性拒绝 |
| Go源码/依赖 | 相对241a2cb完全不变 | [核验](DIAG-07-R2-validation/validation.json)；按协调补充复用原[完整Go回归exit0](DIAG-07-validation/full-go-test-02.exit.txt)并记录输出hash |
| analyzer构建来源 | 构建exit0；新旧analyzer逐字节SHA一致 | [构建](DIAG-07-R2-validation/analyzer-build.json)、[二进制身份](DIAG-07-R2-validation/binary-identities.json)；仅构建冻结分析器 |

21项包含缺receiver、缺receiver terminal、重复parent、不可信来源、peer request/trace/service/deployment错误、未知alias/已知alias指向错误instance、可选peer ID缺省、取消完整证据与精确缺口、全量known-loss、零CPA/只有Aito拒绝，以及2条call子集的动态期望。两服务正向分支使用**明确合成的service标签变换**，只是检查器分支测试，绝不是CPA→gcli live证据。早期replay-01/02及offline-01保留，最终依据replay-03/offline-02；早期offline-01未补历史manifest的known-loss标志，不能据其声称全量验证。

## P3与证据疑点

| R1条目 | 本轮处理 |
|---|---|
| DEBUG on/off等价性 | 同时要求HTTP status相等和去除id/created后的JSON相等；保留独立范围，不声称帧/顺序/背压/统计全面等价 |
| throttle帮助/exit0 | 不改受阻main.go；README明确旧help文字与实际rate/first-delay参数区别，exit0在所有环境均因硬编码缺口而不可达 |
| 二进制构建身份 | 必填hash绑定build provenance；分别记录运行时HEAD、支持文件hash、实际driver/analyzer hash、Go嵌入元数据、构建来源声明；不将旧binary绑定当前HEAD |
| trackedProductionClean | README明确只覆盖tracked diff，不代表未跟踪文件清洁 |
| UTF-8 | 所有read_text显式encoding='utf-8'，最终AST核验包含该条件 |
| CPA复用caller ID | checker将实际CPA头矩阵实例纳入reused-id-candidates检查；尚无该矩阵live证据 |
| blocked记录hash差异 | 原记录字节不变，增加[旁注](DIAG-07-validation/blocked-binary-note.md)：只有main.go对应binary，脚本为当时快照 |
| 跨服务call debugCapture | 只写DIAG-07 README；CPA none / gcli enabled_throughout不能直接横比；未修改DIAG-06/生产文档 |
| 硬杀活动性 | 代码注释、新场景名、交付/矩阵均降为“kill前观察dispatch计数”；不证明kill瞬间响应活动或缺失flush时序。旧run中的场景ID只作历史标识 |
| SetLogLevel疑点 | 既有`internal/util/util.go:60-76`在logrus降级前调用NotifyDebugDisabled；`internal/diagnostics/semantic.go:8-19`的epoch保存两次观察之间off→on。见[原始源码摘录及hash](DIAG-07-R2-validation/source-evidence.json)，不改生产文件 |

driver仍是`da12f45f2719a4e199eb160036caa6dc94eda4bc0cc3fdf40cf1fa5c4fb7c71f`，main.go仍是`393abcdcaab6af5b449d2777debd180dbee49a70bb4cb9caee878bc49e3842e0`。本轮没有执行、重建、改名或规避该受阻对象。analyzer为`9b7e75e50d7f980bd8f16bf7f9c351dc5fcc9d140977df8eba621e23f69399ef`，重新构建的是独立分析器，且其hash与旧分析器相同。

CPA nested的2次call/attempt与{429,503}、cpa2 empty/thought的502、限速89/provider_output等依旧是**未经本轮live验证的期望**。首次合规运行若不符，应调查协议/配置/证据后判断期望，不以改断言迎合结果。

## 后续边界

本轮未启动任何服务进程。六实例同时存活、CPA→gcli所有链路、CPA头矩阵与外层重试、CPA live限速及动态DEBUG、跨服务完整等价、POSIX fork/CGO race/真实旧版滚动/shutdown_asyncgens专项/Home/bootstrap/Aito媒体/实际16MiB等缺口维持原矩阵。代码复审通过也不构成整体验收。仅在获授权的合规环境执行真实完整runner后，才可补这些证据。

重放命令示例（新输出目录）：

```powershell
$env:PYTHONDONTWRITEBYTECODE = '1'
python scripts/diag-integration/replay_r1.py --analyzer <recorded-analyzer> --output <new-replay-directory>
python scripts/diag-integration/check_offline.py --analyzer <recorded-analyzer> --live coordination/diagnostics/20260924/DIAG-07-run-06 --live-known-loss aito4 --pair coordination/diagnostics/20260924/DIAG-07-aito-smoke --peer-plan <new-replay-directory>/historical-peer-plan.json --required-service aitoapi --output <new-offline-directory>
python scripts/diag-integration/test_checker.py
```

原失败轮次、早期证据、run-06源码快照及原清单均保留；R2新增制品单独索引于[本轮清单](DIAG-07-R2-validation/submission-manifest.json)。
