# 跨项目诊断开发任务台账

日期：2026-09-25。用户已明确授权八任务计划。本文件是 DIAG-00 交付分支中的规范/状态快照，源工作区台账由协调窗口维护，本任务没有修改它。

## 执行状态

任务管理连接已恢复，旧 Transport closed 阻塞及“等待初始化”已失效。DIAG-00 本批隔离任务已创建，交付状态为**待审核**；DIAG-01 已经协调窗口与 Claude R1 审核并推送准确 HEAD。其余任务仍等待批准的依赖，不能凭草稿启动。

- DIAG-00：`01a0d64a-0519-7fc0-9810-110cd3f6835b`，worktree `C:/Users/lywx2/.codex/worktrees/622a/CLIProxyAPI`，分支 `codex/diag-00-contract`，基线 `fde3903689b8c9882d1f0233587fe33e2edfdf77`。
- DIAG-01：`01a0d64a-5710-7ee2-8412-f1b42d68c7f0`，worktree `C:/Users/lywx2/.codex/worktrees/e63f/gcli2api`，批准并推送 HEAD `1f65d3ec10830245f22a58691e124c5129f75707`。此状态来自协调方通知/只读 disposition，本任务未重复调用 Claude。

## 已确认规则

- 协调窗口只调度、审核、放行；每个任务新建所属项目的 Codex 任务和隔离 worktree，不复用旧窗口。
- 最大并行三个开发任务；依赖必须先经协调窗口和 Claude 审核。
- 每个任务准确 HEAD 通过审核后推送任务分支；不合并 main/master、不部署、不调用生产模型。
- 退回修订使用本批为该任务新建的窗口；不让联调任务跨仓库修改源码。
- 技术范围：通用追踪＋重点链路诊断；详细日志受 DEBUG 门控；gcli2api 不更新面板版本。

## 基线

| 项目 | 远程基线 | 已核对 SHA |
| --- | --- | --- |
| CLIProxyAPI | origin/main | `fde3903689b8c9882d1f0233587fe33e2edfdf77` |
| gcli2api | origin/master | `cb706830a33d9275abac2cca78eadf2c28504a7f` |
| Aitoapi-custom | origin/main | `daeab836eb194d4ff402bc3d645baeba4ccdc96f` |

实际派发前复核 SHA；依赖任务使用审核通过的提交，不静默换成新的主分支。

## 任务状态

| 任务 | 项目 | 依赖 | 状态 | 派发说明 |
| --- | --- | --- | --- | --- |
| DIAG-00：冻结诊断契约与任务规范 | CLIProxyAPI | 无 | 待审核 | [任务说明](coordination/diagnostics/20260924/DIAG-00.zh-CN.md) |
| DIAG-01：修复 Antigravity 消息清理 | gcli2api | 无 | 已审核并推送 | [任务说明](coordination/diagnostics/20260924/DIAG-01.zh-CN.md) |
| DIAG-02：gcli2api 追踪与诊断接入 | gcli2api | DIAG-00, DIAG-01 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-02.zh-CN.md) |
| DIAG-03：Aitoapi 追踪与既有诊断适配 | Aitoapi-custom | DIAG-00 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-03.zh-CN.md) |
| DIAG-04：CLIProxyAPI 通用追踪基础 | CLIProxyAPI | DIAG-00 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-04.zh-CN.md) |
| DIAG-05：CLIProxyAPI 重点链路语义诊断 | CLIProxyAPI | DIAG-04 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-05.zh-CN.md) |
| DIAG-06：离线日志关联工具 | CLIProxyAPI | DIAG-02, DIAG-03, DIAG-05 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-06.zh-CN.md) |
| DIAG-07：跨项目多实例联调与交付清单 | CLIProxyAPI | DIAG-02, DIAG-03, DIAG-06 | 等待依赖审核 | [任务说明](coordination/diagnostics/20260924/DIAG-07.zh-CN.md) |

## 审核、进度与验收

状态流：待依赖 → 可启动 → 开发中 → 待审核 → 退回修订/审核通过 → 已推送 → 联调验收。每次状态改变在本窗口汇报。

每次派发记录项目 ID、任务 ID、worktree、分支、基线；每次提交审核记录 HEAD、契约摘要、测试、Claude 轮次/意见处理及放行 SHA。任何 HEAD 变化均使旧审核失效。未解决且影响验收的 P1/P2 不放行；不采纳意见须说明依据。

最终联调包括所有目标拓扑、每种服务两个实例、跨实例重试/重启/并发、ID 冲突、400 清理、87 token 计数来源、限速、流式取消、日志缺口及包装前后行为等价。新窗口提示中给出具体专项与项目回归。

机器可读状态：[manifest.json](coordination/diagnostics/20260924/manifest.json)。

完整技术方案：[方案](CROSS_SERVICE_DIAGNOSTICS_PLAN_CN.md)。独立审查：[Claude 记录](CROSS_SERVICE_DIAGNOSTICS_CLAUDE_REVIEW_CN.md)。

## 后续派发与准确版本

1. 协调窗口检查 DIAG-00 准确提交及完整修订方案，再调用 Claude；未解决的 P1/P2 不放行。本任务仅交付候选，不能宣布正式冻结。
2. 契约来源用批准 Git SHA＋SHA256SUMS 清单 SHA-256 双重锁定；清单可离线重现，源码 SHA 单独记录避免自引用。
3. DIAG-02 使用已批准 DIAG-01 HEAD `1f65d3ec10830245f22a58691e124c5129f75707` 和批准 DIAG-00 契约；DIAG-03 保持 Aitoapi 锁定主分支 SHA；DIAG-04 从批准 DIAG-00 HEAD 开始。
4. DIAG-05 从批准 DIAG-04，DIAG-06 从批准 DIAG-05 并消费批准 02/03 脱敏资源，DIAG-07 从批准 06 并只读使用批准 02/03 入口。依赖关系保持八任务表所列。
5. 每次真实创建记录 threadId/worktree，不重用旧任务；最多三个并行。退回本批原窗口修订，任何 HEAD 变化重新审核。

候选契约与验证：[README](contracts/diagnostics/v1/README.md)。完整文档与历史审查原文均已纳入本任务分支。任何本地版本文件中的待审核标识不得解释为批准。
