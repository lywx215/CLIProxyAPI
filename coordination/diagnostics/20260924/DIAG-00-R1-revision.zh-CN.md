# DIAG-00 R1 修订交付：待审核

日期：2026-09-25。已按协调方取舍修订全部 R1 意见，尚未获得新 HEAD 的协调/Claude 批准；未推送或启动依赖任务。

## 身份与版本

- 任务：DIAG-00；真实任务 ID：`01a0d64a-0519-7fc0-9810-110cd3f6835b`。
- 项目/worktree：CLIProxyAPI，`C:/Users/lywx2/.codex/worktrees/622a/CLIProxyAPI`。
- 原分支：`codex/diag-00-contract`。
- 初始锁定基线：`fde3903689b8c9882d1f0233587fe33e2edfdf77`。
- 本轮修订父提交/R1 审查对象：`583201f29c8b4ef65391bdf605ba9d6a2c12f08b`。
- 开始时 tracked 工作区干净，仅存在审查时生成的 Python __pycache__；未覆盖其他源码改动。
- 新 HEAD 在本任务最终回复中报告；本报告随新提交纳入版本，不嵌入自身 SHA。
- 契约：`ai-proxy-diagnostics/1`；制品仍为批准前 `1.0.0-rc.1`，修改后需重新审核。
- 新契约 SHA-256（SHA256SUMS 原始字节）：`26904145b3f1713577f41cd4bbd588ffe48b5f2670b91705537e386f1aafd57a`。
- 72 个清单条目，含清单共 73 个契约文件；全部与 Git 暂存区精确字节一致。

## 已修订内容

R1 完整原文与每项取舍见 [disposition](reviews/DIAG-00-R1/disposition.zh-CN.md) 和 [review](reviews/DIAG-00-R1/review.zh-CN.md)。审查模型由协调方确认是 claude-opus-5-5，原结论 request_changes；本开发窗口没有自行调用 Claude。

1. P2-1：头向量以框架解析后的逐字段值为边界；应用不额外 trim、不恢复线缆 OWS。独立解析器负例与真实 HTTP 用例分开，h11 内存解析覆盖 6 个 HTTP 用例。
2. P2-2：未来版本扩展逗号也按本地 profile 拒绝；重复行/合并值均不能接受。Node 的 traceparent/tracestate 都要求 headersDistinct/rawHeaders；512 字节按逐字段可观察值组合计算。
3. P2-3：公共 timing 固定 server_monotonic，只投影可靠的新单调指标，未知 null；旧墙钟/浏览器耗时保留原日志/统计，schema 拒绝混合来源。
4. P2-4：unknown/none accessCapture 和矛盾终局在无已知丢失时优先 unknown；补齐终局序号、截断存根、known-loss 优先级用例。
5. P2-5：先盘点真实写入/复制来源，仅过滤已证实的自动入站复制点，保留显式 extra_headers 能力。取消 providerOwnedTrace 测试魔法输入；用本模块注入时设置的最小本地拥有权标记，重定向先清自己旧头、再既有业务构造、最后按实际发送 URL 重检。非 peer 不继承本模块标准头，也不全局删除提供方业务头。

P3 十项一并处理：删 not_configured、仅 peer 采集响应 IDs、删当前方案新增指纹要求、标题注明历史审核、raw total_tokens/cachedContentTokenCount 专属白名单、终局存根存在但不完整、实例冲突证据边界、最终 request-target、消费仓库 LF 属性、Node tracestate 字段集合。没有改动路由、模型/账号选择、重试、正文、协议转换或统计。

DIAG-02/04 和 Aito 实际出站适配（如存在）必须交付已有头写入/复制清单。协调方确认当前 gcli 可选参数白名单不等于路由传入入站头，因此没有声称已证实当前路由泄漏，也不强制删掉该显式能力。

## 验证命令和结果

| 命令/检查 | 退出结果 |
| --- | --- |
| `python -m pip install --target .diag-00-tools -r contracts/diagnostics/v1/requirements.txt` | 0，工作区临时验证依赖，包含 h11 0.16.0 |
| 设置 PYTHONPATH=.diag-00-tools、PYTHONDONTWRITEBYTECODE=1 后 `python contracts/diagnostics/v1/validate.py --write-manifest` | 0，3 schemas、53 fixtures/schema cases、9 JSONL 行、242 vectors |
| 同环境 `python contracts/diagnostics/v1/validate.py` | 0，同样全部通过且只读复现摘要 |
| 独立 PowerShell Get-FileHash 逐条核验 SHA256SUMS | 0，72/72 条相符 |
| Python 比较 73 个契约文件的 `git show :<path>` 与工作区字节，并检查相对 POSIX 字典序 | 0，精确一致，无依赖 core.autocrlf 的摘要差异 |
| `go build -o .diag-00-build.exe ./cmd/server` | 0，临时二进制交付前删除 |
| `git diff --cached --check` | 0；导入 R1 原文的空白行尾两个空格及末尾换行已规范化 |
| 比较前一提交 Claude 历史原文及原方案快照 | 0，完整正文不变 |
| 比较只读源 R1 评审与提交副本 | 0，181 行正文相同，仅空白行尾和末尾换行规范化 |

没有 Go 业务源码修改；未运行无关的全仓 Go 单测，不需要 gofmt。未执行真实服务/浏览器/多实例联调。本轮 h11 测试仅解析内存中的本地合成 HTTP 报文，不打开 socket、不启动服务；不是 Go/Node 框架验收替代品。

## 脱敏样例与覆盖缺口

样例仍在 `contracts/diagnostics/v1/examples/bilateral-basic.jsonl`、`debug-87.jsonl`；纯合成，基础样例无 detailed usage。新增 fixtures 包含正常 raw token 原值和混时钟/非 peer 响应 ID 的拒绝情况；FORBIDDEN 为合成哨兵。

新共享资源：`vectors/http-ingress.json`、`copy-source.json`、`redirects.json`。覆盖 HTTP OWS、未来头重复/合并、512 字节边界、拥有权保持、跨 origin 和同源路径外重定向、新 provider 写入与重新注入。

仍待后续实现验证：Go/Node 真实框架、多服务异步/取消/EOF/Close、真实 redirect metadata 连续性、最终 URL 观察边界、代理/背压/重试等价、日志队列与多 worker 输出。无可靠观察/拥有权的路径必须不注入并列覆盖缺口；schema 不能替代敏感值来源审计。终局 stub 不能证明成功/verified；full 仅指声明观察范围。

## 未执行动作

没有自行调用 Claude、推送、合并、部署、生产模型调用、真实凭证/数据库/Volume 改动、跨仓库写入、源台账编辑、派发新任务或子代理。仅更新本隔离 worktree 的契约、方案、任务规范、测试 oracle 和审查记录。仍待协调方对新准确 HEAD 检查并调用 Claude 复审。

## 本轮变更文件清单

- `CROSS_SERVICE_DIAGNOSTICS_CLAUDE_REVIEW_CN.md`
- `CROSS_SERVICE_DIAGNOSTICS_PLAN_CN.md`
- `CROSS_SERVICE_DIAGNOSTICS_TASKS_CN.md`
- `contracts/diagnostics/v1/README.md`
- `contracts/diagnostics/v1/SHA256SUMS`
- `contracts/diagnostics/v1/fixtures/index.json`
- `contracts/diagnostics/v1/fixtures/invalid/browser-public-timing.json`
- `contracts/diagnostics/v1/fixtures/invalid/gemini-rejects-openai-total.json`
- `contracts/diagnostics/v1/fixtures/invalid/legacy-browser-duration-in-public-timing.json`
- `contracts/diagnostics/v1/fixtures/invalid/legacy-wall-public-timing.json`
- `contracts/diagnostics/v1/fixtures/invalid/nonpeer-cannot-claim-diag-response-id.json`
- `contracts/diagnostics/v1/fixtures/invalid/undefined-rejection-reason.json`
- `contracts/diagnostics/v1/fixtures/valid/gemini-cached-token-zero.json`
- `contracts/diagnostics/v1/fixtures/valid/openai-raw-total-tokens.json`
- `contracts/diagnostics/v1/record.schema.json`
- `contracts/diagnostics/v1/requirements.txt`
- `contracts/diagnostics/v1/validate.py`
- `contracts/diagnostics/v1/vectors/README.md`
- `contracts/diagnostics/v1/vectors/aito-mapping.json`
- `contracts/diagnostics/v1/vectors/copy-source.json`
- `contracts/diagnostics/v1/vectors/coverage.json`
- `contracts/diagnostics/v1/vectors/graph.json`
- `contracts/diagnostics/v1/vectors/headers.json`
- `contracts/diagnostics/v1/vectors/http-ingress.json`
- `contracts/diagnostics/v1/vectors/outbound.json`
- `contracts/diagnostics/v1/vectors/peer-response.json`
- `contracts/diagnostics/v1/vectors/peers.json`
- `contracts/diagnostics/v1/vectors/redirects.json`
- `coordination/diagnostics/20260924/DIAG-00-R1-revision.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-00-delivery.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-00.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-01.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-02.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-03.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-04.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-05.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-06.zh-CN.md`
- `coordination/diagnostics/20260924/DIAG-07.zh-CN.md`
- `coordination/diagnostics/20260924/manifest.json`
- `coordination/diagnostics/20260924/reviews/DIAG-00-R1/disposition.zh-CN.md`
- `coordination/diagnostics/20260924/reviews/DIAG-00-R1/review.zh-CN.md`
