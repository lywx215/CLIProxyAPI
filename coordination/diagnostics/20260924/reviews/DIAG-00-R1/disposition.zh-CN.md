# DIAG-00 R1 修订处置：待协调与 Claude 复审

R1 审查对象：`583201f29c8b4ef65391bdf605ba9d6a2c12f08b`；初始锁定基线：`fde3903689b8c9882d1f0233587fe33e2edfdf77`。
审查结论：request_changes，无 P1，5 项 P2；实际模型由协调方确认是 `claude-opus-5-5`。
[完整审查原文](review.zh-CN.md) 来自只读源工作区，源文件 SHA-256 为 `a387975f6acd85b8259b68035c5df8de93afacae3bef7a5b7b841f4002f370da`。提交副本仅规范化原文空白行末尾空格与末尾换行，以通过 git diff --check；正文和行数不变。未复制包含其他项目源码的审查 prompt。

本次开发窗口仅按协调取舍修订候选，没有调用 Claude 或批准自身修改。准确新 HEAD 与摘要见本轮交付回复和修订报告；原 HEAD 审查不自动批准新提交。

## P2 逐项处理

| 意见 | 取舍与修改 | 对应证据 |
| --- | --- | --- |
| P2-1：OWS 向量边界不真实 | 接受。应用输入明确为框架解析后的逐字段值，协议 OWS 已剥离；应用不再额外 trim，限长只测可观察值。headers 向量区分 framework_fields/parser_unit；不要求绕过框架读取 socket | http-ingress 6 例通过 h11 0.16.0 真实解析，覆盖 `X-Request-Id:  a `→`a` 与 traceparent 前 600 个 wire 空格被剥离；解析器未剥离空格负例单独标记 |
| P2-2：未来版本逗号拼接被误接受 | 接受本地 profile 拒绝任何逗号。多行字段必须保留；Node 对 traceparent 和 tracestate 都用 rawHeaders/headersDistinct，不用 req.headers 合并值 | future-duplicate-field-lines、future-comma-combined、future-extension-comma-local-profile；真实 HTTP 重复/合并未来头；逐字段 tracestate 512/513 边界 |
| P2-3：混合时钟单一来源标签 | 采用较低侵入方案。公共 timingSource 固定 server_monotonic，只收可靠的新单调时钟观察，缺失 null；旧 firstEffectiveMs/browserDurationMs 及无法确认来源的旧指标保留原日志/业务统计，不投影 | mixed-legacy-clocks-never-project-to-monotonic；schema 拒绝 legacy_wall/browser_reported 和 browserDurationMs 混入；混合输入只投影显式新单调指标 |
| P2-4：完整性优先级不一致 | 接受。已知丢失/截断先 partial；否则未知 accessCapture 或 none 与完整终局矛盾先 unknown，即使 debugCapture=none。另检查终局序号矛盾 | debug-none-access-unknown、debug-none-access-none-terminal-contradiction、debug-on-access-none-terminal-contradiction、terminal-sequence-contradiction、known-loss-precedes-unknown-access |
| P2-5：最终 hook 猜不出头来源 | 接受源头控制＋最终 peer 替换，保留必要自身拥有权。盘点实际调用来源，只过滤已证实的自动复制点；不把 optional extra_headers 白名单当自动泄漏证据。去掉 providerOwnedTrace 输入，用诊断模块注入时设置的请求本地标记。重定向先清本模块旧头、再已有业务构造、最后实际 URL 重检 | copy-source、outbound、redirects 向量；非 peer 显式业务头保留、同值不猜来源、自身标准头离开允许目标必清除；DIAG-02/04 和 DIAG-03 实际出站适配要求完整来源清单 |

P2-5 没有照搬 Claude 建议中的“非 peer 永远不删除标准追踪头”：这会留下前一跳诊断层注入的上下文。采用协调补充的最小本地拥有权和明确先后顺序，覆盖跨 origin 和同源路径外跳转；不能保留元数据/清理顺序的路径不注入并报告未覆盖，不改变既有业务重定向策略。

协调方已检查 gcli2api 批准基线：`src/api/antigravity.py` 的可选 extra_headers 白名单支持 traceparent/tracestate，但当前三个 Antigravity 路由调用未见传入入站 headers。本任务将此作为协调方证据边界记录，不把它表述为本任务完整代码审计或已确认泄漏。具体接入任务仍须盘点真实调用参数；没有自动复制的路径不做额外修改。

## P3 逐项处理

| 编号 | 处理 |
| --- | --- |
| P3-1：not_configured 无产出定义 | 删除 schema 枚举；未启用适配器不采集该头，拒绝字段为 none。增加无效枚举 fixture |
| P3-2：非 peer 响应 ID | 仅 peerConfigured=true 采集 X-Diag 响应 ID；其他值忽略为 null/none。schema、peer-response 向量及任务规范一致；本地响应提交仍清除透传的 X-Diag-* |
| P3-3：旧流式哈希段落 | 删除当前完整方案的新增流式哈希/指纹要求。v1 不增加或投影模型哈希；历史原方案快照仅作为历史保留 |
| P3-4：历史审查标题 | 当前方案明确历史审核不等于当前批准；审查文档标题和开头注明历史/当前区别，R1 结论为 request_changes |
| P3-5：raw usage 白名单 | 增加 OpenAI total_tokens（Chat/Responses）和 Gemini cachedContentTokenCount；schema 仍按协议限制，含零值有效/跨协议无效 fixtures。只是只读保留原值，不更改业务计数或 normalized 算法 |
| P3-6：终局截断存根 | diag.server/diag.call 存根证明终局构造过：terminalMissing=false、debugCoverage=partial；不等同完整终局，不证明成功或 verified。其他事件存根不证明终局存在。coverage 增加独立 terminalStubCount 与两个用例 |
| P3-7：错误实例配置的识别承诺 | 删除“仅靠身份总能识别错误配置”的承诺；同 instanceId 不同 bootId 正常多 worker/重启也会出现。始终保留不同进程，额外矛盾证据才报冲突 |
| P3-8：httpx 丢失原始点段 | 匹配最终客户端将发送的 destination origin 与 escaped request-target；不恢复原始拼写。仍有点段的实际路径不传播；已规范化路径按可观察值匹配，向量标 final_request_target |
| P3-9：消费仓库换行 | README 和八任务公共规范要求消费仓库设置实际复制路径的 text eol=lf，并检查工作区/Git index 字节 |
| P3-10：Node tracestate 合并差异 | 与 P2-1/2 一并处理，原逐字段值用单个逗号组合、不人为加空格；6 个真实 HTTP 用例之一精确验证 512 字节边界 |

全部意见已按协调要求修订，无本轮主动延后项。实际 Go/Node 框架接入、三服务并发/取消/重定向和运行时拥有权仍属于后续任务；h11 内存解析不是跨项目联调通过声明。
