# 多服务请求追踪与模型诊断统一方案

日期：2026-09-24；DIAG-00 修订：2026-09-25。状态：契约候选待审核，业务尚未实施。契约标识：`ai-proxy-diagnostics/1`。

执行授权：用户已明确要求实施八任务计划。DIAG-00 已在本批新建隔离任务交付待审；DIAG-01 已经协调窗口代码检查、Claude R1 审核并推送准确 HEAD，其他任务仍等待批准依赖。旧 Transport closed 阻塞已解除。本文件为 DIAG-00 分支中的完整修订方案，源工作区台账保持只读；执行状态以协调窗口为准。

审查状态：已完成 Claude 独立审查，并按已核对的意见修订；不是代码审计或上线验收。原始意见、处理取舍及原方案快照见 [Claude 审查记录](CROSS_SERVICE_DIAGNOSTICS_CLAUDE_REVIEW_CN.md)。首期收敛为“最小访问关联 + 双边证据校验 + 四类 DEBUG 语义观察”，浏览器子 span、跨部署内容指纹和 OTLP 延后。

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
| spanId / parentSpanId | 当前操作及直接父操作。64 位、16 个小写十六进制字符、非全零。首期只有入站 server span 和单次出站 call span；不单独建立业务 attempt span |
| requestId | 当前服务一次入站请求的内部 ID。服务自己生成，保留项目既有约定；不能拿外部 ID 覆盖内部队列或统计键 |
| callerRequestId / callerAlias | 调用方传入的请求别名及本地已认证主体的匿名别名。callerAlias 从已有鉴权结果取得，不能信任自报的头；不可用则 null。原始 API key、邮箱不参与输出 |
| callerIdSource | 记录取值头名及来源可信度，不因为头名为 X-Request-Id 就声称来自 new-api |
| attemptId / attemptNo / retryScope | 沿用当前服务既有业务尝试身份、序号及重试拥有者；作用域包含当前 server span。未知则 null，不由传输层猜测业务重试 |
| callNo | 当前 server span 内实际出站调用的递增序号，分配并发安全；每个 call 有独立 spanId |
| providerRequestId | 上游实际返回的请求 ID；不覆盖任何上述字段 |
| environment / deploymentId / service | 环境、逻辑部署、服务类型。不同独立安装区分 deploymentId，同一部署的副本共享 deploymentId |
| nodeLabel / instanceId / bootId / buildCommit | 人工可读别名、实际运行副本身份、进程启动身份、构建版本。nodeLabel/buildCommit 可空；instanceId/bootId 必须本地生成或解析得到 |

链路关系由 traceId + span 父子关系表达；本地业务仍用自己的 requestId/attemptId。CLIProxyAPI 旧短 ID 可作为兼容检索字段，不能充当全局 traceId。唯一定位日志还应包含 service、instanceId、bootId。

每次应用层实际调用产生 call span，出站 traceparent 的 parent-id 始终是该 call spanId；接收服务 server span 指向它。一项业务 attempt 可以关联多个 call，但不额外引入 attempt span。底层不可观察的 TCP 重传不计为新 call；HTTP 重定向等若在选定传输边界实际可见则有独立 call，并标明 callKind，不冒充业务重试。

请求数按 server span 计；出站调用数按 call span 计；业务尝试按 `(server span, retryScope, attemptId)` 计。没有可靠 attemptId 时，只有重试拥有者能保证序号唯一才使用 attemptNo，否则标 unknown。不能仅凭错误码、相同 parent 或 transport 进入次数判为业务重试。

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

例如同一 trace 的首次调用到达 gcli 实例 G1，CLIProxyAPI 重试后到达 G2：两次调用必须有不同 call span，G1/G2 各自的 server span 分别指向它们。每个节点展示 service、deploymentId、instanceId、bootId、requestId 和 attempt，不能把两台机器的“第 1 次尝试”合并。

调用方记录的 targetAlias 是逻辑上游地址配置，不代表实际处理实例。实际实例以接收方日志的资源字段为准，通过 trace/span 关联；没有接收方日志时显示 peerInstance=unknown。第一期不增加携带内网机器信息的响应头，不因负载均衡域名相同猜测实例相同。

事件去重键为 `(service, instanceId, bootId, spanId, logSeq)`；requestId、时间戳和文本内容都不单独用于去重。多份导出有相同事件键时去重；相同键但内容不同则保留冲突并告警。缺少这些字段的旧日志保留文件/行号，不强行去重。跨实例排序使用已核对的父子关系和各 span 内 logSeq，墙钟仅辅助展示。pid 仅作排查 fork/worker 的辅助字段，不能替代 bootId 或跨机器判断唯一性。

traceId 用于检索一个 trace 的候选节点，不能让重复或伪造的外部上下文导致覆盖记录。若出现多个无关联根、重复 span 身份或父节点缺失，展示分支/冲突/缺口，不自动修补成一条看似完整的链路。

## 4. HTTP 传递规则

采用 [W3C Trace Context](https://www.w3.org/TR/trace-context/) 的 `traceparent` / `tracestate`，不要用自定义头替代标准父子关系。优先使用合规传播器处理格式和未来版本。

### 4.1 入站

1. 有有效 traceparent：沿用 traceId，生成本服务 spanId，将传入 parent-id 作为 parentSpanId。
2. 没有或无效 traceparent：生成新 traceId 和服务 span，记录 contextSource=generated 或 invalid_replaced。错误追踪头不应让正常模型请求返回 400。
3. X-Request-Id 存入 callerRequestId；只对明确配置的调用方适配其他头名。callerAlias 由本地鉴权结果决定；来源不明时不推测为 new-api。
4. 首期不新增 X-Correlation-Id 逐跳传播。每个入口保留自身 callerRequestId，深层通过 trace/call 证据关联。查询别名必须带本地环境、部署、服务、callerAlias 作用域；callerAlias 缺失时列出所有候选，不合并。
5. traceparent、外部 ID 均不用于认证、计费幂等、账号选择、取消其他请求或打开 DEBUG。即使两个调用方提交相同 ID，也必须有独立本地 requestId/spanId。
6. 自定义 ID 接受单值且不超过 128 个 ASCII 字符，字符限定为字母、数字及 `._:/-`；多值、控制字符或超长值忽略，记录原因，不打印原值。标准追踪头按照标准规则验证。

接收 traceparent 仅表示接受远端声称的上下文，不能自动证明调用关系。解析失败必须丢弃与其配套的 tracestate；本期不写自有 tracestate。新建上下文使用有效的版本 00、flags=00，已有合法 flags 按传播规范处理；采样位不控制访问记录或 DEBUG。实现需通过共享测试向量，不以“几十行手写解析器”作为验收标准。

### 4.2 出站与响应

| 方向 | 规则 |
| --- | --- |
| 调用参与服务 | 创建本次实际出站调用的 call span；traceparent 携带相同 traceId 和该 spanId。有效 tracestate 按标准处理 |
| 内部关联头 | 新增 X-Diag-Request-Id 表示当前发送服务的本地 requestId，只有配置的内部调用方适配器接收它。既有 X-Request-Id 行为保留，不要求逐跳改写 |
| 返回响应 | 新增 X-Diag-Request-Id / X-Diag-Trace-Id 返回本服务 requestId / traceId。它们是本方案自定义响应约定。既有 X-Request-Id 和 X-Trace-Id 如已有含义则不改变 |
| 上游响应透传 | 新增诊断响应头在本地正常提交点替换为本服务值，不追加重复值；对端值另记 peerRequestId/peerTraceId，提供方旧 ID 另记 providerRequestId |
| 调用第三方模型 | 默认仅使用提供方支持的追踪头；内部业务关联头不自动传给第三方。记录本地 attempt 与提供方 ID 的映射 |

响应 ID 应在正常提交响应头时写出，成功、错误和流式入口行为一致；不能为了返回 ID 提前提交 HTTP 200、增加 SSE 事件或修改模型 JSON。客户端 CORS 暴露相关响应头时沿用既有允许源策略。

参与服务判定由本地拟议配置 `DIAG_PEERS` 映射定义，匹配规范化的精确 origin（scheme、host、port）及必要的路由前缀，不仅匹配主机名。复用现有上游配置别名；不增加独立服务注册中心。默认未配置目标不注入本方案新增头。逻辑目标与实际副本身份分开记录。

诊断传播适配器在既有业务头构造完成后处理：对参与服务，在请求副本上大小写不敏感地替换 traceparent/tracestate 和本方案 X-Diag-* 头，每种字段只发送一个合法值。不能使用 Add，不能让旧白名单再次覆盖。自动头透传不得把入站追踪头直接复制给第三方；提供方明确要求的既有头行为按提供方适配器保留，不进行全局删除 X-Request-Id 等业务头。重定向仍遵守原策略，每个新目标重新检查是否允许传播，跨 origin 不继承本方案内部关联头。若共享层无法可靠做到，先标记该路径未接入，不能偷偷改变重定向行为。

X-Trace-Id / X-Diag-Trace-Id 不作为入站标准父子上下文。只有 X-Request-Id 而没有 traceparent 时，独立入口/重试可能产生不同 traceId；可以按带来源作用域的 callerRequestId 检索多个 trace，但不能把它们硬合并成一个。没有任何 ID 的未改造 new-api 仍能调用，只能按时间、模型等筛选其平台记录。

## 5. 非 HTTP 与流式生命周期

- Go 使用 context.Context；Python 使用 ContextVar 并在请求/迭代结束时正确恢复；Node.js 使用 AsyncLocalStorage 或显式不可变上下文。后台日志线程必须在入队之前捕获身份，不读取线程当前上下文。
- 流式请求的 server span 保持到本地发送结束、失败或取消；不能在返回生成器或收到响应头时就结算成功。
- Aitoapi 第一期不改浏览器脚本和 WS 协议。服务端为实际派发建立 call span，通过既有 request_id/request_attempt_id 关联回报，浏览器已有耗时作为独立来源的属性；不声称观察到了浏览器内部 span。
- 浏览器 traceContext 和独立子 span 作为后续可选能力，待确有跨浏览器日志需求再设计版本兼容；禁止把服务端新增 holder 放进会整体序列化的 proxyRequest 对象。
- 同一请求的不同尝试、多个并发请求、后台任务和连接重建不得串上下文。取消后的迟到事件属于原 attempt，不得写入当前尝试。
- 观测只读取既有取消/超时状态，不引入新的模型超时、心跳、重试或提前 flush。

结束规则：HTTP call 在正文 EOF、提前 Close、读取异常或取消时只结算一次，RoundTrip 返回头时不结算；记录 endReason，保留流式/升级需要的接口。Python ContextVar 在拥有其 token 的同一入口上下文重置，异步生成器读取请求 holder，不跨上下文 reset；并行 call 通过显式不可变 call 句柄隔离。Node 可用 ALS 或既有请求句柄，在入队时快照公共身份，不在共享 flush 时从 ALS 读取。请求结束后 holder 封闭；有界迟到事件引用原身份，已写出的终局数据不回写。crash 或未关闭导致无终局事件时报告缺口。

## 6. 公共日志契约

统一交换格式为单行 JSON，新增 `diagnosticSchema: "ai-proxy-diagnostics/1"`。保留项目已有 schemaVersion 和既有字段含义；新协议版本独立管理。公共字段采用 camelCase，CLIProxyAPI 的 request_id、gcli2api 原字段通过明确适配映射，旧文本日志保留可检索性。

公共信封：diagnosticSchema、ts（UTC ISO 毫秒）、level、event、environment、deploymentId、service、nodeLabel、instanceId、instanceIdentitySource、bootId、buildCommit、traceId、spanId、parentSpanId、requestId、callerRequestId、callerAlias、callerIdSource、attemptId、attemptNo、retryScope、callNo、logSeq、stage。按事件需要输出相关字段；可空字段不能以空字符串冒充有效 ID。未知计数用 null，观测到零才写 0。logSeq 在本地 span 内分配且并发安全，与已有流事件 seq 分开。

日志关联方式与 [OpenTelemetry Logs Data Model](https://opentelemetry.io/docs/specs/otel/logs/data-model/) 的 TraceId/SpanId 对齐，后续可映射到 OTLP；本期不依赖部署 Collector 或集中日志平台。

### 6.1 事件

首期固定三类最小访问关联记录，沿用基础访问日志开关，不受 DEBUG 控制。不得关闭详细诊断后仍把 usage、文本、清理结构等详细摘要塞入基础日志。

| 基础记录 | 输出时机与内容 |
| --- | --- |
| diag.process | 每个 worker 启动时一次；资源身份、构建/契约版本、pid、日志能力及开关。配置变化时记录新版本 |
| diag.server | 每次入站请求本地完成、失败或取消时一次；公共身份、路由模板、HTTP 提交状态、状态码、结束原因、耗时、本地调用数、诊断覆盖元数据 |
| diag.call | 每个实际出站调用结束时一次；公共身份、targetAlias、callKind、已知 attempt 身份、状态码、结束原因、耗时、对端诊断 ID |

这些记录是基础访问日志的结构化扩展：已有记录可补字段或替换为等价结构化记录，不重复输出同一事件。基础访问日志关闭、写入失败或进程崩溃时可能缺失；工具必须显示证据不足。标准模式不保证重建 87 token 的全部计数过程，需开启 DEBUG 或使用已有业务统计记录。不能把“trace 头一直传递”宣传为“所有细节一直可追溯”。

首期 DEBUG 只新增四类公共语义观察：request.normalized、upstream.attempt_finished（usage/错误来源）、response.converted（包括 deliveredUsage）、throttle.finished。下表是完整事件目录，其他事件优先复用现有事件/基础访问记录；未经明确需要，不在三个仓库逐一铺开。

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

- 分开记录 upstreamStatus、wireStatus、resultClass、deliveryState、failureOrigin、failureStage。HTTP 200 与内容成功不是一回事；开流后发生错误不能在日志里把实际 200 改写成 502。
- resultClass 包括 success、blocked、empty、incomplete、error、cancelled、unknown；附分类依据。仅无普通文本不能判 empty，工具调用和媒体可能是有效输出；未看到完整结尾不能写 success。
- 新公共字段 deliveryState=local_finished 仅表示本地写出完成，不证明 new-api 已读取、计费或展示；项目已有 deliveryOutcome 原值保留，通过映射输出。
- usage 分别保存 input、candidate、reasoning、outputTotal，以及 source、字段存在性和协议口径。OpenAI completion_tokens 可能已含 reasoning，不能再次相加；保留提供方原始字段和规范化结果。
- 不累计同一流重复到达的累计 usage；逐候选和末尾仅含 metadata 的帧必须被正确观察。重试 token 按 attempt 保存，最终响应 usage 与全链路尝试成本分别展示，不能混算。
- clientStreaming、upstreamStreaming、deliveryMode 分开记录，支持非流请求经内部流式收集、伪流、真实流。
- 总耗时、等待和速率使用各进程单调时钟；墙钟只用于定位。跨服务不能相减两台机器的时间戳来断言网络耗时。
- 记录 firstUpstreamByteMs、firstEffectiveOutputMs、responseCommitMs、firstDownstreamEffectiveOutputMs、totalMs。保活空白不算首个有效模型输出。
- 限速记录配置快照/版本、targetTokensPerSecond、选中的首字延迟、tokenCount/tokenSource、plannedWaitMs/actualWaitMs、elapsedMs、cancelled。速率必须附 token 分子和时间分母口径。

### 6.3 请求结构与重复短回复

结构摘要记录消息总数、末尾最多 4 条消息的原始位置/角色/part 类型/文本字节数、空消息数量、工具调用与工具响应数量。变换记录操作类型、位置、原因和变换前后末尾角色，不记录正文或工具参数。

文本摘要按候选分别统计普通文本 UTF-8 字节数；普通文本、思考、工具参数、媒体分开。首期通过各阶段 usage、字节数、结束原因定位 87 token 的产生位置；字节数相同不能证明内容相同。跨部署内容指纹/HMAC 密钥分发延后，不作为接入前置条件。既有 CLIProxyAPI SHA256 保留为 legacy 调试证据，不强制其他项目复制。

流式哈希按文本增量更新，避免累计帧重复计入；帧解析失败或截断须标 partial。工具只有计数，不采集工具参数哈希。指纹只是判断可见文本是否相同，不能反推出 87 token 内容或证明业务请求相同。

### 6.4 兼容映射与证据完整性

| 已有字段/语义 | 公共表示 | 兼容规则 |
| --- | --- | --- |
| Aitoapi requestId / request_attempt_id | requestId / attemptId | 复用身份，不改队列键 |
| deliveryOutcome=success（本地完成） | deliveryState=local_finished | 原值保留，只在已确认本地 finish 语义时映射 |
| seq / recentEvents[].seq | 原流序号；新 logSeq 单独分配 | 禁止用流序号去重日志 |
| logsDropped（logger 累计） | sinkDroppedTotal | 不冒充单请求丢弃数 |
| firstEffectiveMs | firstEffectiveOutputMs + timingSource | 保留旧字段，标明其时钟来源；新测量使用单调时钟，不把 Date.now() 的旧值标为单调时间 |
| CLIProxyAPI request_id | requestId | 旧文本短 ID 继续可查，非全局主键 |

logSeq 在记录实际构造后、入队前分配；超长、队列满和关闭 DEBUG 清队列都必须记丢弃原因。基础终局记录附 expectedLastLogSeq、可得的 droppedForSpan、debugCapture（none/interrupted/enabled_throughout/unknown）。异步写入后才发生的损失不假装已知，sink 计数只是辅助证据。

导入工具计算 debugCoverage：只有起止覆盖已知、声明序号连续、终局齐全且没有已知丢失时才为 full；已知关闭/丢失为 partial；从未开启为 none；其余为 unknown。缺少终局时无法证明尾部完整。进程崩溃及日志出口被截断无法靠应用内计数完全检测；任何“完整”都限于可观测的采集范围。

## 7. 日志级别、负载与数据边界

- 上下文提取/传递、响应 ID、本地必要状态常态有效，DEBUG 关闭不切断 trace。
- 新增详细诊断事件统一受本地 DEBUG 门控，沿用 Aitoapi 约定；外部 sampled 标志不能打开正文日志或详细诊断。基础访问记录必须携带关联字段，受基础访问日志开关控制，不能仅写成“可以增补”。
- DEBUG 关闭时不生成仅用于诊断的结构摘要和哈希。动态开启只完整采集之后新请求；动态关闭停止新输出，标记采集中断，不能宣称缺失记录代表阶段未发生。
- 不逐 token/chunk 输出日志。基础关联行目标不超过 2 KiB，详细行上限 4 KiB（含前缀），优先缩减可选字段；超长输出带同一身份/序号的截断存根，保留原因。复用已有日志出口和有界缓冲，不为三个项目各增加通用异步队列；基础记录不能放进会因关闭 DEBUG 而清空的队列。
- 专用诊断出口写纯 JSONL；与文本混流时使用固定 `@diag ` 前缀接完整 JSON，并通过同一进程的统一 writer 一次写入，不再套旧格式前缀。按进程分文件，或依赖已验证的运行时日志收集器。4 KiB 不是跨平台原子写保证，需针对实际多 worker stdout 测试；失败则调整出口，导入时保留坏行和来源，不静默忽略。
- 不采集 API Key、Cookie、OAuth token、签名、完整请求/回复、工具参数、图片或任意异常对象；目标仅记录配置别名和路由模板。上游错误优先稳定分类和白名单 reason，任意 message 不直接输出。
- 凭证使用服务内匿名稳定标识；数组索引只作临时定位，不当跨服务账号身份。
- 高基数字段如 traceId、requestId 不作为监控指标标签。诊断故障不得让正常模型请求失败，也不能形成无限缓存。
- 日志保留/轮转沿用部署配置，跨项目导出的诊断包默认只含结构化摘要。集中存储作为后续独立工作。

已确认 gcli2api 旧 WARNING 会打印 text/part 原值；只读诊断包导出仅选择 schema 匹配的 JSON 或 @diag 行，不附带旧原始日志。旧正文日志清理列为独立小修复，不能把旧日志也宣称为已脱敏。callerAlias/credentialRef 优先已有非敏感内部 ID；否则使用进程内随机映射并注明 boot 作用域，不为第一期引入跨部署共享盐或哈希真实 API key。

## 8. 定位流程和证据等级

查询工具第一期读取多个项目导出的 JSONL 或 @diag 记录。traceId 用于筛选候选节点，不能直接当作一条可信调用树。输入仅有调用方 ID 时按调用方作用域检索 callerRequestId，列出所有候选 trace；不可任意挑选一条。

导出包清单记录资源身份、构建版本、契约版本、日志时间范围、DEBUG 覆盖情况、丢弃计数和来源文件。文件名仅供阅读，不能作为实例身份的唯一依据。导入不同机器日志时保留来源，优先展示部署和实例，再展示错误汇总；一次请求失败数量与实际网络尝试失败数量分开统计。

离线边校验规则：调用方 diag.call 的 traceId/spanId 必须与接收方 diag.server 的 traceId/parentSpanId 匹配，且有唯一配对、无已知来源/对端冲突，才标 verified。它表示受控日志中的双边证据，不是密码学证明。只有接收方声明时为 external_parent_unverified，只有调用方时为 missing_peer；同一个 call 父关联多个接收请求时为 ambiguous_parent，不自动判为业务重试。网关透明重放、代理改写和缺失的中间节点均可能造成歧义。

对端响应诊断 trace 与调用方不一致时记录 context_mismatch，不仅凭这个差异断言发生了重启；需结合两侧记录再解释。相同 callerRequestId 为候选关联，仅时间/模型/usage 匹配为推测。未认证来源或未知调用方复用同一个 traceparent 时，展示独立的入站根候选，不能合并计费或重试次数。旧日志、尾部缺失、时钟偏移、DEBUG 未开和日志丢失都明确降低证据等级。

| 问题 | 必要证据 |
| --- | --- |
| 末尾 model 导致 400 | 同一 trace 与对应尝试中，接收/发送/清理前后结构，及上游错误 reason；定位空 user 首次出现和被删除的位置 |
| 重复输出 87 token | DEBUG 中各阶段 usage 来源、deliveredUsage、可见文本字节数/已有可用指纹；若各受控层均未产生 87，只能将范围缩小到调用方计数/展示，不能断言其算法。未开启详细诊断则报告证据不足 |
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

既有业务 attempt 只作 call 属性，不另建 span。传输包装负责单次实际调用的 call span；高层执行器只标注 attempt 身份和已知语义，不能重复创建同一个 call。连接鉴权、额度同步等非模型后台请求不能误归入模型尝试。同步发起的辅助调用要明确 callKind；脱离请求的后台任务必须脱离原请求上下文。

公共接入不要求重构路由、模型选择、凭证选择、重试循环、计费统计或流式发送主流程；禁止全局 monkey patch、运行时函数替换及新增控制中心依赖。采集模式关闭时详细 observe 为 no-op，基础 trace 传递继续工作。

每个实现提交列出“新增模块、既有文件接入点、为什么中间件无法替代该观察点、覆盖协议”。评审重点检查业务主流程 diff；修改散布到多个同类路由时，先收敛到共享边界。日志能力分批提交，400 业务修复独立提交，方便分别回退。

### CLIProxyAPI

- `internal/logging/requestid.go`、`gin_logger.go`：公共上下文和入站提取，保留旧 request_id。
- `internal/logging/global_logger.go`：公共字段白名单或结构化诊断出口，防止字段被格式化器丢弃。
- 优先在共享 HTTP client 完成代理/压缩/缓存等配置后的最后装饰边界挂载追踪 RoundTripper，例如 `internal/runtime/executor/helps/proxy_helpers.go`；以 finalizeClient 之类的统一出口覆盖不同返回分支。不能提前包装 context 中的 transport，否则可能破坏 Devin 路径的类型断言；不修改缓存 transport，nil 时包装默认 transport。帮助函数放 `internal/runtime/executor/helps/`。
- 自定义 transport、WS 等不经过该构造器的路径单独列入覆盖矩阵，通过自身共享边界适配，不能声称一次包装覆盖全部执行器。保留已有 usage transport 的顺序和行为，尤其已有 MarkUpstreamAttempt 不能因诊断再次累加。保持必要的 CloseIdleConnections/流式接口能力；传输未提供的能力不虚构。
- 请求接收/完成使用入口及现有生命周期；转换后和最终输出在已有公共转换/发送边界增加摘要观察，必要时少量协议专用钩子；限速只在实际限速实现边界观察生效值。
- 先验证当前 Gemini→Antigravity 路径，再以相同抽象补齐其余协议。支持矩阵明确标记未覆盖的执行器，不能把部分接入宣称为全覆盖。

### gcli2api

- 纯 ASGI 入口中间件与日志层建立上下文，关注 StreamingResponse 生命周期和 ContextVar 隔离；不在每条路由重复设置/清理上下文。
- `src/httpx_client.py` 的共享请求/流式生命周期和 httpx hooks 观察出站头/状态；正文结束在既有读取边界观察，hook 本身不预读。不能以替换底层 transport 的方式丢失 proxy/mount 配置。Antigravity 的共享规范化、重试决策和收集函数添加少量语义观察；只读取已选定的匿名凭证身份。
- 接收直接 new-api 调用与 CLIProxyAPI 调用使用同一中间件。
- 通用上下文可复用；本期业务诊断与消息清理修复聚焦 Antigravity，不顺带扩展已停止维护的 GeminiCLI 业务。

### Aitoapi-custom

- Express 入口中间件建立上下文；在 `src/core/RequestHandler.js` 既有 `_startTrackedRequest`、attempt 初始化/切换及结算方法集中映射 requestId/attemptId，避免在各协议处理方法重复接入。不改变队列、取消、统计归因身份。
- `src/utils/LoggingService.js` 扩展公共字段白名单，保持 DEBUG 门控、有界队列和动态关闭规则。
- 第一期保留浏览器 WS 协议，通过既有 attemptId 关联服务端派发与回报；共用 flush 只能输出入队前已快照的字段。浏览器子 span 延后，迟到 ACK 的业务校验不变。
- 适配现有 generation.* 结果、usage、浏览器阶段，不改已约定的管理 API requestId 或审计语义。未来管理追踪另行定义。

## 10. 分期与验收

1. **公共契约与共享测试向量**：冻结身份/头规则、双边关联、Aitoapi 映射、三类基础记录、四类 DEBUG 观察；不要求三语言共用源码或新增微服务。
2. **被调方先接入**：gcli2api、Aitoapi 入口、最小访问记录及既有生命周期适配；gcli2api 观测上游 HTTP，Aitoapi 观测现有 WS 派发，不改浏览器协议。
3. **CLIProxyAPI 接入**：入口、最终 HTTP client 装饰点、受控目标传播、响应诊断头，验证 usage transport 与重试统计不变。三个项目均可独立作为入口，不限定未来传播只能发生于 CLIProxyAPI。
4. **按需补齐四类业务证据并联调**：转换、usage、限速及 Aitoapi 既有事件适配；离线工具核对两端证据，展示实例、调用树和缺口。已确认的消息清理顺序/空白 part 修复、旧正文日志整改各自独立提交，不等待日志改造。
5. **可选后续**：浏览器独立 span、跨部署指纹、new-api 主动发送标准上下文、OTLP、集中查询和告警。实施前再评估收益，不作为首期依赖。

必须用本地假上游验证，不依赖生产凭证或真实模型调用：

- 所有目标拓扑；入口无 ID、仅 X-Request-Id、有合法 traceparent、多种头并存、坏头及重复 ID。
- 多个客户端复用同一 traceparent，两个调用方使用同一请求 ID：按来源列出独立候选，缺双边 call 证据时不构造已验证重试关系。
- TRACE 头旧白名单透传、同名大小写、重复头、错误响应透传、同主机不同端口/路径、跨 origin 重定向：只在配置目标传播且字段不重复，原业务头语义不变。
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
- DEBUG 全关仍能以基础访问记录关联实例与调用，但不出现详细 usage/清理内容；基础访问关闭则明确缺口。DEBUG 切换、日志过载、记录截断可反映完整性，现有 seq/deliveryOutcome 含义不变。
- 多 worker stdout 行完整性、按进程文件轮转、导出仅收结构化诊断；无敏感字段、无每 chunk 日志、无诊断引入的提前 HTTP 提交。
- 旧响应解析器忽略新增头；原协议正文、状态码、管理契约保持兼容；未改造节点导致的链路缺口明确显示。

每个仓库完成后独立提交，注明契约版本及已覆盖协议；全部目标链路通过假上游联调后再安排发布。当前文档不代表三个项目已经实现或部署。


## 11. DIAG-00 契约候选与验收接口（2026-09-25）

本次将完整方案、Claude 原始意见/快照和八任务规范纳入本任务分支；未实施业务诊断。
候选契约为 [contracts/diagnostics/v1](contracts/diagnostics/v1/README.md)，交换标识不变，制品版本 `1.0.0-rc.1`。
**尚未正式冻结**：协调窗口必须先检查本提交代码/文档，再将完整修订方案送 Claude 审核；准确 HEAD 和 SHA256SUMS 摘要均通过后才能向依赖任务派发。历史 Claude 审查不能替代本轮。

### 11.1 本次消除的歧义

- JSON Schema 固定公共平面身份字段＋事件 `data`，增加显式 `spanKind`、`recordKind`、本地 `serverSpanId`。所有对象封闭，基础 data 不接受 usage/结构/限速细节；已有项目日志不必改成此形状，由适配器投影。
- `serverSpanId` 表示本地拥有者，call 的 parentSpanId 指向它；业务 attempt 不另建 span。父 server 缺失单独显示，不据此丢掉可校验的跨服务 call/server 证据。
- 入站适配须由既有鉴权和本地配置确认，出站 DIAG_PEERS 不构成入站身份认证。callerAlias 增加 deployment/boot/unknown 作用域，boot 别名跨重启不可混同。
- traceparent 重复值拒绝；tracestate 多字段合法合并与重复 key 拒绝分别验证。绑定 W3C Level 1 2021，输出 00 并保留 sampled 位，新根 00；未知位清零。标准头只在允许目标传播，绝不记录原值。
- DIAG_PEERS 为显式 JSON 数组，定义精确 origin、默认端口、路径分段边界、重定向逐跳重检；相交配置、无效配置整体禁用诊断传播，不改变模型请求。编码路径保守不传播，具体限制与有效/无效向量见契约。
- X-Diag-* 出站最后 Set、响应正常提交点替换；X-Request-Id/X-Trace-Id 原有含义保留。第三方路径阻止自动继承诊断头，提供方显式旧约定仍由其适配器负责。
- 双边唯一配对还要核对已知 service/deployment、响应 ID、受控来源；多父、重复事件内容、来源冲突、循环均不能升级 verified。缺接收端记录时实际实例未知。
- 入队前 logSeq 包含基础与 DEBUG、终局自身；expectedLastLogSeq 等于终局 logSeq。缺终局不能证明尾部完整，截断存根有相同身份和序号；sinkDroppedTotal 不冒充单请求丢弃。full 仅是声明采集范围内完整。
- Aitoapi 保留旧 request/attempt/seq/deliveryOutcome/schemaVersion，公共投影采用新 logSeq/deliveryState；旧 wall-clock 和浏览器耗时标来源，浏览器子 span/WS 改动延后。
- 导出只选通过公开 schema 的 JSONL/@diag 记录。坏行仅导出位置/长度/原因，原文留本地，避免夹带旧 WARNING 正文。schema 不能证明值本身没有秘密，生产者还须限制字段来源。

### 11.2 制品与复现

契约包含三份 JSON Schema、有效/无效 fixtures、合成脱敏 JSONL、头/来源/配置/双边关系/完整性/Aitoapi 映射共享向量、离线验证脚本和版本文件。
全部 UTF-8 无 BOM、LF；SHA256SUMS 对目录内制品逐文件计算 SHA-256（自身与 Python 缓存除外），契约摘要为清单原始字节 SHA-256。源码提交 SHA 单独记录，避免自引用。普通验证不改摘要。

`python contracts/diagnostics/v1/validate.py` 验证 schema、样例、向量和清单；依赖任务必须将同一向量应用到其实际 Go/Python/Node 接入实现，不能把 DIAG-00 的测试 oracle 当生产实现或联调通过。

### 11.3 八任务约束保持不变

DIAG-00 契约；DIAG-01 清理顺序/空白和旧正文 WARNING 独立修复；DIAG-02 gcli 接入（依赖 00/01）；DIAG-03 Aito 适配（依赖 00）；DIAG-04 CPA 基础（依赖 00）；DIAG-05 CPA 重点语义（依赖 04）；DIAG-06 离线工具（依赖 02/03/05）；DIAG-07 多实例联调（依赖 02/03/06）。
最大三个开发任务并行，每项新建任务和隔离 worktree；不复用旧任务、不在子任务派发其他任务、不跨仓库写入。退回原窗口修订，HEAD 改变重新审核。只在批准准确 HEAD 后由协调窗口放行推送任务分支；不合并主分支、不部署、不调用生产模型、不碰真实凭证/数据库/Volume。new-api 不改；gcli2api 不更新 panel-version.txt，钩子面板询问选 n，不恢复 GeminiCLI/旧管理项目。

本次不证明任何运行时协议已覆盖；目标仍为 Gemini→Antigravity 与 Aitoapi 既有语义，真实流/非流/流转非流、取消、并发、多实例、重启、usage87 与缺失/零值、限速及行为等价由后续八任务矩阵完成。不得借诊断重写路由、账号/模型选择、重试、转换、发送、超时或计费。
