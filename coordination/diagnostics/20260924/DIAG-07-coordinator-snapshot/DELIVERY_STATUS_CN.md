# 跨项目诊断交付状态

本文件为协调窗口的当前记录，不能替代准确提交的审核回执。

## 已审核并推送的任务分支

下列版本均经协调窗口检查及实际 `claude-opus-5-5` 审核，远程分支
和本批隔离 worktree 的 HEAD 已再次逐一核对一致。主分支没有合并，服务没有部署。

| 任务 | 仓库 | 远程任务分支 | 批准提交 |
|---|---|---|---|
| DIAG-00 | CLIProxyAPI | `codex/diag-00-contract` | `bb291667f7b6bd7a1dab6f9b7f906b5871d1306c` |
| DIAG-01 | gcli2api | `codex/diag-01-antigravity-cleanup` | `1f65d3ec10830245f22a58691e124c5129f75707` |
| DIAG-02 | gcli2api | `codex/diag-02-gcli-diagnostics` | `0b3a07e003ead2ba7a9f7827426c09f8ff996813` |
| DIAG-03 | Aitoapi-custom | `codex/diag-03-aito-diagnostics` | `a2d51383bc91751f23bc0f8927c19ef736593cea` |
| DIAG-04 | CLIProxyAPI | `codex/diag-04-cpa-tracing` | `50d335a18a2495bd2fbbb30ac47d09476a8a14b6` |
| DIAG-05 | CLIProxyAPI | `codex/diag-05-cpa-diagnostics` | `4996e7ae12b2af38f3bdc490eedf32e1887aeed6` |
| DIAG-06 | CLIProxyAPI | `codex/diag-06-log-analyzer` | `9422a853a222aef0dbf67815888c53ef6f1ede77` |

CLIProxyAPI 的 DIAG-06 包含此前契约、追踪和语义诊断提交；gcli2api 的
DIAG-02 包含 DIAG-01 消息清理修复。三个仓库保持各自的 Git 历史。

统一契约：`ai-proxy-diagnostics/1`，制品版本 `1.0.0-rc.1`。
`SHA256SUMS` 摘要：`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。

## 尚未放行的联调任务

DIAG-07 在新的 `codex/diag-07-integration` worktree 中开发。
目前没有批准提交，没有推送，也没有整体验收结论。

Windows Application Control 以 WinError 4551 拒绝运行新构建的 CPA 联调程序。
协调窗口已提出具体环境选择，开发窗口继续当前环境可运行的独立验证。
六实例同时运行、由 token 数主导的 CPA 非流式限速等强制证据仍需完成。
一个完整 Go 回归测试可执行程序也受到相同系统策略拦截，不能记为全套通过。

准确环境阻塞、失败运行和已完成独立场景见 DIAG-07 的工作报告；仅状态码成功
不等于完整语义验收。整体 Opus 5.5 审核将基于实际提交和证据，不沿用单项目结论。

## 使用时必须保留的语义与限制

- 业务请求成功、诊断证据完整是两个独立判断。收到协议 DONE 后未观察到实际
  HTTP EOF 的情况可以同时出现成功的转换记录和本地 incomplete 的尝试记录。
- `87` 本身不是故障判定。gcli 的 OpenAI 输出计数使用 candidate；CPA/Aito
  某些协议包含 reasoning。必须比较各阶段真实 usage，不能重复加 reasoning。
- 调用方身份未知时，相同外部 ID 只扩大查询候选，不证明属于同一调用者。
  `DIAG_PEERS` 配置控制出站传播，不等同于入站身份认证。
- gcli 诊断文件每 boot 上限 16 MiB，达到上限停止写入，当前不提供轮转或目录总量
  保留机制。部署前需要单独规划容量与保留。日志接近上限只是缺失风险提示。
- Windows 下未执行的 POSIX fork、CGO race，以及尚缺失的实际多版本运行证据
  会逐项保留，不能用合成记录替代实际执行声明。

审核原文、意见处理及精确 SHA 以本目录 `manifest.json` 和 `reviews/` 为准。
