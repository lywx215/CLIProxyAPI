# Gemini AI Studio 免费层配额管理实现与运维交接

本文记录本次后端与管理前端实现的行为、首次启用步骤和验证清单，供代码审核及后续部署使用。设计依据为 `docs/AI_STUDIO_FREE_TIER_POLICY_REVIEW_CN.md` 与任务中给出的七项评审修订约束。生产目标是 `https://cpa-p3.zeabur.app`；本文中的配置和 HTTP 示例不表示已经部署或修改该服务。

## 1. 管理范围与额度含义

策略默认关闭，只管理使用 API Key 直连官方 `https://generativelanguage.googleapis.com` 的 Gemini 生成请求，包括标准生成、流式生成和官方 Interactions 生成路径。OAuth 渠道、自定义 Gemini 上游、未列入 `managed-models` 的模型及 `countTokens` 不扣本策略的日预算。自定义渠道是否使用相同模型别名不改变这一范围判断。

策略开启时，未列入日预算表的官方 Google 生成模型仍受每个客户端请求三次物理发送上限约束。`countTokens` 不计入生成发送数，但其 Google 错误也会脱敏。

本地预算按“官方源站 + 配额项目身份 + 真实上游模型 + 太平洋日期”计算。Google 的限制按项目共享，RPD 在太平洋午夜重置；RPM、TPM、RPD 任意一项达到限制都可能返回限流错误。各项目的实际限额应查看 AI Studio，公开限制也不保证实际容量。[Google 官方限流说明](https://ai.google.dev/gemini-api/docs/rate-limits)

本次 Flash 的 20 次、Lite 的 500 次是用户指定的本地日预算，不能当作所有 Google 项目都享有的官方固定额度。配置模型名单也不保证项目具有该模型权限。策略不自动查询 Google 剩余额度；其他工具直接使用同一项目产生的消耗只能通过已有用量估算或上游错误反映。

当前配置公开的是 RPD 上限。RPM、TPM 通过 Google 错误反馈触发短期冷却，没有对外提供精确的分钟请求数或 token 预扣配置。每个项目与真实模型默认最多一个在途请求。

## 2. 七项硬约束对应实现

| 约束 | 实现位置 | 实际行为 |
| --- | --- | --- |
| 发送后的额度绝不退款 | `internal/runtime/budget/ticket.go`、`internal/runtime/executor/helps/gemini_budget.go` | Reserve 写入 WAL 并同步落盘后才允许进入传输。调用底层 RoundTrip 前设置单向原子发送标志；该标志设置后，即使取消、断网、读取失败、503 或 429，也只释放在途槽位，绝不补偿日额度。真正发送前的失败才允许以 `target_attempt_id` 幂等补偿。 |
| COS 每天最多一次预算 PUT 尝试 | `internal/runtime/budgetbackup/backup.go`、账本的 `ClaimBackup` | 独立 S3 Client 使用 `aws.NopRetryer`，发送前同步写入当日标记。传输层再限制一次 PUT，关闭重定向和透明连接重试；失败或结果不确定同样消耗当天机会。 |
| 客户端错误脱敏 | `internal/runtime/executor/helps/gemini_budget.go` | 内部解析 Google 限额、原因与重试提示，再用通用错误和客户端请求别名替换非 2xx 响应或 SSE 错误帧。原始错误正文不向客户端透传。 |
| 别名与 Thinking 使用同一预算模型 | `sdk/cliproxy/auth/gemini_budget.go` | Conductor 选择和执行器预留统一规范化模型名，解开配置别名、前缀及 Thinking 后缀，使用真实上游模型的预算桶。 |
| 单次请求最多三次受管发送 | `internal/runtime/budget/ticket.go`、Conductor 执行及选择路径 | 同一客户端 Context 持有共享计数器，发网前原子检查上限；已尝试的项目也从后续选择中排除，外层重试轮数不能放大实际发送次数。流式输出后发生错误不会重新生成。 |
| 显式初始化与卷丢失分离 | `internal/api/handlers/management/gemini_budget.go`、`internal/runtime/budget/engine.go` | 首次初始化要求策略关闭、Revision 匹配和管理员确认；写入签名 `init.meta`。策略已经开启但账本缺失或损坏时进入灾难态，当日受管模型暂停，COS 写入冻结，次日太平洋午夜后才恢复。 |
| 滚动部署单写者接管 | `internal/runtime/budget/lock*.go` | Unix 使用内核 `flock`，Windows 使用 `LockFileEx`；打开账本最长等待 30 秒，让旧进程释放锁后再接管。超时不能绕过锁建立第二个账本写者。 |

发送标志在底层传输开始前保守设置，所以 DNS、连接或 TLS 阶段失败也可能计入本地预算。这一选择避免“已经发出，但调用方认为没发出”造成重复发放额度。

## 3. 配置示例

以下策略位于 V8 配置根节点，首次设置时保持 `enabled: false`。启用前先完成第 5 节的初始化流程。

```yaml
config-version: 8

gemini-free-tier-policy:
  enabled: false
  storage-path: "/data/gemini-budget"
  cos-backup:
    enabled: false
    schedule: "00:10"
    object-key: "quota/gemini-budget/latest.json"
  managed-models:
    gemini-2.5-flash: 20
    gemini-3.5-flash: 20
    gemini-3.6-flash: 20
    gemini-3.7-flash: 20
    gemini-3.8-flash: 20
    gemini-2.5-flash-lite: 500
    gemini-3.1-flash-lite: 500
    gemini-3.1-flash-lite-preview: 500
    gemini-3.5-flash-lite: 500
```

省略 `managed-models` 时采用以上默认名单；开启策略时不允许显式空名单。模型键必须为无前缀、无 Thinking 后缀的规范小写真实名称。上限必须为正整数，最大 1,000,000。备份时间使用 `America/Los_Angeles`，不使用容器默认时区；对象键必须为独立相对路径。

`storage-path` 应在首次初始化前确定。运行中修改已打开账本的目录会暂停受管请求，要求重启后打开新路径；不能通过热更新悄悄切换到另一个额度池。修改模型上限不会清除已经记录的用量。

### 配额项目与管理界面分组

`quota-project` 属于单条 Gemini Key 的可选字段。共享同一 Google 项目的 Key 必须设置相同值，即使它们在界面中属于不同组。一个界面组也可以包含多个 Google 项目；组名、模型继承和轮询权重不决定预算身份。

```yaml
api-keys:
  gemini:
    - name: ais-shared-config
      base-url: "https://generativelanguage.googleapis.com"
      models:
        - name: "gemini-3.1-flash-lite-preview"
          alias: "gemini-2.5-pro"
      keys:
        - api-key: "REPLACE_WITH_FIRST_KEY"
          quota-project: "ais-project-a"
        - api-key: "REPLACE_WITH_SECOND_KEY"
          quota-project: "ais-project-a"
        - api-key: "REPLACE_WITH_THIRD_KEY"
          quota-project: "ais-project-b"
```

项目标签在账本及监控中使用哈希身份，实际 Key 不写入预算快照。未提供 `quota-project` 时，使用凭据哈希作为本地池身份；程序无法据此推断不同 Key 是否属于同一 Google 项目。重复使用相同凭据不会增加独立预算池。

应在第一次初始化前核对项目归属。策略开启后改变 `quota-project` 会改变预算桶身份，当前没有用于转移当日用量的管理 API。不得把改标签当作恢复额度的手段。普通分组整理或前缀变更不改变凭据隔离身份；已确认无效的凭据不能靠调整界面分组解除隔离。

示例中的客户端请求 `gemini-2.5-pro` 实际使用 Lite 的 500 次桶；`:low`、`:thinking` 等变体及原生 Thinking 后缀也回到相同真实模型桶。管理端监控为了运维展示真实模型名称，客户端生成返回沿用请求别名。

## 4. 本地权威账本与运行条件

生产服务必须给 `/data/gemini-budget` 挂载可持续保留的本地持久卷。至少保留以下文件作为一个完整集合：

| 文件 | 用途 |
| --- | --- |
| `writer.lock` | 内核排他锁的锁定对象；文件存在本身不代表仍有进程持锁。 |
| `init.meta` | 带签名的初始化与灾难恢复元数据。 |
| `init.secret` | 验证元数据签名的本地秘密；不要放入 Git、日志或公开报告。 |
| `ledger.wal` | 额度、冷却、隔离、熔断、备份尝试等事件，以及原子替换的检查点。 |

WAL 使用 `GBW1`、事件类型、序列号和 CRC。规定的基础事件为 Reserve `0x01`、Compensate `0x02`、UpstreamExhausted `0x03`、Cooldown `0x04`，另有初始化、恢复、隔离、备份及熔断等内部事件。预留必须 `fsync` 成功后才能发送；落盘失败采用暂停准入。检查点以临时文件同步和原子替换完成，Unix 同步目录，Windows 使用写穿透替换。

重启回放会保留已占用日额度和备份尝试标记，旧进程的在途槽位释放，但旧预留不能在新进程退款。末尾未完成的 WAL 帧可以截断；完整帧 CRC 或序列损坏进入灾难恢复。

同一预算账本必须只有一个活动写者。`flock` 只能协调访问同一个文件系统锁对象的进程，不能协调各自拥有独立卷的多副本，也不能替代分布式一致性。Zeabur 滚动部署应保证新旧实例访问同一持久卷，并在 30 秒锁等待窗口内让旧实例优雅退出。

卷丢失或签名/WAL 损坏时，当日受管请求保守返回 503，预算 COS 写入同时冻结。恢复信息落盘以避免同一天反复重启恢复出新额度。下一次太平洋午夜后，准入操作恢复新一天预算；不会重发丢失当天的额度。COS 快照不是当前日的精确额度权威来源，现有管理 API也没有“一键从 COS 重置账本”入口。

## 5. 首次启用步骤

1. 确认真实持久卷挂载、单写者运行条件和项目归属，备份现有配置。保留现有 Key、组、权重和模型映射。
2. 保存策略配置，保持 `enabled: false`。根据 AI Studio 实际项目限额确认本地 RPD 值；按需要设置各 Key 的 `quota-project`。
3. 在管理界面的 Quota 页面查看 Gemini 免费层状态，或请求下述 GET。只有 `can_initialize: true` 时进入首次初始化。
4. 阅读首次日历史用量可能不完整的提示，明确确认后以同一 Revision 提交一次初始化 POST。
5. 检查初始化结果 `initialized: true`、策略仍关闭，以及持久目录中的签名元数据和 WAL。确认本地或上游工具产生的历史用量没有被错误当作精确剩余额度。
6. 使用已有 V8 配置保存流程把 `enabled` 改为 `true`，其余 Key、组和模型配置保持原值。再次刷新状态，确认账本可用并核对路由。

初始化不会自动开启策略，不会保存 Key 配置，也不会把相同初始化接口变成“每日重置”接口。已经初始化、灾难态、策略已开启或缺少历史未知用量确认时，初始化拒绝执行。

### 管理 API 合约

沿用管理鉴权，管理 Key 与客户端模型 Key 不混用。以下只展示请求格式；真实密钥应由现有安全凭据机制放入请求头，不应写入命令参数、报告或日志。

```http
GET /v8/management/quota/gemini-budget
Authorization: Bearer <MANAGEMENT_KEY_FROM_SECURE_MEMORY>
```

返回包括配置 `revision`、`enabled`、`engine_state`、`initialized`、`can_initialize`、太平洋日期、下次重置时间、项目与模型计数，以及最近备份状态。`project_id` 为哈希；`used` 包含不可退款的发送预留，`inflight` 单独显示当前在途数。接口设置 `Cache-Control: no-store`，不会发起 Google 探测或 COS 写入。

```http
POST /v8/management/quota/gemini-budget/init
Authorization: Bearer <MANAGEMENT_KEY_FROM_SECURE_MEMORY>
Content-Type: application/json
If-Match: "<REVISION_FROM_GET>"

{
  "revision": "<REVISION_FROM_GET>",
  "acknowledge_unknown_usage": true
}
```

请求体 Revision 与 `If-Match` 都必须匹配当前配置。冲突返回 409；重新读取状态、核对影响并重新确认，不能自动覆盖或自动重放初始化。保存成功也不等同于运行时已经开启策略，应以刷新后的状态核对。

初始化会导入当前可见用量记录中属于当日、受管项目与模型的估算。首日显示估算标记；已有记录缺失、外部调用或原有记录无法关联到当前凭据时，程序无法补齐真实历史用量。最稳妥的首次启用窗口是项目日额度刚重置且尚未发生其他调用的时间。

## 6. 错误与自动停用行为

| 情况 | 本地处理 | 恢复条件 |
| --- | --- | --- |
| 本地日预算达到上限 | 从选择中排除对应项目与真实模型，阻止继续发送。 | 下一太平洋日；上游更长重试提示会继续生效。 |
| Google 明确 RPD 耗尽 | 锁定项目与模型当日预算，不影响该项目的其他模型。 | 次日午夜与上游提示中较晚的恢复时间。 |
| RPM、TPM 或无法确认类型的 429 | 按项目与模型短期冷却；采用上游提示与退避中较长者。 | 冷却结束；成功完整响应可以清除短期冷却。 |
| 503 | 该项目与模型短期冷却；30 秒内三个不同项目发生 503 时开启同源同模型 60 秒熔断。 | 熔断后允许一个半开探测；完整成功响应恢复。 |
| 明确凭据无效 | 持久隔离对应凭据，稳定身份不依赖界面组或前缀。 | 运维核实并更换有效凭据；不自动按日解除。 |
| 明确模型不可用 | 隔离该凭据的对应真实模型。 | 核对模型权限和配置；不误伤其他模型。 |
| 缺少明确无效原因的 401/403 | 短期冷却，不直接永久隔离全部模型。 | 冷却结束。 |
| 账本不可用、卷丢失、锁获取失败 | 受管路径采用 503 暂停准入。 | 修复存储或完成安全恢复。 |

短期退避基础为 60、120、240、480、900 秒，加上最多 10% 的正向抖动；Google 更长提示优先。策略不删除 API Key，也不通过修改每条 Key 的启停字段实现每日熔断。共享项目与真实模型预算由账本控制，因此 Flash 耗尽不会把 Lite 一同停用。

传输之后的失败仍扣额度，界面“已使用”不能解释成成功生成次数。每个受管客户端请求最多发往三个不同项目，后续外层重试不能重新获得发送次数。已交付流式数据后出现网络中断，返回当前错误并结束流，不换 Key 重生成。

## 7. COS 每日一次预算备份

预算备份独立于普通配置或 OAuth 对象存储同步，使用以下已有连接环境变量：`OBJECTSTORE_ENDPOINT`、`OBJECTSTORE_BUCKET`、`OBJECTSTORE_ACCESS_KEY`、`OBJECTSTORE_SECRET_KEY`，可选 `OBJECTSTORE_REGION`、`OBJECTSTORE_PREFIX`。凭据仅用于独立 S3 Client，不进入预算快照或公开状态。

启用 `cos-backup.enabled` 后，仅在未来的太平洋时间 `schedule` 执行；默认 00:10。启动时不立即上传，错过执行时间不补传，停止时不上传，模型请求结束和监控刷新均不上传。显式首次初始化可以使用当天第一次备份机会。

只有账本存在未备份变化时才尝试备份。尝试之前把当日标记写入本地 WAL 并 `fsync`，一天最多一次预算 HTTP PUT。500、超时、取消、未知结果、重启和重复调用都不能触发第二次 PUT；应等待下一太平洋日。未知结果如需检查，只允许人工或独立只读 HEAD/GET，当前没有自动结果核验任务。

若一次上传跨过太平洋午夜，物理写入前还会占用新一天的标记，保守消耗两天机会，避免跨日操作与新日上传重叠。备份失败或对象存储未配置不影响健康本地账本的配额权威性。灾难冻结期间禁止预算上传，以免用不可信状态覆盖原有快照。

“每日一次”承诺限定于该预算对象。现有其他对象存储功能仍属于各自流程；本策略避免把受管额度和冷却变化送进普通凭据高频同步路径，但不能据此宣称整台服务所有 COS 对象总计只写一次。

## 8. 管理前端行为

Quota 页面新增 Gemini 免费层状态卡片，显示项目哈希、Key 数量、真实模型、已预留/上限、剩余、在途、状态、恢复时间及最近备份尝试。一个项目内有可用的同胞 Key 时，不因另一条 Key 被隔离而把整个项目显示为不可用。

读取状态由页面加载、现有页面刷新或手动刷新触发，不建立后台配额轮询，也不发送模型探测。首次初始化按钮仅在策略关闭且后端明确允许时显示；确认后提交一次 POST。冲突刷新状态并要求重新确认，不自动重试。切换管理连接后，旧连接的状态和待发请求受到连接代际检查，不能把旧服务的确认发到新服务。

## 9. 验证清单与发布条件

下面列出需要核对的验收项；实际执行结果由最终测试记录补充，不能把清单当作全部已通过的证明。

- [ ] Flash 仅剩 1 次，20 个并发竞争者只允许 1 次真实发送。
- [ ] Header 写出后断网，WAL 没有 Compensate；重启后额度仍占用。
- [ ] 别名、`:low`、`:thinking` 与原生 Thinking 后缀归入相同真实模型桶。
- [ ] Flash 耗尽时 Lite 可继续选择和生成。
- [ ] 同项目多 Key 共用预算，已经尝试项目不会在同一请求换 Key 重试。
- [ ] 外层重试配置高于三次仍不超过三次受管物理发送。
- [ ] 流式交付首块之后断网，不换 Key 重新生成。
- [ ] Google 429 非流式和多行 SSE 错误不含项目号、真实模型名或密钥。
- [ ] COS 返回 500、取消或重定向时，只出现一次 PUT；同日重启和重复调用为零次新增 PUT。
- [ ] 启动、停止、错过 00:10、未变化账本和灾难冻结不触发预算上传；夏令时按太平洋日期运行。
- [ ] 首次显式初始化匹配 Revision，冲突返回 409，初始化后策略仍关闭，重复初始化不能重置。
- [ ] 完整 WAL 回放、尾帧截断、CRC 损坏灾难态、次日恢复和排他锁接管均符合保守准入。
- [ ] 已隔离凭据不能通过分组或前缀变更恢复；项目状态保留其他健康 Key 的可用性。
- [ ] Go 格式、相关单元/联动测试及 `go build -o test-output ./cmd/server` 通过。
- [ ] 前端 `bun run verify` 通过；桌面和移动端初始化、冲突、只读刷新联调通过。

生产发布前还应确认：Zeabur 持久卷跨滚动部署保留且新旧实例共享锁对象；Google 各项目实际免费层额度与模型权限；COS 环境变量、对象权限和真实网络下单次 PUT 行为。本节为上线前检查清单；已完成的生产验证见第 13 节及生产验收记录。真实 Google 请求会消耗额度；若首次初始化触发真实 COS 备份，将占用当天上传机会，应在获准生产验证时计入计划。

## 10. 代码导航

| 区域 | 主要文件 |
| --- | --- |
| 策略及 V8 配置校验 | `internal/config/gemini_free_tier.go`、`internal/config/config.go`、`internal/config/config_v8.go` |
| 单 Key 项目标识 | `internal/config/config_types.go`、`internal/watcher/synthesizer/config.go` |
| 本地账本、传输屏障、回放 | `internal/runtime/budget/engine.go`、`ticket.go`、`wal.go`、`lock_unix.go`、`lock_windows.go` |
| 预算模型与项目身份 | `sdk/cliproxy/auth/gemini_budget.go` |
| Conductor 选择与重试控制 | `sdk/cliproxy/auth/conductor_selection.go`、`conductor_execution.go`、`conductor_stream.go` |
| Gemini 请求与错误过滤 | `internal/runtime/executor/gemini_executor.go`、`internal/runtime/executor/helps/gemini_budget.go` |
| 独立 COS 备份与生命周期 | `internal/runtime/budgetbackup/backup.go`、`sdk/cliproxy/service_gemini_budget.go` |
| V8 监控与显式初始化 | `internal/api/server_management_v8.go`、`internal/api/handlers/management/gemini_budget.go` |
| 管理前端 | 伴随仓库中的 `src/services/api/geminiBudget.ts`、`src/features/quota/components/GeminiBudgetSection.tsx`、`src/features/quota/geminiBudgetView.ts` |

## 11. 本轮实际验证结果（2026-10-12）

- 账本、独立备份、传输辅助、配置、Auth 调度和 v8 配额管理测试通过，包含并发最后一次额度争抢、传输后断网不退款、WAL 重放、模型隔离、日期/DST 和每日上传标记恢复。
- 真实 Conductor 与模拟 Google 传输联动通过：八条 Key、外层十轮重试仍只发送三次；未列入预算表的模型同样受限。Flash 耗尽后 Lite 可调用；别名和 Thinking 变体使用真实模型桶；流式首块后断网不重新生成。
- Windows 后端 `go build -o test-output ./cmd/server` 和 Linux amd64 交叉编译通过。已执行全库 `gofmt -w .`；后续修改定向格式化。
- 前端 `bun run verify` 通过：1,742 个测试，以及 lint、TypeScript 和 Vite 构建。前后端管理页面 SHA256 一致：`2CB32768EB83B401BB80CBADC37A5CC9623C6CCD63EA6EEB7865ED4374C30162`。
- 本地实际后端与浏览器联调通过：过期 Revision 返回 409；界面初始化只发送一次 POST；初始化不启用策略、不写配置、不消耗模型额度或上传 COS；移动端没有横向溢出。
- 使用最终构建重启本地进程后，初始化身份保留，账本不能重置；V8 策略启停返回 200，无效日额度返回现有接口约定的 422 且文件不变，重复初始化返回 409。本地合成配置最终恢复为关闭策略。
- API、服务生命周期、相关 SDK handler 和 watcher 测试通过。完整执行器套件除下述原有偶发诊断测试外通过；该诊断矩阵及 Gemini 诊断等价测试单独运行也通过。

验证限制：完整执行器套件中的 `TestDIAG05RealSemanticBoundaries` 在 Antigravity 流式诊断事件上偶发失败。使用 Git HEAD 中原始 Gemini 执行器通过只读编译 Overlay 重复运行，复现相同失败；当前没有将其记为全部通过。Windows 的 Application Control 偶尔阻止临时测试程序，相关配额、API、管理测试已改为工作区编译后运行。当前主机无启用 CGO 的 C 编译器，未运行 Go race detector。Linux 已验证编译，尚未验证真实 Zeabur 持久卷与滚动部署、生产 Google 或生产 COS；本轮无生产配置修改或部署。

本地验证产物保存在 `C:/Users/lywx2/.codex/visualizations/2026/10/12/01a12e4b-a19b-7ac1-8925-e6314a7ef8e2/`：`gemini-budget-actual-backend-qa.json`、`gemini-budget-restart-qa.json` 及桌面/移动端截图。验收清单中的发布环境项目仍需在部署时逐项确认。

## 12. 生产上线前追加检查（2026-10-12）

后续部署任务已完成此前缺失的 Go 竞态检查：预算账本、独立 COS 备份、执行器辅助与 Auth 全包，以及最终 GeminiBudget Conductor 和 V8 管理集成均通过。使用官方 portable GCC，下载摘要匹配 Release 元数据，仅临时进程启用 CGO，没有修改系统配置。证据保存在 `C:/Users/lywx2/AppData/Local/Temp/gemini-budget-race-toolchain/race-evidence.json`。

上线前审查还修复了旧 Gemini 冷却状态边界：受管路由在请求内投影中忽略原有临时冷却，由预算账本准入；管理员禁用、真实模型禁用和已确认终止性无效凭据仍保留限制。历史错误原因转换为通用文案，不能在启用新策略后泄露真实模型或 Google 项目。此修正不改写保存的 Key、分组或模型清单，新增四项回归通过最终 Auth 竞态测试。

新版管理页由前端源提交 `9e5efb7` 构建，重新运行 `bun run verify` 的 1,742 项测试、lint、TypeScript 和 Vite 均通过。最终页面 SHA256 为 `B99DC4FD260FFCF40EB365889710C6521A6360E7758BCD50B6D850CA738828D1`；第 11 节中的摘要属于先前本地验证版本。

当前生产目标没有已挂载持久卷。计划给 `/data/gemini-budget` 单独挂载卷，不覆盖现有 ObjectStore 镜像目录。首次上线必须先暂时阻止客户端新请求、导出最后用量，完成部署后导入，再显式初始化和启用；最后恢复原客户端 Key。管理员已确认各 AI Studio Key 来自不同 Google 项目。最终生产验证及实际部署版本另见生产验证记录。

## 13. 生产验收完成（2026-10-12）

已经部署到 `https://cpa-p3.zeabur.app`，挂载独立持久卷、安全初始化并启用策略；原 25 条 Key、2 个组及模型映射保留，客户端访问恢复。真实请求返回 200、按两次上游尝试扣减 Lite 预算；实际重启完整保留计数；COS 首次预算备份成功，同日再次调度和重启没有改变对象及每日尝试标记。正式备份调度恢复为太平洋时间 `00:10`。

完整证据、版本及观察边界见 [生产验收记录](GEMINI_FREE_TIER_PRODUCTION_VALIDATION_CN.md)。前面第 11、12 节的“未部署”和“无持久卷”描述分别属于此前本地验证及部署前检查时点。

## 14. 服务端有界排队与退避（2026-10-13）

受管生成请求在凭据选择之前进入纯内存 FIFO 队列，按实际规范模型协调顺序。别名、Thinking 及重叠模型集合不能绕过同模型的队列上限；Flash 忙或日额度耗尽不会阻塞可用 Lite。候选仍遵守 provider、模型路由、固定 Key、凭据策略、管理员禁用及本请求已发送项目限制。

默认每个真实模型最多 128 个待准入请求，服务总计最多 256 个，单客户端请求累计凭据获取等待最多 75 秒。只等待忙碌槽位、短期冷却与模型熔断；日额度耗尽、失效凭据和不可信账本立即拒绝。取消、超时或溢出不预扣额度，不追加预算 WAL，也不触发 COS。

选择 turn 仅覆盖 Pick 到最终 HTTP Reserve 之间的间隙；Reserve 成功后立刻交接，等待者看到真实在途占用。HTTP 非 2xx 先持久记录冷却，再释放槽位，避免新请求抢在冷却落盘前发送。流式占用持续到响应体结束；不会因管理器返回流式对象而提前释放。

同一次请求在实际发送后的 429/503 上，下一次凭据获取先等待 500 或 1,000 毫秒，另加 0–100 毫秒抖动。等待与退避共享累计预算，不改变已连接上游的网络超时，不重置传输计数；最多 3 次实际发送、同项目每请求一次及流式首块后禁止重生成保持原约束。

可选配置示例：

```yaml
gemini-free-tier-policy:
  admission-queue:
    enabled: true
    max-pending-per-model: 128
    max-pending-total: 256
    max-wait-seconds: 75
```

已有开启策略的配置省略此段也使用以上默认值。设置 `enabled: false` 仅关闭排队，不关闭预算策略。现有 Key、分组、模型映射、RPD 和 COS 调度无需修改。V8 预算状态新增 `admission_queue` 运行时指标，包括待准入数量、等待时间、超时、取消和退避次数；指标只保存在内存，重启后重新计数。

以下测试、构建、部署及观察结果保留为 **2026-10-13 的历史验收记录**。2026-10-23 本地复核时，生产记录第 10、11 节新增的 8 个证据引用中有 7 个原路径不存在，仅管理页面只读联调 JSON 仍可读取。本轮可核对源码提交、文档数字的算术关系及该页面留档，无法据此重新确认完整的历史生产结果，也不代表当前分支或当前生产状态；详细边界见 [历史证据可用性与复核边界](GEMINI_FREE_TIER_PRODUCTION_VALIDATION_CN.md#历史证据可用性与复核边界2026-10-23)。

当时验收记录记载：新增 17 项队列定向回归与竞态检查通过，另验证真实 Conductor→Gemini 执行器下单项目三请求排队，等待者零预扣、释放后各扣一次。完整 Auth、预算、COS、执行器辅助、配置、V8 管理、SDK handler、Gemini handler、API 和服务生命周期相关回归通过；Windows 构建与 Linux amd64 交叉编译通过。这些属于历史测试结论，本轮文档整理没有重新执行测试或构建。

2026-10-13 部署记录记载，源码 `5ec399af` 已部署到生产服务，完整保留原配置、25 条 Key、2 个分组与模型映射，4,501 次预算逐桶恢复。实际三请求同时调用唯一官方项目，捕捉到一个在途、两个等待，全部完整成功且真实模型桶精确增加三次；另一次客户端别名流式完整成功，COS 对象与每日尝试标记未变。最终短期等长窗口客户端 429/503 部署前后均为零，不能据此认定统计因果改善。详细结果见 [生产验收记录第 10 节](GEMINI_FREE_TIER_PRODUCTION_VALIDATION_CN.md#10-服务端排队与退避部署验收2026-10-13)。

同日后续完整 30 分钟窗口记录为 **568 次请求、549 次 HTTP 200、19 次 HTTP 429（3.3451%）、0 次 HTTP 503**。该窗口与部署前后的短窗口分别统计，短窗口零错误不代表后续持续没有 429，也不能证明队列带来了可归因的错误率改善。等待与退避累计量来自邻近快照，不能视为精确覆盖同一窗口；详细结果及采样边界见 [生产验收记录第 11 节：完整 30 分钟只读观察](GEMINI_FREE_TIER_PRODUCTION_VALIDATION_CN.md#11-完整-30-分钟只读观察2026-10-13)。
