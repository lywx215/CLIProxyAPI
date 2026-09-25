# DIAG-00 独立审查（Claude）

## 审查对象与结论

- **HEAD**：`583201f29c8b4ef65391bdf605ba9d6a2c12f08b`
- **基线**：`fde3903689b8c9882d1f0233587fe33e2edfdf77`
- **契约摘要**：`31221d21a265eab37572024c96337ecf5e9d4bfe064ecb13d3cafba98d16a435`
- **结论：request_changes**
  - 未发现 P1。
  - 有 5 项 P2，均为契约自身的规则矛盾或无法在真实边界落地的向量，修复量都很小。
  - 按"未解决 P1/P2 不放行"的规则，需要修订后针对新 HEAD 复审。

我没有运行工具。下文结论依据附件文本，以及对 validate.py 逻辑的逐行推演。

## 已核对、无问题的部分

- **协调方预检查项**：未来版本 traceparent 中的空格问题，README L89–94、`validate.py:93-100` 和三条向量三者一致：
  - 固定 55 字符前缀内出现空白则拒绝（`embedded-whitespace`）；
  - 扩展段接受空格（`future-opaque-extension-space`）；
  - 扩展段拒绝制表符（`future-opaque-tab-...`）；
  - 512 字节上限在去除外层 OWS 之前计量（`raw-ows-over-local-limit`）。

  这一项本身已闭合。但它与 P2-2 叠加后会产生新问题。
- **tracestate**：key/value 语法与 W3C Level 1 的 ABNF 一致。以下规则均有向量覆盖：多个字段按顺序合并、重复 key 时整体丢弃、traceparent 无效时连带丢弃 tracestate。
- **DIAG_PEERS**：以下规则在向量中完整，与 README 一致：
  - 精确 origin 匹配，默认端口等价；
  - 按路径分段边界匹配，大小写敏感；
  - 前缀重叠或任一条目无效时整体失效；
  - 拒绝重复的 JSON key。
- **span 模型**：server/call 两类 span、`serverSpanId` 的所有者规则、attempt 作为业务属性，三处（方案、README、schema）一致。计数向量中"2 外层 + 6 内层 = 8 call"的口径清楚。
- **封闭 schema 的隐私边界**：基础记录不接受 usage、结构或限速字段；带 FORBIDDEN 哨兵的负例覆盖了嵌套对象；usage 的"未知"与"零"区分明确。
- **与用户批准范围的一致性**：历史 Claude 意见中被拒绝的项目没有被重新引入，包括常态 deliveredUsage、仅按主机名的 allowlist、固定 sampled=01、WS 协议改动和 HMAC。

---

## P2（需修订）

### P2-1 自定义 ID 和 traceparent 的 OWS 向量无法在真实 HTTP 边界复现

**位置**
- README L89–93："Only HTTP outer OWS is removed … measures its 512-byte cap before stripping outer OWS"。
- README L97："Do not trim custom IDs"。
- `vectors/headers.json` 中的 `id-space`（`" a "` 期望 `invalid`）、`raw-ows-over-local-limit`、`outer-http-ows`。
- `validate.py:60-64`、`:93-95`。

**冲突**
- RFC 9110 规定，字段值的首尾 OWS 不属于字段值本身。
- 三种运行时在交给应用之前都已经去掉了首尾 OWS：Go 的 `net/textproto`、Node 的 llhttp、Python 的 h11 或 httptools。
- 因此，真实入口收到 `X-Request-Id:  a ` 时，框架交给应用的值是 `"a"`，结果为合法；向量却期望 `invalid`。
- `raw-ows-over-local-limit` 中的原始长度（420 个空格加 55 个字符）在应用层同样观察不到，框架给出的是 55 个字符的合法值，结果为 accepted；向量期望 `invalid_replaced`。
- 任务说明要求"runtime 测试须针对真实处理边界"。在这个要求下，DIAG-02/03/04 要么无法通过这几条向量，要么只能绕开框架去解析原始 socket，而后者违背低侵入原则。

**修正**
- 把向量输入定义为"框架解析后、按字段行保留的值列表（已去除首尾 OWS）"。
- 删除或改写 `id-space` 和 `raw-ows-over-local-limit`，512 上限改为对框架给出的值计量。
- 自定义 ID 的规则改为"应用层不再额外 trim；值内部出现空白则拒绝"。

### P2-2 未来版本 traceparent 在"逗号拼接"时与"多值无效"规则矛盾，且结果因语言而异

**位置**
- README L80："Multiple traceparent values (even identical or comma-joined) are invalid"。
- README L91–92：扩展段是不透明内容，允许 ASCII 空格。
- `validate.py:94-97`：扩展段只检查可打印 ASCII，不排除逗号。

**可复现输入**
```
[["traceparent","01-11111111111111111111111111111111-1111111111111111-01-x,01-11111111111111111111111111111111-2222222222222222-01"]]
```
- oracle 的输出是 `accepted`、parent=`1111111111111111`。
- 按 README L80，这是逗号拼接的多值，应判为 `invalid_replaced`。
- 现有的 `comma-joined` 向量只覆盖了 version 00（00 版本不允许扩展段，所以碰巧被拒），没有暴露这个问题。

**跨语言后果**
- Node 的 `req.headers` 会把两行 traceparent 拼成 `"<v1>, <v2>"`。
- 在 00 版本下，这个值会被正确拒绝。
- 在未来版本下，扩展段允许空格和逗号，于是拼接值被接受，parent 取第一行的值。
- Python（ASGI 原始列表）和 Go（`[]string`）则会判为重复。

**修正**
- 在本 profile 中，扩展段禁止出现 `,`。依据是 RFC 9110 的列表语义：逗号等价于多个字段行。
- README 写明：Node 必须使用 `headersDistinct` 或 `rawHeaders`。
- 增加一条"未来版本 + 重复行"向量和一条"未来版本 + 扩展段含逗号"向量。

### P2-3 `timing` 只有一个 `timingSource`，无法表达 Aitoapi 映射表要求的混合时钟来源

**位置**
- `record.schema.json` 的 `$defs.timing`：四个时间字段共用一个 `timingSource`。
- README L327–329：`firstEffectiveMs` 标 `legacy_wall`，`browserDurationMs` 标 `browser_reported`，新观测标 `server_monotonic`。
- `validate.py:376`：只输出单个 timingSource，两个旧字段同时出现时取 browser。

**冲突**
- 同一个 Aitoapi attempt 同时有新测的 `firstUpstreamByteMs`（单调时钟）和旧的 `firstEffectiveMs`（墙钟）时，schema 只允许写一个来源，必然有一个字段被标错。这违反方案 §6.4 中"不把 Date.now() 的旧值标为单调时间"的规定。
- `browserDurationMs` 在公共 schema 中没有对应字段。映射表声明它会被投影，实际却无处可放。

**修正（二选一，推荐第二种，更低侵入）**
- 改为按字段标注来源（例如 `{value, source}`）。
- 或者规定一个 timing 对象只能承载单一来源，其他来源的字段必须为 null；旧的墙钟值和浏览器耗时留在旧日志中，不进入 v1 投影；同时删除映射表中无法落地的 `browserDurationMs` 一行。

### P2-4 `debugCoverage` 的优先级在 README 与 oracle 之间不一致

**位置**
- README L273–277：先判"known loss ⇒ partial"，再判"missing/contradictory terminal 或 **unknown capture**/counters ⇒ unknown"，然后才判 "debugCapture=none ⇒ none"。
- `validate.py:355-361`：unknown 分支只检查 `debugCapture`，不检查 `accessCapture`；`accessCapture` 直到 none 分支之后才判断。

**可复现输入**
```
sequences=[1], expectedLastLogSeq=1, terminalCount=1, debugCapture="none", accessCapture="unknown", droppedForSpan=0, truncatedEvents=0
```
- oracle 输出 `none`。
- 按 README 应为 `unknown`。

另一个问题是 `accessCapture="none"` 却存在基础终局记录，这本身就是"contradictory terminal"，oracle 同样给出 `none`。README 自己规定三者不一致即阻断批准。

**修正**
- 让 oracle 的 unknown 分支同时覆盖 `accessCapture ∈ {unknown, none}`；或者在 schema 中禁止终局记录出现 `accessCapture=none`。
- 补两条向量。

### P2-5 第三方目标的"剥离继承头 + 保留提供方显式头"在最终 hook 处无法实现

**位置**
- README L129–134。
- 方案 L114。
- `vectors/outbound.json` 中的 `providerOwnedTrace` 输入。
- `validate.py:134-137`。

**问题**
- 规则要求最终出站适配器"在既有业务头构造完成后"处理头：剥离从入站继承来的 traceparent/tracestate，但保留提供方适配器显式设置的追踪头。
- 在最终 client hook 处（Go 的 RoundTripper、httpx 的 event hook），请求副本上只有一个 `traceparent` 值，不带任何来源信息。
- 向量里的 `providerOwnedTrace` 是测试直接喂入的标志，运行时无法推导出来。

**可复现场景**
- gcli 的旧白名单先把入站 traceparent 复制进请求头，随后某个提供方分支也设置了 traceparent。
- hook 只能看到一个值：要么全删（可能破坏提供方约定，违背"保留旧头"），要么全留（泄露入站上下文）。
- 另外，契约中没有任何任务被要求盘点"哪些现有代码会向非 peer 目标写入 traceparent/tracestate"。

**修正（同时也更低侵入）**
- 对"旧白名单把入站追踪头复制给上游"的问题，在白名单源头做一行改动：从中移除 traceparent、tracestate 和 X-Diag-*。
- 最终 hook 只做两件事：
  - 对 peer 目标，替换或设置追踪头；
  - 对非 peer 目标，只剥离 X-Diag-*，traceparent/tracestate 视为业务代码所有，不再动。
- DIAG-02 和 DIAG-04 的交付中加入"现有追踪头写入点清单"。
- outbound 向量的非 peer 用例相应改写，去掉无法落地的 `providerOwnedTrace`。

---

## P3（建议，不阻断）

1. **`rejected` 和 `peerIdRejected` 中的 `not_configured` 没有定义产出条件。** oracle 在 `untrusted-diag-ignored` 用例中输出 `none`。应当定义它（例如"未配置适配器时出现 X-Diag-Request-Id"），或者删除这个枚举值。
2. **对非 peer 目标是否读取响应中的 X-Diag-* 未作规定。** 建议仅在 `peerConfigured=true` 时采集，第三方的响应值一律记为 null。
3. **方案 L184 仍在描述"流式哈希……指纹"，与 README L235"本版本不新增文本指纹/哈希"矛盾。** 建议删除 L184，以免 DIAG-05 按方案实现。
4. **方案 L7 "已完成 Claude 独立审查并按意见修订"容易被误读为当前版本已获批准。** 建议加限定语"历史审查"。
5. **usage 的 raw 白名单缺少 OpenAI 的 `total_tokens`（`openai_chat`）和 Gemini 的 `cachedContentTokenCount`。** 如果 87-token 定位需要与平台计费口径对比，建议补上。
6. **截断存根的归属未定义。** 被截断的 `diag.server` 存根缺少 coverage 字段，应明确它算作"终局存在"还是"coverage=unknown"。
7. **方案 §10 称"实例身份错误配置时暴露冲突"，但同一 instanceId 下出现多个 bootId 与多 worker 的正常情况无法区分。** 建议删除这一承诺，或把它降级为"不影响唯一性"。
8. **README L180 "Inspect the actual outbound URL before lossy normalization" 与 httpx 的实际行为不符。** httpx 在 hook 之前就已归一化点段，无法观察到归一化之前的 URL。建议改为"以实际发送到线上的 request-target 为准"。
9. **消费仓库需要同步 `.gitattributes`。** 消费方按字节复制契约后，gcli2api 和 Aitoapi 也必须加入 `contracts/diagnostics/v1/** text eol=lf`；否则在 Windows 下 `core.autocrlf=true` 会导致 SHA256SUMS 不一致。开发机正是 Windows。
10. **Node 的 tracestate 拼接差异。** `req.headers` 用 `", "` 拼接，会让 512 字节的边界值与原始的 `","` 拼接相差 1 字节。这一条与 P2-2 一并写明，使用 `headersDistinct` 即可解决。

---

## 更低侵入方案评估

当前范围基本合理，不建议推翻：

- 首期只有三类基础记录、四类 DEBUG 观察点和双边校验，本身已是历史审查中侵入最低的方案 A。
- 业务代码只在入口中间件、最终 client 装饰点和少量语义边界接入。
- 其余复杂度集中在离线 oracle 和 DIAG-06，这两处都不在服务运行路径上。

在此基础上，建议做三处收窄，都能进一步降低侵入：

1. **白名单修正放在源头（对应 P2-5）。** 在白名单源头做一行改动，替代在 hook 中推断头来源，避免为三种语言各写一套来源追踪。
2. **向量输入改为框架解析后的逐行值（对应 P2-1、P2-2）。** 实现方无需绕开框架去读原始字节，Node 只需改用 `headersDistinct`。
3. **Aitoapi 的公共投影只收新的单调时钟测量（对应 P2-3）。** 旧的墙钟值和浏览器耗时留在现有的门控日志中，可减少一张映射表和一类 schema 字段。

## 放行条件

1. 修正 P2-1 至 P2-5，并补齐对应向量。
2. 重新生成 SHA256SUMS，报告新的 HEAD 和契约摘要。
3. 对新 HEAD 复审；协调方重新执行全部 schema、fixture 和向量校验。

P3 可以在同一次修订中顺带处理，也可以在记录处置理由后延后。
