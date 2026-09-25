# 跨服务诊断方案：Claude 独立审查与处理记录

日期：2026-09-24。审查工具：Claude Code 2.1.280。CLI 返回模型：`claude-opus-5-5`。审查完成状态：success，1 次审查回合。

审查输入：完整方案及 CLIProxyAPI、gcli2api、Aitoapi 的关键源码和已有诊断文档摘录；未向审查者提供凭证或生产日志。工具关闭，只读审查；源码覆盖范围有限，不能视为三仓库全代码审计。

原方案 SHA-256（UTF-8、LF）：`62cfbedd82a36322ba078927d900ca0bd084d31226d5f2054fcbea3a5bedc89b`。Claude 原文中的行号对应文末原方案快照，不对应修订后的方案。

## 审查处理原则

Claude 的总体结论为“修改后可实施”。以下原文是独立模型意见，不自动作为已验证事实或工程指令；主审将结合源码逐项采纳、修正或延后，并在统一方案记录最终约定。

## 主审处理结果

已修订 [统一方案](CROSS_SERVICE_DIAGNOSTICS_PLAN_CN.md)。以下为主审取舍，不是 Claude 对修订版的再次批准；本次未做第二轮 Claude 审查，尚未实现代码或完成跨项目联调。

| 意见 | 处理 | 最终约定与依据 |
| --- | --- | --- |
| P1-1：DEBUG 关闭缺少确定关联记录 | 部分采纳 | 三类最小基础访问记录必须带身份；受基础访问日志开关控制。结构、usage 和限速细节仍 DEBUG，不采纳常态输出详细 deliveredUsage；未开启时明确计数证据不足，保留既有详细诊断约束 |
| P1-2：不能仅信任传入 traceparent | 采纳并收紧表述 | traceId 只筛候选；双边 call/server 唯一配对才标 verified。它是日志证据而非密码学证明；同父多节点也可能是网关重放，标歧义而非直接判伪造 |
| P1-3：新旧头传播冲突 | 采纳原则，调整做法 | 本方案头由最后传播边界 Set，不 Add；新增 X-Diag-* 保留既有响应头含义。不采纳全局删除所有 X-Request-Id/追踪头，避免破坏提供方约定；重定向按目标重检 |
| P1-4：attempt/call/span 混淆 | 采纳 | server/call 两类 span，traceparent 总指当前 call；attemptId/retryScope 为业务属性，callNo 独立。无法观察的底层重传不计数，不由 transport 猜业务重试 |
| P2-1：调用方来源作用域 | 采纳并简化 | callerAlias 从现有鉴权结果取匿名标识，配环境/部署/服务查询；未知来源不猜 new-api。不新增 X-Correlation-Id 逐跳传播，不引入对真实 API key 的哈希方案 |
| P2-2：参与服务范围 | 调整后采纳 | 使用精确 origin 加必要路径前缀的 DIAG_PEERS；主机名 allowlist 过宽。首期不宣称 Aitoapi/其他服务永远不可能作为中间节点，保留通用接入接口 |
| P2-3：transport 装饰位置 | 采纳 | 查实 Devin 对 *http.Transport 有类型断言；在最终配置完成后装饰，不改 context/cache。另查实 usage 包装已有 MarkUpstreamAttempt，诊断不能重复计数 |
| P2-4：Aitoapi 同名不同义 | 采纳 | 增加映射表，日志用 logSeq，公共交付用 deliveryState；原 seq/deliveryOutcome 值保留，耗时标 timingSource |
| P2-5：日志完整性无依据 | 采纳并修正 | 入队前序号、丢弃原因、覆盖元数据及工具侧连续性校验；仅开头/结尾 DEBUG 都开不代表中途没有关闭。完整性不承诺覆盖不可检测的崩溃/出口损失 |
| P2-6：多进程日志交错 | 采纳并限定保证 | 基础 2 KiB、详细 4 KiB 目标，纯 JSONL 或 @diag 混流，按进程文件/已验证收集器。大小限制不等于跨平台原子写保证，需要实际部署测试 |
| P2-7：旧日志可能输出正文 | 采纳 | 摘录可确认 gcli2api 旧 WARNING 打印 part/text。导出只收结构化诊断；旧正文日志整改独立提交，本次不扩大为业务代码修改 |
| P2-8：响应头对外兼容 | 采纳 | X-Diag-Request-Id / X-Diag-Trace-Id 为新契约，不强制改变既有 X-Request-Id 语义 |
| P2-9：流/异步生命周期 | 采纳原则，调整实现 | HTTP 正文结束一次结算，ContextVar 不跨上下文 reset，队列入队前快照。不采纳“禁止 ALS”结论；正确捕获上下文即可，禁止 flush 时依赖环境上下文。避免新增全局请求 Map，优先既有请求句柄 |
| P2-10：匿名凭证身份 | 采纳简化版本 | 已有非敏感内部 ID 或进程随机映射，明确作用域；不把邮箱、文件名或数组序号冒充永久身份，不增加跨部署密钥管理 |
| P3-1、2、4、9：缩小首期 | 采纳 | 延后浏览器子 span/协议变化、跨部署 HMAC 和 OTLP；复用日志出口与缓冲，仅四类新增 DEBUG 语义点 |
| P3-3：仅保留 bootId | 不完全采纳 | 多台运维需要部署/实例可读归属，保留其资源字段和自动回退；bootId 区分进程启动，pid 只作辅助，不能凭跨主机 pid 判断冲突 |
| P3-5：新根一律 sampled=01 | 不采纳固定 01 | 首期无追踪导出采样要求，新根使用 00；采样标志不控制基础访问记录/DEBUG。采用合规传播器与共享测试向量，不把手写行数少当成优点 |
| P3-6、7、8 | 部分采纳 | 拒绝 ID 只记原因/长度，不引入额外 HMAC。新耗时用单调时钟；CPA 旧短 ID 不作全局键 |
| 清理顺序与纯空白 part | 已核对，独立修复项 | 摘录能确认先判末尾角色、再删空消息；也能确认在 rstrip 前判断有效值。这些业务修复不混入观测方案实现 |
| 解压正文与错误头透传 | 条件性记录 | 仅凭 httpx_client 局部代码不能认定最终返回一定出错；需检查完整返回路径/假上游压缩响应测试后再定级，不在本次顺带修复 |

Claude 提出的“两类请求/调用终局记录 + 双边校验 + 按需语义观察”比原先全事件铺开更适合低侵入要求，已作为首期架构。其建议的 host-only 白名单、全局删除业务头、所有新根 sampled=01、强制常态 usage、仅 bootId 归属等没有原样照搬。

## DIAG-00 候选补充（2026-09-25，待复审）

本次未调用 Claude，保留以下历史原文与完整方案快照。旧审查的“修改后可实施”不批准当前契约；完整修订方案须由协调窗口先检查再送 Claude。

本次将主审取舍具体化为 [v1 契约](contracts/diagnostics/v1/README.md)、封闭 JSON Schema 和共享向量。新增的确定规则包括：serverSpanId 所有者、boot 级匿名作用域；入站认证适配与出站 DIAG_PEERS 分离；路径分段/默认端口/重定向和整体配置失败回退；重复 traceparent 与多字段 tracestate 区分；双边对端/来源/重复/循环冲突；终局序号含自身、完整性优先级与坏行脱敏；Aitoapi 字段来源映射。均为待审工程决策，不声称 Claude 已采纳。

继续拒绝常态 deliveredUsage、全局删业务 X-Request-Id、host-only allowlist、仅 bootId、多语言通用新队列、浏览器子 span 和跨部署 HMAC。基础日志/DEBUG、未知/零、原始身份与管理统计契约保持原取舍。

本候选的日志格式采用公共信封＋data，旧字段由适配投影且不重复输出同一终局；这要求 DIAG-02/03/04 明确各自白名单接入点并用真实处理边界通过向量。公开 schema 不能替代敏感值来源审查，也不能替代 DIAG-07 行为等价与多 worker 输出测试。

## Claude 原始审查结果

# 跨服务模型代理追踪方案：独立架构审查

审查依据只有你提供的方案全文和几段源码摘录。以下内容我没有看到，涉及时按条件性结论处理：CPA 的重试调度器和各 executor、gcli 的路由/重试/头转发代码、Aitoapi 的 GenerationPipeline 和浏览器脚本、new-api 实际发送的头、以及部署拓扑（worker 模型、stdout 与文件、入口网关）。

---

## 总体结论：修改后可实施

方向是对的：
- 用 W3C traceparent 表达父子关系。
- traceId、requestId、attemptId 分开，不用外部 ID 覆盖内部 ID。
- 不重构主流程，详细诊断受 DEBUG 门控。

但按现文档直接实施，会在"准确关联"这个核心目标上失败，主要有四处：

1. **没有定义 DEBUG 关闭时一定写出的关联记录。** 生产环境常态下，Aitoapi 的日志里根本不会出现 traceId。
2. **任何入站 traceparent 都被当作父子关系的直接证据。** 固定或复用的 traceparent 会伪造出"重试兄弟节点"。
3. **没有处理既有头透传。** gcli 的头转发和错误路径的响应头透传，会产生重复或过期的 traceparent / X-Request-Id。
4. **attempt、client span、网络调用三者层级定义自相矛盾。** 嵌套重试计数无法统一验证。

同时存在过度设计，第一期应删除：浏览器子 span、跨服务 HMAC 指纹、X-Correlation-Id 逐跳传播、三语言通用有界队列，以及 10 类事件在三仓库全量铺开。

---

## 一、问题分级

### P1：不修正会导致关联错误或核心证据缺失

#### P1-1 缺少"常开关联记录"，DEBUG 关闭时跨服务关联实际不存在

- **位置**：L161–163、L118、L228；`Aitoapi-custom/src/utils/LoggingService.js:78`；`CLIProxyAPI/internal/logging/gin_logger.go:36`
- **失败场景**：
  - L161 写"本地必要状态常态有效"，但没有定义任何常开记录。
  - L228 只要求扩展 `diagnostic()` 白名单，而 `diagnostic()` 第一行就是 `if (!isDebugEnabled()) return false`。
  - 生产以 INFO 运行时，CPA 返回了 X-Trace-Id，但 Aitoapi 没有任何一行带 traceId 的日志，只能按时间猜。
  - 87 token、限速这类偶发问题，除非长期开 DEBUG，否则永远没有证据。
- **影响**：方案的核心价值（跨服务可关联）只在 DEBUG 下成立，与 L161 自相矛盾。
- **最小修正**：
  - 每个服务、每个 server span 固定写一条常开 JSON 记录 `diag.server`。
  - 每次出站调用写一条 `diag.call`（字段见第二节）。
  - 单行不超过 2 KiB，不含正文，不受 DEBUG 控制。
  - 两者都在请求结束或调用结束时写出一次，不需要队列。
  - `diag.server` 必须包含交付给调用方的 usage 数值（`deliveredUsage`）。这是在不改 new-api 的前提下，与 new-api 计费记录做"时间 + 模型 + usage"匹配的唯一可靠线索。

#### P1-2 入站 traceparent 未经验证就作为直接证据，会伪造重试或合并无关请求

- **位置**：L84、L172、L176；L76 只处理了"重复 span"
- **失败场景**：
  - 某客户端或 SDK 使用固定的 traceparent，例如直接抄 W3C 规范示例值；或者 new-api 原样透传了客户端的头（是否透传未知）。
  - 数千个无关请求共享同一个 traceId，并且 parentSpanId 也相同。
  - 离线工具按 traceId 建树（L172）后，它们表现为"同一父 span 下的多次兄弟调用"。这与"未改造的调用方重试了 N 次"在形态上完全一样。L76 的冲突检测识别不了。
  - 同类情况：服务网格或入口网关（如 Envoy 类代理）重写 traceparent 的 parent-id 后，子服务的 parentSpanId 指向网关 span，调用方记录里找不到这个 span。
- **影响**：直接违反"准确关联、避免日志混乱"。
- **最小修正**：把"直接证据"改为双边校验。
  - 一条边判为 `verified`，当且仅当同时满足：
    - 调用方有一条 `diag.call`，其 spanId 为 S、traceId 为 T；
    - 被调方有一条 `diag.server`，其 parentSpanId 为 S、traceId 为 T。
  - 其他情况一律展示为 `external_parent_unverified`。
  - 同一个 S 对应多个被调方 server span 时，标 `duplicate_parent`，不当作重试计数。
  - 可选的防御：对未认证或未配置为可信的调用方，新建 trace，并把原 traceparent 记为 `linkedTraceId`（相当于 OTel 的 span link）。第一期可以只做工具侧校验。

#### P1-3 头透传与本方案注入冲突，导致父子关系错位或变为无效上下文

- **位置**：L26（gcli 已有追踪头转发）、L96–98、L201；`gcli2api/src/httpx_client.py:111`
- **失败场景**：
  - **出站**：gcli 既有逻辑把入站 traceparent / X-Request-Id 按白名单转发给上游。
    - 如果包装器用 Add 注入，会出现两个 traceparent。按 W3C 规则这是无效上下文，下游会新建 trace，链路断开。
    - 如果透传值后写入并覆盖注入值，下游的 parent 会跳过 gcli，直接挂到 CPA 的 span 上。gcli 三次内部尝试全部"消失"。
    - 如果下游是 Google，内部头会泄露给第三方，违反 L99。
  - **响应**：`httpx_client.py:111` 在非 200 时用 `dict(r.headers)` 构造 FastAPI Response。如果调用方把它直接返回，上游（或下一跳参与服务）的 `X-Request-Id` / `X-Trace-Id` 会进入 gcli 的响应。ASGI 中间件再追加一个，就会出现两个 X-Request-Id。
- **影响**：父子树错误，响应 ID 不确定。L98 只写了原则，没有给出实现规则。
- **最小修正**：在契约中写死以下规则。
  - **出站**：先从待发送头中删除 `traceparent`、`tracestate`、`X-Request-Id`、`X-Correlation-Id`、`X-Trace-Id`，再用 Set 写入本跳的值。对非参与服务只删除、不写入。
  - **响应**：在响应头提交点（ASGI `http.response.start`、Gin 中间件、Express `writeHead` 之前）替换而不是追加本服务的 ID。透传来的对端值改记为 `peerRequestId` / `peerTraceId`。
  - **验收**：每个方向同名头恰好一个。

#### P1-4 attempt / client span / 网络调用层级定义不一致，嵌套重试计数无法验证

- **位置**：L36、L40、L47、L95、L203、L244
- **问题描述**：
  - L47 和 L95 把"一次重试"等同于"一个 client span"。
  - L203 又引入"业务 attempt 包含多个子网络 span"。
  - 文档没有规定 traceparent 里携带的是哪一层的 spanId，也没有规定 attemptNo 由哪一层重试拥有。
- **失败场景**：
  - 一个服务内部可能有多个重试拥有者，例如凭证轮换加上 base URL 回退或空回复重试。CPA 的具体实现我未见到，属于条件性判断。
  - 实现方 A 把业务 attempt 的 spanId 放进 traceparent，实现方 B 用网络 span。
  - L244 的"2 × 3 = 6 次上游结果"在不同仓库里会得出 6、9 或 12 等不同数字，而且无法判定谁对。
- **最小修正**：只保留两层。
  - server span：每次入站一个。
  - call span：每一次 HTTP 请求或 WS 派发一个；traceparent 永远携带 call span 的 id。
  - 业务尝试不再单独建 span，作为 call 上的属性：`attemptNo`、`retryScope`（例如 `auth_rotation | base_url_fallback | empty_retry | stream_bootstrap`，按各仓库实际存在的层级枚举）、`callNo`（本 server span 内单调递增）。
  - 计数口径：
    - "请求失败次数"按 server span 计。
    - "尝试次数"按 `(retryScope, attemptNo)` 去重计。
    - "网络失败次数"按 call 计。

### P2：条件性风险或显著影响准确性、安全性、可维护性

#### P2-1 调用方 ID 没有来源命名空间；X-Correlation-Id 的信任边界未定义

- **位置**：L38–39、L86–87、L96、L172
- **问题**：
  - 两个 new-api 实例，或 new-api 与直连客户端，可能产生相同的 X-Request-Id。
  - 入口网关可能在缺失时自动生成 X-Request-Id，这时 callerRequestId 看起来像来自 new-api，实际不是。
  - L87 的"可信内部跳转"没有任何判定依据。在 new-api → CPA → gcli 链路中，gcli 收到的 X-Request-Id 是 CPA 的 8 位 ID：如果 gcli 不信任 CPA 的 X-Correlation-Id，就会退回用 CPA 的 ID 建 correlationId，各跳语义不一致。
- **修正**：
  - 新增 `callerAlias`：在已有鉴权完成后得到的调用方主体别名。优先用服务已有的 key 名；否则用 HMAC-SHA256（部署盐，API key）截断到 12 个十六进制字符；没有盐时为 null。它只在 `(deploymentId, service)` 范围内有意义，在 `diag.server` 写出时读取。
  - callerRequestId 的检索键改为 `(callerAlias, callerRequestId)`。
  - 不声称 callerRequestId 的来源是 new-api。
  - 第一期删除 X-Correlation-Id 逐跳传播。深层跳由 trace 可达，只需要入口 span 的 callerRequestId。

#### P2-2 "参与服务 / 第三方"无法由包装器判定；出站范围可以大幅缩小

- **位置**：L95、L99
- **问题**：CPA 的 OpenAI 兼容或 Gemini 兼容上游，可能配置成 gcli/Aitoapi，也可能是真正的第三方。传输包装器看不到业务语义。
- **修正**：
  - 用部署变量 `DIAG_PEER_HOSTS`（主机名 allowlist）判定：命中则注入追踪头，其余一律不注入任何追踪或内部头。第三方默认连 traceparent 都不发。
  - **范围缩减**：
    - Aitoapi 的上游是浏览器 → Google，不可能接到另一个参与服务，不需要出站传播。
    - gcli 只有在其上游 base URL 可以配置成另一个参与服务时才需要（条件性，未见代码）。
    - 所以"任意排列"只要求三者都做入站，出站注入第一期只在 CPA 实现。

#### P2-3 CPA 包装挂载点可能破坏既有 transport 定制

- **位置**：L213–214；`CLIProxyAPI/internal/runtime/executor/helps/proxy_helpers.go:52-53`、`:66-75`
- **问题**：`NewDevinHTTPClient` 对 context 里的 RoundTripper 做 `rt.(*http.Transport)` 类型断言，然后 Clone 并设置 `DisableCompression`。如果为了"一处覆盖所有分支"而在 `cliproxy.roundtripper` 的来源处包装，断言就会失败，Devin 路径失去关闭压缩的定制，行为静默改变。
- **修正**：
  - 规定只在最终赋值 `httpClient.Transport` 之后、返回之前包装最外层，用一个 `finalizeClient(c)` 收口。
  - nil 分支显式包装 `http.DefaultTransport`。
  - 永远不替换 context 里的值，也不修改缓存中的 transport。
  - 包装器转发 `CloseIdleConnections`。
  - 与既有 usage transport 的相对顺序，在提交说明中逐分支列出。

#### P2-4 与 Aitoapi 既有字段同名不同义

- **位置**：L116、L118、L143、L164；实施记录 L28；`LoggingService.js:14`、`:117`
- **冲突点**：
  - `deliveryOutcome`：现有值为 `success`（实施记录 L28），方案要求 `local_finished`（L143）。
  - 顶层 `seq`：已在白名单中，而且 recentEvents 里 `seq` 表示流事件序号；方案要把它定义为 span 内日志序号（L118）。
  - `logsDropped` 与 `droppedEvents` 不一致。
  - `firstEffectiveMs` 与 `firstEffectiveOutputMs` 不一致。
- **失败场景**：离线工具按同名字段聚合，把 success 与 local_finished 分成两类；或者用旧的流 seq 做去重，误删或误报冲突。
- **修正**：
  - 契约中附一张"既有字段 → 公共字段"映射表。
  - 新语义一律用新名字，例如日志序号用 `logSeq`。
  - 不改变既有字段的取值。

#### P2-5 `observationComplete` 缺乏可计算依据；现有代码存在静默丢弃

- **位置**：L118、L163–164；`LoggingService.js:36-43`、`:117`、`:122`
- **现状**：
  - `logsDropped` 是 logger 级累计值，无法判断某个请求是否丢了事件。
  - 超过 8 KiB 的行直接 `return false`，不计数。
  - 关闭 DEBUG 时直接清空队列，也不计数。
  - 终局事件恰好超长时会无声消失。
- **修正**：
  - 每条 DEBUG 事件带 span 内的 `logSeq`，且在入队前分配。
  - `diag.server` 带 `lastLogSeq` 和 `debugCoverage`（`full | partial | none`）。
    - `full` 定义为：span 开始和结束时 DEBUG 均开启，并且本 span 没有丢弃。
  - 工具按 `logSeq` 的空洞精确判断缺失。
  - 超长时降级为截断存根记录；清空队列时计入丢弃。

#### P2-6 多进程共享输出时行交错；JSON 与文本混流

- **位置**：L164（8 KiB）、L116、L168；`gin_logger.go:36`
- **问题**：
  - 多个 worker 共享容器 stdout（管道）时，只有不超过 PIPE_BUF（Linux 为 4096 字节）的单次写入保证原子。8 KiB 的行可能与其他进程交错，产生损坏的 JSON。
  - 多进程写同一个轮转文件也可能丢日志。
  - CPA 的 logrus 格式化器会给消息加 `[时间] [info ] | id |` 前缀，JSONL 无法直接提取。
- **修正**：
  - 常开记录不超过 2 KiB；DEBUG 在共享 stdout 部署时不超过 4 KiB。
  - 诊断行用固定前缀（例如 `@diag `）后接纯 JSON，绕过格式化器直接写同一个 writer。
  - 多进程写文件时按进程分文件。
  - 现有文本行格式不改。

#### P2-7 既有日志已在输出正文，与数据边界矛盾

- **位置**：L165、L168；`gcli2api/src/converter/antigravity_fix.py:827`、`:842`、`:847`
- **问题**：这些 WARNING 会输出用户文本、part 内容（可能含内联数据或签名）。导出诊断包时如果附带文本日志，就违反 L165。
- **修正**：
  - 导出工具只收 `@diag` 行。
  - 在单独的业务修复提交中，把这三处改为只记录结构（位置、类型、长度）。

#### P2-8 响应头变化对外可见（条件性）

- **位置**：L97–98
- **条件**：如果某服务当前把上游的 `x-request-id` 透传给客户端，改成返回本服务的 ID 会改变客户端可见的语义。
- **修正**：只新增、不改变既有头。当某服务已有 X-Request-Id 语义时，以新头名返回本服务 ID，并在迁移说明中写明。

#### P2-9 生命周期实现约束缺失

- **位置**：L107–108、L201
- **Go**：
  - call span 在 `RoundTrip` 返回时不能结束。
  - 需要包装 `resp.Body`，在 EOF、Close、错误或 ctx 取消中最先发生的一个时写 `diag.call`，只写一次。
  - 状态放在包装对象内，不进全局 map。调用方从不 Close 时记录缺失，即为"证据不全"。
  - 101 Upgrade 响应的 Body 必须保持 `io.ReadWriteCloser`（条件：存在经 http.Client 升级的路径时）。
- **Python**：
  - 只在 ASGI 中间件里设置 ContextVar，一次性放入一个可变 holder 对象。
  - 不在异步生成器内 set/reset；跨上下文 reset 会抛出 ValueError。
  - 请求结束写记录时把 holder 置为 sealed，之后后台任务的写入只计为 `lateWrites`。
- **Node**：
  - Aitoapi 的共享 `setImmediate` flush 会在第一个调度它的请求的 ALS 上下文中运行。
  - 建议不用 ALS，沿用 `req.__generationStartedAt` 的写法（`RequestHandler.js:1379`），用 `req.__diag` 显式持有上下文。

#### P2-10 凭证匿名标识未定义（条件性）

- **位置**：L66、L166
- **风险**：如果实现直接使用认证文件名或账号标识，而这些值可能包含邮箱，导出时就会泄露。
- **修正**：固定为"HMAC（部署盐，稳定凭证 ID）截断"，或已有的非敏感配置别名。没有盐时只在本进程内稳定，并在记录中标明 `credentialRefScope=boot`。

### P3：优化或过度设计

| 编号 | 位置 | 建议 |
|---|---|---|
| P3-1 | L109–110、L229 | 删除浏览器子 span、WS `traceContext`、`browserTraceSupported`。浏览器日志只进 DOM，这些 ID 无人收集；浏览器时钟不可比。服务端 attempt 已能通过 `request_attempt_id` 关联，浏览器回报的耗时作为属性即可。另外 `_forwardRequest` 用 `...proxyRequest` 整体展开发送，往 proxyRequest 上加字段会自动下发到浏览器，需要警惕 |
| P3-2 | L155–157 | 延后跨服务 HMAC 指纹，它需要多部署共享密钥和轮换。87 token 链路先靠各跳的 `deliveredUsage`、文本字节数和 finishReason；指纹第一期只在单服务内使用 |
| P3-3 | L59–62、L74 | `bootId`（UUID）作为唯一必需身份锚点；`instanceId` 从平台已有变量尽力读取，否则为 null，不强推 `DIAG_INSTANCE_ID`。去重键简化为 `(bootId, spanId, event, logSeq)`。增加信息字段 `pid`：同一 bootId 出现多个 pid 即报警，用来检测 fork 继承缺陷 |
| P3-4 | L164 | 常开记录直接同步写入既有 logger，每请求约 1+N 行，不需要新的有界队列；有界队列只用于 DEBUG |
| P3-5 | L95 | 写明 flags 规则：沿用上游上下文时原样传递 trace-flags；新建时写 `01`。第一期不添加自有 tracestate 条目，只原样传递合法值 |
| P3-6 | L89 | 被拒的自定义 ID 记录 `callerRequestIdRejected` 原因、长度和短 HMAC，否则现场无法排查格式问题 |
| P3-7 | L147 | Aitoapi 既有耗时基于 `Date.now()`（`RequestHandler.js:1379`），适配时不能宣称是单调时钟；新字段用 `performance.now()` |
| P3-8 | L45 | CPA 的 8 位 ID 约 7.7 万次请求即有约 50% 碰撞概率。保留它，但在文档中声明它只用于展示和旧日志检索，下游 callerRequestId 不能单独作为检索键 |
| P3-9 | L122–135 | 10 类事件第一期只保留 4 个 DEBUG 语义点：`normalized`、`attempt_usage`、`converted`、`throttle`。其余由 `diag.server` / `diag.call` 覆盖 |

### 旁支：摘录可确认的业务缺陷（不属于日志改造，建议独立提交并优先处理）

1. **末尾 model 清理顺序错误**：`antigravity_fix.py:761-769` 在 `:797-859` 之前执行。对 `[…, user, model, user(空)]` 这样的输入：
   - 移除末尾 model 的循环先看到最后一条是 user，于是不动；
   - 随后清理步骤删掉空的 user；
   - 最终以 model 结尾，不支持预填充的模型返回 400。

   这与 L236 所说的顺序缺陷一致。
2. **纯空白 text 会被保留**：`:811-815` 在 `rstrip` 之前判断是否有有效值；`:838-839` 再 rstrip 之后，纯空白 text 变成 `""` 仍被保留。
3. **错误路径头透传（条件性）**：`httpx_client.py:111` 中 `aread()` 返回解压后的正文，但 `dict(r.headers)` 保留了 `content-encoding` / `content-length`。如果该 Response 被直接返回给客户端，会出现长度或编码不一致。

修正方式：把第 1、2 条放在清理之后统一处理，末尾 model 移除放到最后执行。

---

## 二、替代架构：两类常开终结记录 + 双边校验 + 按需语义诊断

核心思路：常开层只做"谁调用了谁、结果如何"，DEBUG 层才做"为什么"。关联靠两端记录互相印证，不靠信任入站头。

### 常开记录（每条不超过 2 KiB，不受 DEBUG 控制）

| 记录 | 生命周期 | 关键字段与语义 |
|---|---|---|
| `diag.process` | 进程启动时写一次，DEBUG 切换时各写一次 | `service`、`bootId`、`pid`、`instanceId?`、`deploymentId?`、`environment?`、`buildCommit?`、`debugEnabled`、限速配置版本 |
| `diag.server` | 每个入站 server span 结束（完成、取消或出错）时写一次；进程崩溃则缺失，按缺口展示 | `traceId`、`spanId`、`parentSpanId\|null`、`traceContextState`（`accepted \| generated \| invalid_replaced \| untrusted_restarted`）、`linkedTraceId?`、`requestId`、`callerRequestId?`、`callerAlias?`、路由模板、请求模型、`clientStreaming`、`wireStatus`、`headersCommitted`、`resultClass\|null`、`endReason`（`finished \| client_cancel \| error`）、`deliveredUsage{input,output,reasoning,total,present,source}`、`responseCommitMs`、`totalMs`、`attemptCount`、`callCount`、`lastLogSeq`、`debugCoverage`、`lateWrites` |
| `diag.call` | 每一次出站 HTTP 请求或 WS 派发，在 EOF / Close / 取消 / 错误中最先发生者时写一次 | `traceId`、`spanId`（即出站 traceparent 中的 parent-id）、`parentSpanId`（本 server span）、`requestId`、`attemptNo`、`retryScope`、`callNo`、`targetAlias`、`peer`（是否命中 `DIAG_PEER_HOSTS`）、`credentialRef`、`status`、`headersMs`、`totalMs`、`endReason`（`eof \| closed_early \| cancelled \| transport_error \| timeout`）、`respBytes`、`peerRequestId?`、`peerTraceId?`、`providerRequestId?` |

`peerTraceId` 与本方 `traceId` 不一致时，说明对端重启了上下文（无效头、不信任、网关改写）。这条边不需要任何内网信息就能被标记为 `restarted`。

### 实现方式

- **请求级 holder**：入口中间件创建一个可变 holder，放入 ctx / ContextVar / `req.__diag`。业务侧的 observe 调用写入 holder，holder 为空时是 no-op。结束时中间件读出 holder 写 `diag.server`。这样主流程只增加少量写字段调用，不需要传参。
- **离线 join**：
  - 按 P1-2 规则得到 `verified` / `unverified` / `duplicate_parent` / `restarted` 四种边。
  - new-api 侧没有 ID 时，按 `(时间窗, 模型, deliveredUsage)` 做候选匹配，并标为"推测"。

### 对比

| 维度 | 现方案 | 方案 A（推荐） | 方案 B（各语言 OTel SDK + 文件或 stdout 导出） |
|---|---|---|---|
| 改动范围 | 3 仓库 × 10 事件 + WS 协议 + 浏览器 + HMAC + 身份配置 | 3 个中间件 + CPA 一个 client 收口 + 1 个 attempt 标注点 + 4 个 DEBUG 语义点 | 三套 SDK 初始化与上下文桥接；Node 的自动埋点依赖运行时补丁，违反 L205，只能手工埋点 |
| 关联准确性 | 入站头即证据，可被伪造或误合并 | 双边校验，缺口显式展示 | 同现方案，需要另写校验 |
| DEBUG 关闭时 | 基本不可关联 | 可关联，且有 usage | 取决于采样配置 |
| 依赖 | 无 | 无（traceparent 解析约数十行，配共享测试向量） | 新增 SDK 依赖；三种 console 导出格式不统一，版本兼容性未知 |
| 上线顺序 | 需要协议一次到位 | 被调方先上，逐步见效，可单独回退 | 需要统一导出后才有价值 |
| 代价与风险 | 高，且范围蔓延 | 低 | 中高，未来接 Collector 时更顺畅 |

方案 A 的字段与 OTel 日志数据模型的 TraceId / SpanId 对齐，将来接 OTLP 时把 `diag.server` / `diag.call` 映射为 span 即可，不需要推翻。

---

## 三、最少改动实施顺序

| 步骤 | 仓库 | 改动 | 说明 |
|---|---|---|---|
| 0 | gcli | 旁支业务修复（清理顺序、空白 text、三处正文日志） | 独立提交，不等日志改造 |
| 1 | 契约 | 一页契约：traceparent 解析与输出规则、三类记录 schema、头清洗规则、Aitoapi 字段映射表、测试向量与 join 用例 | 无代码 |
| 2 | gcli | 新增 diag 模块；纯 ASGI 中间件（入站解析、holder、`http.response.start` 时替换响应头、结束写 `diag.server`）；从既有头转发白名单中剔除追踪与内部头；在重试循环的既有位置标注 attemptNo / retryScope，并在收集结束时写 usage | 只有上游可指向参与服务时，才在 `httpx_client.py` 的 `get_client_kwargs` 中挂 httpx `event_hooks` 注入头，不替换 transport（替换会绕开 proxy / 环境代理 mounts） |
| 3 | Aitoapi | Express 中间件写 `req.__diag`；`_startTrackedRequest` 建立 requestId → ctx 的有界 Map，结算时删除；`diagnostic()` 按 `fields.requestId` 自动补全身份字段、白名单加字段、补齐静默丢弃计数；在既有结算方法经非 DEBUG 路径写 `diag.server` | 第一期不改 WS 协议 |
| 4 | CPA | 在 `gin_logger.go` 的 AI 路径分支内解析 traceparent、放 holder、`c.Next()` 前设响应头、结束后写 `diag.server`（现有文本行格式不变）；`proxy_helpers.go` 各返回点统一 `finalizeClient`；在重试拥有者处加一行 ctx 标注（attemptNo / retryScope / credentialRef） | 按 `DIAG_PEER_HOSTS` 注入 |
| 5 | 工具 | 离线 join 脚本：读 `@diag` 行，去重、四类边、缺口与覆盖展示 | |
| 6 | CPA / gcli | DEBUG 语义点：转换摘要、usage 来源、限速实际参数 | CPA 近期提交 `c1bcc62c` / `fde39036` 正处于这一区域，可在其边界接入 |
| 7 | 可选 | WS traceContext、跨服务指纹、OTLP、new-api 主动发 traceparent | |

### 明确不该改的主流程

- **CPA**：
  - 路由和模型选择；凭证调度与重试策略（只加 ctx 标注）。
  - 各 executor 的请求构造与翻译器；流式写出。
  - 限速算法；usage transport 的顺序。
  - 代理选择与 `NewDevinHTTPClient` 的 transport 定制。
  - 8 位 requestId 的生成与文本日志格式。
- **gcli**：
  - 凭证选择、重试次数与条件、`stream_post_async` 的产出语义。
  - 规范化逻辑（除步骤 0 的独立修复外）。
  - 响应格式。
- **Aitoapi**：
  - `request_id` / `request_attempt_id` 的格式与用途（队列、取消、ACK）。
  - GenerationPipeline、超时与首输出期限、WS protocol_version=2 的校验。
  - 统计 schema 与管理 API 语义。

---

## 四、高价值验收场景（全部使用假上游）

1. **固定 traceparent 冒用**：两个独立客户端并发使用 W3C 规范示例值。
   - 期望：两棵互不相关的树，均为 `external_parent_unverified`，不计为重试。
2. **嵌套重试 + 负载均衡**：CPA 两次调用分别落到 gcli 的 G1 和 G2，每次 gcli 内部三次调用，其中一次为同一 attempt 内的 base URL 回退（若存在）。
   - 期望：2 条 verified 边；gcli 侧 6 条 `diag.call`；attempt 数与 call 数按 `retryScope` 分别正确。
3. **头清洗**：入站同时带 traceparent、X-Request-Id、X-Correlation-Id；上游错误响应带 x-request-id。
   - 期望：发往参与服务的请求中 traceparent 恰好一个，且等于 CPA 的 call span；发往 Google 的请求不含任何内部或追踪头；返回给客户端的 X-Request-Id 恰好一个，且是本服务的。
4. **DEBUG 全关**：模拟 output=87 的响应。
   - 期望：每一跳 `diag.server.deliveredUsage` 可见且 `present` 标记正确，可与模拟的 new-api 记录按"时间 + 模型 + usage"匹配。
5. **CPA transport 等价性**：每个返回分支（请求代理、auth 代理、context RT、nil、Devin）在包装前后，出站字节、Accept-Encoding、代理和连接复用都一致。
6. **多进程**：多 worker（如使用 preload 模式）得到不同 bootId；多进程并发向共享 stdout 写最大长度的行，全部可解析。
7. **流中途客户端取消**：`diag.server.endReason=client_cancel`，`diag.call.endReason=cancelled | closed_early`，任何一方都不判为 success。
8. **诊断缺口**：超长事件、队列溢出、请求中途关闭 DEBUG 三种情况下，`logSeq` 的空洞与 `debugCoverage=partial` 都能被准确检出。
9. **网关生成 X-Request-Id**：callerRequestId 被记录，但不被当作 new-api 来源，检索结果标为候选。
10. **进程在流式中途重启**：调用方 `transport_error`，被调方缺少 `diag.server`，工具展示为缺口而不是成功。
11. **Aitoapi 并发与迟到事件**：共享 flush、浏览器重连、迟到的 attempt_closed 都不串到当前 attempt 或其他请求。
12. **gcli 规范化**：`[user, model, user(空)]` 在 no-prefill 模型下以 user 结尾；纯空白 text part 被移除；正常的工具调用与响应保持不变。

---

## 五、必须修订与可延后

**必须修订（阻断实施）**
- P1-1：定义常开的 `diag.server` / `diag.call` / `diag.process`，含 `deliveredUsage`。
- P1-2：双边校验的边分类；工具不再只按 traceId 建树。
- P1-3：出站"先删后 Set"、响应"替换不追加"的头清洗规则。
- P1-4：两层 span 模型，以及 `attemptNo` / `retryScope` / `callNo` 的计数口径。
- P2-1：`callerAlias` 命名空间；删除 X-Correlation-Id 传播。
- P2-2：`DIAG_PEER_HOSTS`，第三方默认零注入；出站传播第一期仅限 CPA。
- P2-3：CPA 包装只挂在最终 `Transport` 赋值点。
- P2-4：Aitoapi 字段映射表，新语义使用新字段名。
- P2-5：`logSeq` / `lastLogSeq` / `debugCoverage` 的精确定义。
- P2-6：行长上限与 `@diag` 前缀输出。

**可延后**
- P2-7 中的旧日志整改，可随业务修复一起处理。
- P2-8：响应头迁移。
- P2-10：带盐的凭证标识；第一期可以先用 boot 级作用域。
- 全部 P3。
- WS traceContext 与浏览器 span、跨服务 HMAC 指纹、OTLP 与集中查询、new-api 接入。

**需要补充信息后才能定级的点**
- CPA 是否存在 base URL 回退或流式启动重试等多个重试拥有者。
- gcli 头转发白名单的方向与内容。
- gcli 的上游 URL 是否可配置。
- 各服务的 worker 模型与日志落地方式（stdout 或文件）。
- 入口是否有网关或服务网格改写头。
- Aitoapi 顶层 `seq` 的现有用途。

## 审查时的完整方案快照

<details>
<summary>展开原方案（行号按以下文本从标题开始计数）</summary>

# 多服务请求追踪与模型诊断统一方案

日期：2026-09-24。状态：规划，尚未实施。契约标识：`ai-proxy-diagnostics/1`。

补充约束：每种服务都可能同时部署多台、多副本、多进程；诊断必须准确归属到实际执行实例。实现优先独立模块、入口中间件、共享传输包装与现有事件适配，减少主体流程修改。

## 1. 目标与范围

适用项目：CLIProxyAPI、gcli2api、Aitoapi-custom，以及后续接入的代理服务。new-api 当前不修改；未来可以通过相同协议接入。不固定服务顺序，不指定 CLIProxyAPI 为唯一追踪入口。

以下链路使用同一套规则：

- new-api → CLIProxyAPI → gcli2api → 模型上游。
- new-api → gcli2api → 模型上游。
- new-api → Aitoapi → 浏览器执行端 → 模型上游。
- new-api → CLIProxyAPI → Aitoapi → 浏览器执行端 → 模型上游。
- 客户端直接访问上述任意模型服务，以及多个代理串联、重试或切换不同上游。

本方案交付追踪协议、日志语义、各仓库接入点和验收要求。没有部署、修改生产配置、调用真实模型或变更管理 API 的要求。消息清理等业务修复单独提交，不能让日志改造暗中改变请求内容、重试策略、限速或超时。

## 2. 本地代码现状

| 项目 | 已有能力 | 接入缺口 |
| --- | --- | --- |
| CLIProxyAPI | Gin 请求 ID、执行器日志、Gemini 非流式响应摘要、限速逻辑 | 当前 ID 为本地生成的 8 位十六进制；缺少统一跨服务父子关系；响应摘要尚未覆盖所有协议和最终输出阶段 |
| gcli2api | Antigravity 请求规范化、重试、流转非流收集日志；API 层存在部分追踪头转发能力 | 路由、清理、凭证切换和收集日志没有贯穿统一上下文；仅有头部白名单不代表整个链路已接通 |
| Aitoapi-custom | 内部 requestId、request_attempt_id、DEBUG JSON 诊断、空回复和流结束分类、bootId/buildCommit | 保留内部队列和取消关联 ID；新增跨服务上下文、外部 ID 映射和公共字段；扩展 LoggingService 白名单 |

Aitoapi 参考现有 `docs/empty-response-diagnostic-logging-plan.zh-CN.md`、`docs/empty-response-implementation-2026-09-24.zh-CN.md`。它们约定详细诊断受 DEBUG 门控；本方案沿用。代码现状不等于远程部署版本，线上分析必须检查构建标识。

## 3. 身份模型：链路、服务请求、尝试分别标识

| 字段 | 含义与生命周期 |
| --- | --- |
| traceId | 整条被观测链路。W3C 128 位、32 个小写十六进制字符、非全零。没有有效上游上下文时，当前入口生成新值 |
| spanId / parentSpanId | 当前操作及直接父操作。64 位、16 个小写十六进制字符、非全零。每次服务接收、每次对外尝试建立相应 span |
| requestId | 当前服务一次入站请求的内部 ID。服务自己生成，保留项目既有约定；不能拿外部 ID 覆盖内部队列或统计键 |
| callerRequestId | 当前调用方通过 X-Request-Id 传入的 ID；仅作为关联别名 |
| correlationId / correlationSource | 可选业务关联别名，用于从 new-api 记录检索。与 traceId 分开，不作为唯一身份 |
| attemptId / attemptNo | 当前服务当前请求中的一次实际上游尝试；每次重试、切换或并行分支独立编号。attemptNo 仅在本地 requestId 下有意义 |
| providerRequestId | 上游实际返回的请求 ID；不覆盖任何上述字段 |
| environment / deploymentId / service | 环境、逻辑部署、服务类型。不同独立安装区分 deploymentId，同一部署的副本共享 deploymentId |
| nodeLabel / instanceId / bootId / buildCommit | 人工可读别名、实际运行副本身份、进程启动身份、构建版本。nodeLabel/buildCommit 可空；instanceId/bootId 必须本地生成或解析得到 |

链路关系由 traceId + span 父子关系表达；本地业务仍用自己的 requestId/attemptId。CLIProxyAPI 旧短 ID 可作为兼容检索字段，不能充当全局 traceId。唯一定位日志还应包含 service、instanceId、bootId。

一次 CLIProxyAPI 重试对应一个新的 client span，gcli2api 接收该调用时创建其子 server span。gcli2api 的内部重试再创建各自子 client span。CLIProxyAPI 第 2 次调用和 gcli2api 第 2 次尝试不是同一编号空间。

### 3.1 多台、多副本与重启

身份资源层级为 `environment / deploymentId / service / instanceId / bootId`。这组字段由当前进程启动时建立，不能由请求头覆盖；它描述“谁实际执行”，trace/span 描述“这次调用经过谁”。

| 层次 | 例子（示例值） | 规则 |
| --- | --- | --- |
| environment | production、staging | 区分环境；同一链路可以显式跨环境，查询不得静默混合环境 |
| deploymentId | edge-east、edge-west、gemini-pool-a | 逻辑安装/服务池，建议部署配置提供。默认 unassigned，不能据此认定多个安装属于同一组 |
| service | cliproxyapi、gcli2api、aitoapi | 固定服务类型，不手工给每个副本发明不同 service 名 |
| nodeLabel | cpa-east-01、gcli-pool-a-02 | 可选人工别名，仅用于阅读，重复也不能合并实例 |
| instanceId | 平台提供的副本 UID，或本地随机 UUID | 优先使用已确认唯一的副本身份；没有可靠身份就随机生成，instanceIdentitySource 标明 platform/configured/ephemeral。不得仅使用 IP、主机名或进程号 |
| bootId | 每次进程启动随机 UUID | 同一副本多 worker、重启、滚动发布均可区分；进程 fork 后在各 worker 初始化，不继承主进程 bootId |

建议新增的可选部署配置统一为 `DIAG_ENVIRONMENT`、`DIAG_DEPLOYMENT_ID`、`DIAG_NODE_LABEL`、`DIAG_INSTANCE_ID`，均为本方案拟议配置，尚未存在。service 由项目固定，bootId 自动生成。不能把同一 DIAG_INSTANCE_ID 复制给所有副本；未配置时使用随机身份仍可定位，界面明确显示“未分组/临时身份”，不阻止模型服务启动。不依赖中心注册服务分配 ID，不在业务数据目录写身份文件。

平台副本身份相同且有多个 worker 时，instanceId 表示副本、bootId 表示实际进程。临时 instanceId 在重启后可能改变，不能宣称它能提供永久主机身份。buildCommit 是版本，不是实例身份。

查询分组先按 environment/deploymentId/service，再列实例；链路视图按 span 父子关系跨实例展开。凭证匿名标识只在所属 deploymentId/service 下比较，不因为两个实例都出现“凭证 1”就认定是同一账号。

### 3.2 负载均衡、跨实例重试与日志归属

例如同一 trace 的首次调用到达 gcli 实例 G1，CLIProxyAPI 重试后到达 G2：两次调用必须有不同 client span，G1/G2 各自的 server span 分别指向它们。每个节点展示 service、deploymentId、instanceId、bootId、requestId 和 attempt，不能把两台机器的“第 1 次尝试”合并。

调用方记录的 targetAlias 是逻辑上游地址配置，不代表实际处理实例。实际实例以接收方日志的资源字段为准，通过 trace/span 关联；没有接收方日志时显示 peerInstance=unknown。第一期不增加携带内网机器信息的响应头，不因负载均衡域名相同猜测实例相同。

事件去重键为 `(service, instanceId, bootId, spanId, seq)`；requestId、时间戳和文本内容都不单独用于去重。多份导出有相同事件键时去重；相同键但内容不同则保留冲突并告警。缺少这些字段的旧日志保留文件/行号，不强行去重。跨实例排序使用父子关系和各 span 内 seq，墙钟仅辅助展示。

traceId 用于检索一个 trace 的候选节点，不能让重复或伪造的外部上下文导致覆盖记录。若出现多个无关联根、重复 span 身份或父节点缺失，展示分支/冲突/缺口，不自动修补成一条看似完整的链路。

## 4. HTTP 传递规则

采用 [W3C Trace Context](https://www.w3.org/TR/trace-context/) 的 `traceparent` / `tracestate`，不要用自定义头替代标准父子关系。优先使用合规传播器处理格式和未来版本。

### 4.1 入站

1. 有有效 traceparent：沿用 traceId，生成本服务 spanId，将传入 parent-id 作为 parentSpanId。
2. 没有或无效 traceparent：生成新 traceId 和服务 span，记录 contextSource=generated 或 invalid_replaced。错误追踪头不应让正常模型请求返回 400。
3. X-Request-Id 存入 callerRequestId。对已配置的调用方，可映射其已有其他头名；不猜测 new-api 当前是否实际发送某个头。
4. 可信内部跳转携带 X-Correlation-Id 时保留业务别名；否则可从合法 X-Request-Id 建立 correlationId，并记录来源。它只是查询线索，不能证明两个请求为同一次。
5. traceparent、外部 ID 均不用于认证、计费幂等、账号选择、取消其他请求或打开 DEBUG。即使两个调用方提交相同 ID，也必须有独立本地 requestId/spanId。
6. 自定义 ID 接受单值且不超过 128 个 ASCII 字符，字符限定为字母、数字及 `._:/-`；多值、控制字符或超长值忽略，记录原因，不打印原值。标准追踪头按照标准规则验证。

### 4.2 出站与响应

| 方向 | 规则 |
| --- | --- |
| 调用参与服务 | 创建本次尝试的 client span；traceparent 携带相同 traceId 和该 spanId。有效 tracestate 按标准处理 |
| 内部兼容头 | X-Request-Id 使用调用方当前本地 requestId；可选 X-Correlation-Id 沿用受控业务别名。子服务独立生成自己的 requestId |
| 返回响应 | X-Request-Id 返回当前服务自己的 requestId；新增 X-Trace-Id 返回 traceId。二者是本方案响应约定，不是 W3C 标准响应头 |
| 上游响应透传 | 截取上游请求 ID 为 providerRequestId，不能让头透传覆盖本服务响应 ID |
| 调用第三方模型 | 默认仅使用提供方支持的追踪头；内部业务关联头不自动传给第三方。记录本地 attempt 与提供方 ID 的映射 |

响应 ID 应在正常提交响应头时写出，成功、错误和流式入口行为一致；不能为了返回 ID 提前提交 HTTP 200、增加 SSE 事件或修改模型 JSON。客户端 CORS 暴露相关响应头时沿用既有允许源策略。

X-Trace-Id 不作为入站标准父子上下文。只有 X-Request-Id 而没有 traceparent 时，独立入口/重试可能产生不同 traceId；可以按关联别名检索多个 trace，但不能把它们硬合并成一个。没有任何 ID 的未改造 new-api 仍能调用，只能按时间、模型等筛选其平台记录。

## 5. 非 HTTP 与流式生命周期

- Go 使用 context.Context；Python 使用 ContextVar 并在请求/迭代结束时正确恢复；Node.js 使用 AsyncLocalStorage 或显式不可变上下文。后台日志线程必须在入队之前捕获身份，不读取线程当前上下文。
- 流式请求的 server span 保持到本地发送结束、失败或取消；不能在返回生成器或收到响应头时就结算成功。
- Aitoapi 的 WebSocket 控制消息增加可选 traceContext，对应 traceId、parent span 等；保留 request_id/request_attempt_id 作为队列、取消和结束确认身份。浏览器执行端建立子 span，而不是复用服务端 spanId。
- 兼容旧浏览器脚本：缺少 traceContext 时由服务端通过原 attemptId 关联，标记 browserTraceSupported=false；不能推测浏览器内部阶段已经观测到。
- 同一请求的不同尝试、多个并发请求、后台任务和连接重建不得串上下文。取消后的迟到事件属于原 attempt，不得写入当前尝试。
- 观测只读取既有取消/超时状态，不引入新的模型超时、心跳、重试或提前 flush。

## 6. 公共日志契约

统一交换格式为单行 JSON，新增 `diagnosticSchema: "ai-proxy-diagnostics/1"`。保留项目已有 schemaVersion 和既有字段含义；新协议版本独立管理。公共字段采用 camelCase，CLIProxyAPI 的 request_id、gcli2api 原字段通过明确适配映射，旧文本日志保留可检索性。

公共信封：diagnosticSchema、ts（UTC ISO 毫秒）、level、event、environment、deploymentId、service、nodeLabel、instanceId、instanceIdentitySource、bootId、buildCommit、traceId、spanId、parentSpanId、requestId、callerRequestId、correlationId、correlationSource、attemptId、attemptNo、seq、stage、observationComplete。可空字段不能以空字符串冒充有效 ID；未知计数用 null，观测到零才写 0。seq 在本地 span 内递增，不宣称跨进程全局排序。

日志关联方式与 [OpenTelemetry Logs Data Model](https://opentelemetry.io/docs/specs/otel/logs/data-model/) 的 TraceId/SpanId 对齐，后续可映射到 OTLP；本期不依赖部署 Collector 或集中日志平台。

### 6.1 事件

| 公共事件 | 关键证据 |
| --- | --- |
| request.received | 路由模板、入站协议、请求模型、实际请求流式状态、输入结构摘要 |
| request.normalized | 变换前后结构、实际模型、变换类型、空消息和末尾 model 的处理 |
| upstream.attempt_started | attempt、目标服务别名、协议、实际模型、匿名凭证标识 |
| upstream.headers_received | 上游 HTTP 状态、提供方 ID、首头耗时 |
| upstream.attempt_finished | 上游错误分类、usage、文本/工具/媒体摘要、结束证据和该尝试耗时 |
| retry.decided | 是否重试、原因、等待时长、本地尝试数；成功重试不抹去先前失败 |
| response.converted | 上游原始/汇总值与输出协议最终值的摘要、usage 来源和缺失字段 |
| throttle.finished | 是否启用、目标速率、计数依据、计划/实际等待、取消状态 |
| response.committed | 实际 HTTP 状态、首个写出类型（有效内容/保活等）、头是否提交 |
| request.finished | 最终尝试、请求语义结果、本地交付结果、总耗时、日志完整性 |

Aitoapi 已有 generation.* 事件保留，适配映射到以上语义，不重复发两套相同事件。service 特有浏览器、队列、缓冲和 ACK 事件作为扩展，不能要求其他服务伪造这些阶段。

### 6.2 状态、计数与时间

- 分开记录 upstreamStatus、wireStatus、resultClass、deliveryOutcome、failureOrigin、failureStage。HTTP 200 与内容成功不是一回事；开流后发生错误不能在日志里把实际 200 改写成 502。
- resultClass 包括 success、blocked、empty、incomplete、error、cancelled、unknown；附分类依据。仅无普通文本不能判 empty，工具调用和媒体可能是有效输出；未看到完整结尾不能写 success。
- deliveryOutcome=local_finished 仅表示本地写出完成，不证明 new-api 已读取、计费或展示。
- usage 分别保存 input、candidate、reasoning、outputTotal，以及 source、字段存在性和协议口径。OpenAI completion_tokens 可能已含 reasoning，不能再次相加；保留提供方原始字段和规范化结果。
- 不累计同一流重复到达的累计 usage；逐候选和末尾仅含 metadata 的帧必须被正确观察。重试 token 按 attempt 保存，最终响应 usage 与全链路尝试成本分别展示，不能混算。
- clientStreaming、upstreamStreaming、deliveryMode 分开记录，支持非流请求经内部流式收集、伪流、真实流。
- 总耗时、等待和速率使用各进程单调时钟；墙钟只用于定位。跨服务不能相减两台机器的时间戳来断言网络耗时。
- 记录 firstUpstreamByteMs、firstEffectiveOutputMs、responseCommitMs、firstDownstreamEffectiveOutputMs、totalMs。保活空白不算首个有效模型输出。
- 限速记录配置快照/版本、targetTokensPerSecond、选中的首字延迟、tokenCount/tokenSource、plannedWaitMs/actualWaitMs、elapsedMs、cancelled。速率必须附 token 分子和时间分母口径。

### 6.3 请求结构与重复短回复

结构摘要记录消息总数、末尾最多 4 条消息的原始位置/角色/part 类型/文本字节数、空消息数量、工具调用与工具响应数量。变换记录操作类型、位置、原因和变换前后末尾角色，不记录正文或工具参数。

文本摘要按候选分别统计普通文本 UTF-8 字节数；普通文本、思考、工具参数、媒体分开。重复识别指纹使用明确版本、相同语义提取顺序，不能拿 JSON 包装体哈希比较不同协议。新跨服务指纹优先采用受控 HMAC-SHA256 并附算法、keyId、完整性；密钥只走部署配置，不入日志。没有相同 keyId/提取版本则不能跨服务比较，缺失指纹不阻断基本诊断。既有 CLIProxyAPI SHA256 字段保留为 legacy 摘要，不冒充新指纹。

流式哈希按文本增量更新，避免累计帧重复计入；帧解析失败或截断须标 partial。工具只有计数，不采集工具参数哈希。指纹只是判断可见文本是否相同，不能反推出 87 token 内容或证明业务请求相同。

## 7. 日志级别、负载与数据边界

- 上下文提取/传递、响应 ID、本地必要状态常态有效，DEBUG 关闭不切断 trace。
- 新增详细诊断事件统一受本地 DEBUG 门控，沿用 Aitoapi 约定；外部 sampled 标志不能打开正文日志或详细诊断。原有基础访问/错误日志可增补 traceId/spanId。
- DEBUG 关闭时不生成仅用于诊断的结构摘要和哈希。动态开启只完整采集之后新请求；动态关闭停止新输出，标记采集中断，不能宣称缺失记录代表阶段未发生。
- 不逐 token/chunk 输出日志。仅阶段事件和可选低频进展；新日志单行上限 8 KiB，队列有界、为终局预留空间，记录 droppedEvents。丢日志或未启用诊断时报告证据不全，不推断请求卡死。
- 不采集 API Key、Cookie、OAuth token、签名、完整请求/回复、工具参数、图片或任意异常对象；目标仅记录配置别名和路由模板。上游错误优先稳定分类和白名单 reason，任意 message 不直接输出。
- 凭证使用服务内匿名稳定标识；数组索引只作临时定位，不当跨服务账号身份。
- 高基数字段如 traceId、requestId 不作为监控指标标签。诊断故障不得让正常模型请求失败，也不能形成无限缓存。
- 日志保留/轮转沿用部署配置，跨项目导出的诊断包默认只含结构化摘要。集中存储作为后续独立工作。

## 8. 定位流程和证据等级

查询工具第一期读取多个项目导出的 JSONL，按 traceId 重建 span 树，按 service/requestId/attemptId 展示事件。输入仅有 new-api ID 时先查 callerRequestId/correlationId，列出所有候选 trace；不可任意挑选一条。

导出包清单记录资源身份、构建版本、契约版本、日志时间范围、DEBUG 覆盖情况、丢弃计数和来源文件。文件名仅供阅读，不能作为实例身份的唯一依据。导入不同机器日志时保留来源，优先展示部署和实例，再展示错误汇总；一次请求失败数量与实际网络尝试失败数量分开统计。

输出分三类：同 trace/父子关系的直接证据、相同关联别名的候选关联、只有时间/模型的推测。旧日志没有 trace 时始终降低置信度。日志尾部缺失、时钟偏移、DEBUG 未开和日志丢失需要显式显示。

| 问题 | 必要证据 |
| --- | --- |
| 末尾 model 导致 400 | 同一 trace 与对应尝试中，接收/发送/清理前后结构，及上游错误 reason；定位空 user 首次出现和被删除的位置 |
| 重复输出 87 token | 各阶段 usage 来源、最终发送给调用方的 usage、可见文本指纹；若各受控层均未产生 87，只能将范围缩小到调用方计数/展示，不能断言其算法 |
| 非流速度超限 | 最终 usage、限速 tokenCount、目标速率、实际等待和本地交付耗时；与平台统计口径分别比较 |
| 429/503 过多 | 按服务与 attempt 计数，区分 quota_exhausted、capacity_unavailable、rate_limited、resource_exhausted_unknown；不把通用 429 自动认定为额度用完 |
| HTTP 200 但空/截断 | 终止帧、EOF、解析状态、有效输出类型、转换前后摘要、HTTP 提交和本地交付结果 |

## 9. 各项目实施边界

### 9.1 减少主体流程修改的实现原则

各语言实现独立 diagnostics 模块，包含资源身份、上下文传播、公共事件构造和有界日志输出。业务代码只依赖少量只读观察接口，不自行拼接 JSON、生成 trace/span、校验头或管理诊断缓存。

接入优先级固定：

1. **入口中间件**：统一建立上下文、设置响应 ID、记录入站与本地结束；不逐个路由复制接入代码。
2. **共享出站包装**：HTTP transport/client 边界创建 span 和注入头、记录网络状态；WS 在已有消息发送/接收边界适配。
3. **日志适配器**：从请求上下文自动添加公共身份，复用现有诊断事件。禁止依靠解析人类日志文本来猜重试和业务结果。
4. **少量语义观察点**：只有中间件/传输层无法得知的内容才在既有规范化、收集、转换、重试决策和限速边界添加 observe 调用。只读实际业务结果，不复制一套业务计算。

仅用 HTTP 包装无法判断“哪条 user 消息被删除”或“限速为何等待”，因此保留必要语义观察点；不能为追求零改动而丢失关键证据，也不能把追踪参数逐层加入所有函数签名。

包装器不得预读完整请求/响应、替换 SSE 解析器、缓存全部生成内容、改变代理/TLS/连接池/重定向规则或重新实现重试。流摘要尽量消费已有解析结果；确需观察字节时仅在现有读取发生后计数，保留 Close、取消、背压和流式 writer 的必要接口。请求头使用当前请求的副本，禁止修改共享 client 的全局 headers。

已有 attempt/span 由执行上下文复用，避免传输包装和执行器双重创建同一次尝试。一个业务 attempt 若实际发生多个网络请求，用独立子网络 span 表达，不能多次结算业务 attempt。连接鉴权、额度同步等非模型后台请求不误归入当前模型尝试。

公共接入不要求重构路由、模型选择、凭证选择、重试循环、计费统计或流式发送主流程；禁止全局 monkey patch、运行时函数替换及新增控制中心依赖。采集模式关闭时详细 observe 为 no-op，基础 trace 传递继续工作。

每个实现提交列出“新增模块、既有文件接入点、为什么中间件无法替代该观察点、覆盖协议”。评审重点检查业务主流程 diff；修改散布到多个同类路由时，先收敛到共享边界。日志能力分批提交，400 业务修复独立提交，方便分别回退。

### CLIProxyAPI

- `internal/logging/requestid.go`、`gin_logger.go`：公共上下文和入站提取，保留旧 request_id。
- `internal/logging/global_logger.go`：公共字段白名单或结构化诊断出口，防止字段被格式化器丢弃。
- 优先在共享 HTTP client 构造/装饰边界挂载追踪 RoundTripper，例如 `internal/runtime/executor/helps/proxy_helpers.go`；必须覆盖其不同返回分支。帮助函数放 `internal/runtime/executor/helps/`，不在每个 executor 重复注入。
- 自定义 transport、WS 等不经过该构造器的路径单独列入覆盖矩阵，通过自身共享边界适配，不能声称一次包装覆盖全部执行器。保留已有 usage transport 的顺序和行为。
- 请求接收/完成使用入口及现有生命周期；转换后和最终输出在已有公共转换/发送边界增加摘要观察，必要时少量协议专用钩子；限速只在实际限速实现边界观察生效值。
- 先验证当前 Gemini→Antigravity 路径，再以相同抽象补齐其余协议。支持矩阵明确标记未覆盖的执行器，不能把部分接入宣称为全覆盖。

### gcli2api

- 纯 ASGI 入口中间件与日志层建立上下文，关注 StreamingResponse 生命周期和 ContextVar 隔离；不在每条路由重复设置/清理上下文。
- `src/httpx_client.py` 的共享请求/流式边界包装传输观察；Antigravity 的共享规范化、重试决策和收集函数添加少量语义观察。凭证选择继续使用现有流程，只读取已选定的匿名标识。
- 接收直接 new-api 调用与 CLIProxyAPI 调用使用同一中间件。
- 通用上下文可复用；本期业务诊断与消息清理修复聚焦 Antigravity，不顺带扩展已停止维护的 GeminiCLI 业务。

### Aitoapi-custom

- Express 入口中间件建立上下文；在 `src/core/RequestHandler.js` 既有 `_startTrackedRequest`、attempt 初始化/切换及结算方法集中映射 requestId/attemptId，避免在各协议处理方法重复接入。不改变队列、取消、统计归因身份。
- `src/utils/LoggingService.js` 扩展公共字段白名单，保持 DEBUG 门控、有界队列和动态关闭规则。
- 浏览器 WebSocket 增量加入可选上下文，版本兼容和迟到 ACK 的校验沿用已有规则。
- 适配现有 generation.* 结果、usage、浏览器阶段，不改已约定的管理 API requestId 或审计语义。未来管理追踪另行定义。

## 10. 分期与验收

1. **公共契约与共享测试向量**：确定上述字段、多实例身份配置、优先级、失败回退、事件映射与标准 traceparent 测试样本。一个契约版本，三个语言实现，不要求共享源码或新增微服务。
2. **三个服务的上下文接入**：入口、HTTP/WS 出站、响应 ID、异步生命周期、日志字段。先做到身份可关联，不改变模型业务行为。
3. **业务证据补齐**：CLIProxyAPI/gcli2api 的转换、usage、限速；Aitoapi 现有诊断适配；单独修复已确认的消息清理顺序缺陷。
4. **离线关联工具与联调**：读取三方诊断导出，以一次命令按 ID 展示请求树、重试和差异；共享脱敏 fixture。
5. **可选后续**：new-api 主动发送标准上下文；OTLP 导出、集中查询界面及告警。它们不作为前四阶段的前置依赖。

必须用本地假上游验证，不依赖生产凭证或真实模型调用：

- 所有目标拓扑；入口无 ID、仅 X-Request-Id、有合法 traceparent、多种头并存、坏头及重复 ID。
- new-api 使用同一个业务 ID 发起两次无 trace 请求：两个独立 trace 可按别名检索，不错误合并。
- CLIProxyAPI 两次外部尝试、每次 gcli2api 三次内部尝试：保留 2 层父子关系及 6 次上游结果；Aitoapi 切换账号/并行请求同样不串线。
- 高并发、流式延迟结束、客户端取消、异步生成器、后台日志线程、浏览器重连和迟到事件。
- 每种服务至少两个实例，跨实例负载均衡/重试、同一容器多个 worker、实例重启、滚动发布；短 requestId/attemptNo/人工别名相同也不混淆。多实例实例身份错误配置时暴露冲突，不覆盖日志。
- 同一日志重复导入、导出重叠、跨机时钟偏移、缺少父节点和重复外部 trace；验证去重、请求/尝试分别计数和证据缺口提示。
- 包装前后模型请求正文/响应正文一致、发送顺序和背压不变、Close/取消正确传递、无新增网络调用、无重复 attempt 结算；检查代理与自定义 transport 不受影响。
- 流/非流/流转非流、保活、工具/媒体输出、内容拦截、空回复、缺少结束帧、错误帧出现在部分文本之后。
- user→model→空 user、多个空消息、正常工具响应：记录清理差异，验证独立业务修复没有损坏有效上下文。
- usage 缺失/零/仅末尾帧/含 reasoning 的累计值/多次重复累计帧；确认不双计，不把输出 87 硬编码为异常。
- 限速关闭/启用/取消，日志参数等于实际参数；Go 使用可控时间测试。
- DEBUG 开关、日志过载和记录截断；无敏感字段、无每 chunk 日志、无诊断引入的提前 HTTP 提交。
- 旧响应解析器忽略新增头；原协议正文、状态码、管理契约保持兼容；未改造节点导致的链路缺口明确显示。

每个仓库完成后独立提交，注明契约版本及已覆盖协议；全部目标链路通过假上游联调后再安排发布。当前文档不代表三个项目已经实现或部署。

</details>
