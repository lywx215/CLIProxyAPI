# DIAG-00 首次交付报告（历史 HEAD，R1 已退回修订）

日期：2026-09-25。此报告对应首次提交 `583201f29c8b4ef65391bdf605ba9d6a2c12f08b` 和旧摘要；R1 结论 request_changes。当前修订见 DIAG-00-R1-revision.zh-CN.md。下文验证结果仅属于首次交付；未自行调用 Claude、推送或宣布冻结通过。

## 身份与版本

- 任务：DIAG-00；真实任务 ID：`01a0d64a-0519-7fc0-9810-110cd3f6835b`。
- 项目：CLIProxyAPI；worktree：`C:/Users/lywx2/.codex/worktrees/622a/CLIProxyAPI`。
- 分支：`codex/diag-00-contract`。
- 锁定基线：`fde3903689b8c9882d1f0233587fe33e2edfdf77`；开始时 HEAD 相同、工作区干净。
- 本报告随交付提交纳入版本；该提交完整 HEAD 在任务最终回复中报告，协调窗口可用 `git rev-parse HEAD` 复核。文件不嵌入自身提交 SHA，避免自引用。
- 交换契约：`ai-proxy-diagnostics/1`；制品：`1.0.0-rc.1`。
- 契约摘要（SHA-256 of SHA256SUMS bytes）：`31221d21a265eab37572024c96337ecf5e9d4bfe064ecb13d3cafba98d16a435`。
- SHA256SUMS 覆盖 61 个制品文件；自身构成第 62 个契约目录文件。使用 UTF-8 无 BOM、LF、相对 POSIX 路径字典序；Git index 与工作区逐文件字节一致。
- 完整源文档导入摘要记录于 `DIAG-00-inputs.json`。源工作区始终只读；此分支台账为协调通知的状态快照。

## 交付与决策

完整方案、Claude 历史原文/原方案快照、八任务说明均纳入本分支。历史 Claude 原文与完整快照已逐字比较保留。协调方通知的 DIAG-01 批准提交 `1f65d3ec10830245f22a58691e124c5129f75707` 已写入后续 DIAG-02 基线，仍等待契约审核。

候选规定本地资源身份/每 worker bootId，server/call 两类 span 与独立 attempt 计数；显式本地 serverSpanId；调用方鉴权/匿名别名的 deployment/boot 作用域；精确 origin/端口/路径的 DIAG_PEERS 与失效回退；X-Diag-* 最后替换且保留旧业务头；双边唯一证据、来源与冲突；基础日志和 DEBUG 的封闭字段边界；终局/序号/丢弃完整性；Aitoapi 字段映射。没有业务流程接入点或运行时代码改动，只有规范、schema、fixtures、离线 oracle 和 Git 换行规则。

协调预检查的未来版 traceparent 扩展空格问题已修订：固定前缀不可含空格，未知扩展不解析、允许可打印 ASCII 空格；本地长度/控制字符限制明确标出，并有共享向量。另补了尾部换行、原始重复头、重复 JSON 配置键、头注入边界、未知/零值、纯空白不是有效文本等拒绝/语义用例。

正式冻结仍须协调窗口检查准确 HEAD 后，将**完整修订方案**送 Claude 审核。后续任务消费批准提交＋摘要，任何修改使旧审核失效。

## 测试命令与结果

| 命令/检查 | 结果 |
| --- | --- |
| `python -m pip install --target .diag-00-tools jsonschema==4.26.0` | 退出 0；仅工作区临时验证依赖，完整依赖版本已写 requirements.txt |
| PowerShell `$env:PYTHONPATH=(Join-Path (Get-Location) '.diag-00-tools')` 后 `python contracts/diagnostics/v1/validate.py --write-manifest` | 退出 0；3 schema、45 fixtures/schema cases、9 JSONL 行、213 共享向量通过 |
| 同环境 `python contracts/diagnostics/v1/validate.py` | 退出 0；不写清单，重现相同摘要 |
| 独立 PowerShell `Get-FileHash -Algorithm SHA256` 逐条核对 SHA256SUMS | 退出 0；61/61 文件相同，清单摘要相同 |
| Python 对 62 个契约文件比较 `git show :<path>` 与 `Path(path).read_bytes()` | 退出 0；Git index/工作区精确字节相同 |
| Python 比较本地/只读源 Claude 文档 `## Claude 原始审查结果` 后完整正文 | 退出 0；历史原文和原方案快照完整保留 |
| `go build -o .diag-00-build.exe ./cmd/server` | 退出 0；构建产物在交付前删除 |
| `git diff --cached --check` | 退出 0 |

早期验证发现本机 jsonschema 的可选 date-time 格式验证器未安装，导致非法日历日期被放过；已用 Python 标准库显式格式检查修正并通过负例。没有掩盖失败或把 schema-only 通过当成语义通过。

无 Go 源码变更，因此未运行 gofmt 或无关全仓 Go 单测；执行规定的 server 编译。没有运行三个服务运行时、外网模型或生产验证。

复现可在隔离 Python 环境安装 `contracts/diagnostics/v1/requirements.txt`，再运行 validate.py。临时工具目录/生成脚本/可执行产物不提交。

## 脱敏样例

- `contracts/diagnostics/v1/examples/bilateral-basic.jsonl`：CLIProxyAPI 调用 gcli2api 的双边证据；4 行，最大 1275 UTF-8 字节/行。
- `contracts/diagnostics/v1/examples/debug-87.jsonl`：规范化、attempt usage、最终 deliveredUsage=87、实际限速参数与终局；5 行，最大 1867 字节/行。
- `fixtures/valid/usage-missing.json` 与 `usage-zero.json`：未知和观察到零明确区分。
- `fixtures/invalid/` 的 FORBIDDEN 字符串均为合成哨兵，验证未知/敏感字段拒绝，不是真实凭证或正文。

样例为局部导出：debug-87 展示单 server 的语义序列，未包含其 call 日志；bilateral-basic 未模拟真实网络。两者均不证明运行时联调完整。基础样例没有详细 usage/结构/限速字段，详细样例需要 DEBUG。

## 覆盖、缺口与风险

- 已覆盖契约层合法/非法/重复/未来版本头、入站来源、对端响应 ID、origin/端口/路径/重定向策略、配置上限、资源默认/worker/重启策略、缺父/双边/歧义/冲突、重复导入、调用方作用域、嵌套计数、缺失/零、完整性和 Aitoapi 映射。
- 校验器仅为离线规范 oracle；graph 向量是单边投影，不是 DIAG-06 完整工具。逐边独立降级、全图循环/本地父节点缺口、坏行保留位置、资源限制和导出管理由 DIAG-06 实际实现验证。
- 所有 Go/Python/Node 运行时、真实 HTTP EOF/Close/取消/流式提交、ContextVar/ALS、共享 flush、浏览器回报、多实例进程、代理和背压等价仍待 DIAG-02–07；本次未声称已覆盖协议实现。
- W3C Level 1 2021 与本地保守 profile 已明确：512 字节标准头上限、编码路径不传播、配置重叠整体禁用。实现必须以最终出站 URL 为依据，路径无法可靠观察时报告未接入，不改变业务路由。
- schema 只能约束结构/类型，不能证明 ID 样式字符串没有秘密；字段来源白名单仍需实现审核。旧原始日志不能随诊断包导出。未增加指纹/HMAC、浏览器子 span 或新网络行为。
- 基础访问日志可关闭/失败、出口可截断；full 只代表已声明可观察范围，不是无损保证。多 worker 原子行输出与部署配置仍待本地集成测试。
- DIAG-00 自身尚未经正式协调检查/Claude 审核；摘要及 HEAD 变化必须重审。

## 未执行动作

未复用旧任务、未派发开发任务/子代理、未跨仓库写入、未编辑源台账、未读/用真实凭证、未改数据库/Volume、未调用生产模型、未部署/合并/推送，也未自行调用 Claude。只读 Aitoapi 字段实现用于映射核对；DIAG-01 审核信息来自协调方。

## 完整变更文件清单

- `.gitattributes`
- `CROSS_SERVICE_DIAGNOSTICS_CLAUDE_REVIEW_CN.md`
- `CROSS_SERVICE_DIAGNOSTICS_PLAN_CN.md`
- `CROSS_SERVICE_DIAGNOSTICS_TASKS_CN.md`
- `contracts/diagnostics/v1/README.md`
- `contracts/diagnostics/v1/SHA256SUMS`
- `contracts/diagnostics/v1/VERSION.json`
- `contracts/diagnostics/v1/bundle.schema.json`
- `contracts/diagnostics/v1/examples/bilateral-basic.jsonl`
- `contracts/diagnostics/v1/examples/debug-87.jsonl`
- `contracts/diagnostics/v1/fixtures/index.json`
- `contracts/diagnostics/v1/fixtures/invalid/alias-without-scope.json`
- `contracts/diagnostics/v1/fixtures/invalid/basic-stub-debug-event.json`
- `contracts/diagnostics/v1/fixtures/invalid/basic-usage-leak.json`
- `contracts/diagnostics/v1/fixtures/invalid/boot-not-uuid.json`
- `contracts/diagnostics/v1/fixtures/invalid/debug-not-gated.json`
- `contracts/diagnostics/v1/fixtures/invalid/empty-id.json`
- `contracts/diagnostics/v1/fixtures/invalid/id-final-newline.json`
- `contracts/diagnostics/v1/fixtures/invalid/invalid-timestamp.json`
- `contracts/diagnostics/v1/fixtures/invalid/missing-attempt-scope.json`
- `contracts/diagnostics/v1/fixtures/invalid/missing-call-parent.json`
- `contracts/diagnostics/v1/fixtures/invalid/missing-identity.json`
- `contracts/diagnostics/v1/fixtures/invalid/negative-count.json`
- `contracts/diagnostics/v1/fixtures/invalid/nested-body-leak.json`
- `contracts/diagnostics/v1/fixtures/invalid/peer-without-service.json`
- `contracts/diagnostics/v1/fixtures/invalid/present-usage-is-null.json`
- `contracts/diagnostics/v1/fixtures/invalid/raw-error.json`
- `contracts/diagnostics/v1/fixtures/invalid/raw-usage-body.json`
- `contracts/diagnostics/v1/fixtures/invalid/raw-usage-protocol-mismatch.json`
- `contracts/diagnostics/v1/fixtures/invalid/resource-final-newline.json`
- `contracts/diagnostics/v1/fixtures/invalid/secret-field.json`
- `contracts/diagnostics/v1/fixtures/invalid/span-uppercase.json`
- `contracts/diagnostics/v1/fixtures/invalid/trace-zero.json`
- `contracts/diagnostics/v1/fixtures/invalid/unknown-usage-is-zero.json`
- `contracts/diagnostics/v1/fixtures/invalid/unknown-version.json`
- `contracts/diagnostics/v1/fixtures/invalid/unsafe-integer.json`
- `contracts/diagnostics/v1/fixtures/invalid/wire-status-before-commit.json`
- `contracts/diagnostics/v1/fixtures/invalid/zero-logseq.json`
- `contracts/diagnostics/v1/fixtures/schema-cases.json`
- `contracts/diagnostics/v1/fixtures/valid/call.json`
- `contracts/diagnostics/v1/fixtures/valid/peer-server.json`
- `contracts/diagnostics/v1/fixtures/valid/process.json`
- `contracts/diagnostics/v1/fixtures/valid/request.normalized.json`
- `contracts/diagnostics/v1/fixtures/valid/response.converted.json`
- `contracts/diagnostics/v1/fixtures/valid/server.json`
- `contracts/diagnostics/v1/fixtures/valid/throttle.finished.json`
- `contracts/diagnostics/v1/fixtures/valid/truncated-basic.json`
- `contracts/diagnostics/v1/fixtures/valid/upstream.attempt_finished.json`
- `contracts/diagnostics/v1/fixtures/valid/usage-missing.json`
- `contracts/diagnostics/v1/fixtures/valid/usage-zero.json`
- `contracts/diagnostics/v1/peers.schema.json`
- `contracts/diagnostics/v1/record.schema.json`
- `contracts/diagnostics/v1/requirements.txt`
- `contracts/diagnostics/v1/validate.py`
- `contracts/diagnostics/v1/vectors/README.md`
- `contracts/diagnostics/v1/vectors/aito-mapping.json`
- `contracts/diagnostics/v1/vectors/counts.json`
- `contracts/diagnostics/v1/vectors/coverage.json`
- `contracts/diagnostics/v1/vectors/graph.json`
- `contracts/diagnostics/v1/vectors/headers.json`
- `contracts/diagnostics/v1/vectors/outbound.json`
- `contracts/diagnostics/v1/vectors/peer-response.json`
- `contracts/diagnostics/v1/vectors/peers.json`
- `contracts/diagnostics/v1/vectors/resources.json`
- `contracts/diagnostics/v1/vectors/semantic.json`
- `contracts/diagnostics/v1/vectors/source-scope.json`
- `coordination/diagnostics/20260924/DIAG-00-delivery.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-00-inputs.json`
- `coordination/diagnostics/20260924/DIAG-00.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-01.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-02.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-03.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-04.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-05.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-06.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-07.zh-CN.md`
- `coordination/diagnostics/20260924/manifest.json`
