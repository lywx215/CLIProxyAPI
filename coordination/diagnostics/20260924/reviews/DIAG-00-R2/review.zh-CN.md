# DIAG-00 R2 独立复审（Claude）

## 审查对象与结论

- **HEAD**：`990d5ee75fd0878dc1f24753cccf3000701a94ea`
- **上一轮被退回的 HEAD**：`583201f29c8b4ef65391bdf605ba9d6a2c12f08b`
- **契约摘要**：`26904145b3f1713577f41cd4bbd588ffe48b5f2670b91705537e386f1aafd57a`
- **结论：approve**
  - 未发现 P1 或 P2。
  - 下文 P3 均为不阻断的澄清项，可在冻结前顺带处理，也可记录理由后延后。

**依据与未核对项**
- 本轮没有运行工具。结论依据附件全文，以及对 `validate.py` 各 oracle 的逐行推演。
- 以下事实我没有独立核对，采信协调方的复核结果：
  - Git 字节与 72 个文件哈希；
  - 242 个向量的执行结果；
  - 6 个 h11 内存解析用例的执行结果。
- 如果冻结时任何字节发生变化，本结论失效。

---

## 一、R1 五项 P2 的闭合情况

### P2-1 OWS 边界：已闭合

- **README 的定义**：README L86–92 将应用输入定义为"框架解析后的逐字段值"。README L104–105 和 L112–114 明确：
  - 应用不再额外 trim；
  - 512 字节只对可观察值计量。
- **向量分层**：`headers.json` 每条向量都带 `boundary` 标记，`validate.py:507` 强制检查该标记。原来无法落地的两条用例已改名并标为 `parser_unit`，不再冒充线缆行为：
  - `parser-unit-unstripped-outer-ows`
  - `parser-unit-observable-ows-over-local-limit`
- **真实解析器验证**：`http-ingress.json` 通过 h11 真实解析验证了三点：
  - `X-Request-Id:  a ` 解析为 `a`；
  - 600 个线缆空格不可观察；
  - 内部空格判为 invalid。
- **与 h11 行为一致**：h11 的 `header_field` 正则在首尾剥离 OWS，并将字段名转为小写。fixture 中期望的字段名均为小写，与此一致。

### P2-2 未来版本 traceparent 的逗号与多值：已闭合

- **oracle**：`validate.py:95` 新增 `"," not in value` 检查。
- **README**：README L107–108 写明，本地 profile 在任何位置都禁止逗号，包括未知扩展段。
- **向量覆盖**：R1 给出的可复现输入现在得到 `invalid_replaced`。
  - 重复字段行：`future-duplicate-field-lines`，以及对应的 h11 用例；
  - `", "` 拼接：`future-comma-combined`，以及对应的 h11 用例；
  - 扩展段内的逗号：`future-extension-comma-local-profile`。
- **Node 要求**：Node 的 traceparent 与 tracestate 都必须使用 `headersDistinct` 或 `rawHeaders`（README L88–90）。
- **tracestate 边界**：tracestate 按逐字段值以单个 `,` 组合后计量 512 字节，512 与 513 两个边界各有 framework 向量和 h11 向量。

### P2-3 混合时钟：已闭合，且采用了更低侵入的方案

- **schema**：`$defs.timing.timingSource` 为 `const: server_monotonic`，并设 `additionalProperties:false`。
- **负例 fixture**：三个 fixture 分别拒绝 `legacy_wall`、`browser_reported` 和混入的 `browserDurationMs`。
- **映射 oracle**：`mapping_oracle` 只投影显式传入的 `monotonicMeasurements`。在 `mixed-legacy-clocks-...` 向量中，旧字段同名的 `firstUpstreamByteMs:99` 没有被取用。
- **映射表**：映射表中无法落地的那一行已删除（README L392–393）。
- **计时器**：没有要求为填充字段新增计时器（README L409）。

### P2-4 完整性优先级：已闭合

- **oracle 逻辑**：`coverage_oracle` 的 unknown 分支现在同时覆盖以下情况：
  - `accessCapture ∈ {none, unknown}`；
  - 终局数量不为 1；
  - 终局序号矛盾。
- **README 一致**：判断顺序与 README L333–347 一致，并补充了 5 条向量。
- **可复现输入的新结果**：
  - R1 的可复现输入（`debugCapture=none`、`accessCapture=unknown`）现在得到 `unknown`；
  - `known-loss-precedes-unknown-access` 保证已知丢失优先判为 partial。
- **终局截断存根**（`terminalStubCount`）的处理自洽：
  - 输出 `terminalMissing=false`、`partial`；
  - 非终局事件的存根不证明终局存在。

### P2-5 头来源与重定向拥有权：已闭合

- **不再依赖魔法输入**：去掉了 `providerOwnedTrace`。`outbound.json` 中的 `diagnosticOwned` 只由本模块注入时设置。`nonpeer-no-value-based-ownership-guess` 确认不按值猜测来源。
- **源头过滤范围**：只对已证实的自动复制点做源头过滤（README L147–155，以及 `copy-source` 向量）。
  - 没有把 optional `extra_headers` 白名单当作泄漏证据；
  - 与协调方已核对的 gcli 证据边界一致。
- **重定向顺序**：`redirect_oracle` 每一跳的处理顺序是：
  1. 清理本模块拥有的标准头和全部 X-Diag-*；
  2. 清除拥有权标记；
  3. 追加 `businessHeaders`；
  4. 按实际目标重检。

  同时，`require(... == step["allowed"])` 把 `allowed` 字段与 `peers_oracle` 的实际匹配结果绑定，防止 fixture 自说自话。四条重定向向量覆盖：
  - peer→第三方；
  - peer→同源但未允许的路径；
  - peer→peer（生成新 call span）；
  - 清理后由新提供方写入的业务头被保留。
- **未采纳 R1 的原建议是正确的**：R1 曾建议"非 peer 永远不删除标准头"，协调方没有采纳。当前方案会把本模块注入的上下文从非 peer 目标上清掉，同时不删除显式业务头。
- **在 httpx 上可以落地**：`_build_redirect_request` 沿用 `request.extensions` 与上一跳已发送的 headers，而 request hook 在每一跳都会执行。因此本地标记能够跨跳传递，最终钩子可以按"先清理、再重检"执行。

**关于旧业务头的损伤**
- 在 peer 边界，显式提供方的 traceparent 会被覆盖；重定向到非 peer 后也不会恢复。
- 这属于已声明、仅影响配置 peer 的有意取舍（README L157–160、L175–176），不构成对既有非 peer 行为的回归。
- 以下内容在所有路径上都保持不变，向量均有覆盖：
  - X-Request-Id / X-Trace-Id；
  - 非 peer 目标上的显式追踪头；
  - 响应方向上的旧头。

---

## 二、R1 各项 P3

十项 P3 均已按处置表落实。抽查结果：

- `not_configured` 已从 schema 中删除，并有无效 fixture 覆盖。
- 非 peer 不能声称拥有响应 ID：schema 的 `peerConfigured=false` 分支与相应 fixture 共同保证这一点。
- raw 白名单按协议互斥：`gemini-rejects-openai-total` 以及两个合法 fixture 覆盖。
- 方案 §6.3 已明确 v1 不新增模型文本/工具参数的哈希或指纹。
- 以最终 request-target 作为匹配依据：所有 peers 向量都带 `boundary`。
- `.gitattributes` 的要求已写入 README 和八个任务规范。

---

## 三、剩余问题

**P1：无。P2：无。**

### P3（建议，不阻断）

1. **`headers.json` 中 `control-value` 的边界标签不准确。**
   - 该向量的 tracestate 值含 `\n`，标记为 `framework_fields`。
   - 真实框架（h11、Go、llhttp）会在解析阶段拒绝这样的请求，应用层观察不到这个值。
   - 建议与 `control`、`id-control` 一样改标 `parser_unit`。
   - 期望输出不变，只影响 DIAG-02/03/04 对"哪些用例必须走真实 HTTP 边界"的理解。

2. **方案正文存在旧描述，可能误导实现方。**
   - `CROSS_SERVICE_DIAGNOSTICS_PLAN_CN.md` 第 139 行的公共信封字段表与 schema 不一致：
     - 仍列有 `stage`，而 schema 设了 `additionalProperties:false`；
     - 缺少 `spanKind`、`recordKind`、`serverSpanId`、`contextSource`。
   - 第 205 行的 debugCoverage 规则是旧的简化版，没有体现 accessCapture 的优先级。
   - 建议在 §6 开头加一句："字段与判定以 README、schema 和向量为准；§11、§12 取代本节冲突描述"。

3. **重定向向量采用的是 httpx 的继承模型，Go 的行为不同。**
   - Go 的 `http.Client` 在重定向时从初始请求 `ireq.Header` 重建下一跳的头，不继承 RoundTripper 修改过的副本。RoundTripper 本来也不得修改传入的 req。
   - 建议在 `vectors/README.md` 注明：各实现应以"本跳实际交给最终边界的头"作为输入来应用该向量。Go 侧的拥有权标记可能天然为空。
   - 这样可以避免 DIAG-04 为了迎合向量而去修改 `ireq`。

4. **IPv4 与 IPv6 的非规范写法处理不对称。**
   - IPv4 的前导零写法被拒绝；
   - 而 IPv6 的展开写法 `[0:0:0:0:0:0:0:1]` 被接受并规范化（`ipv6` 向量）。
   - README L211 的 "Reject noncanonical numeric hosts" 读起来与此矛盾。
   - 建议写明："该条仅针对 IPv4 的替代数字形式；IPv6 按 RFC 5952 规范化后比较"。

5. **oracle 依赖 Python 版本。**
   - `ipaddress` 从 Python 3.9.5 起才拒绝前导零，`ipv4-leading-zero` 向量依赖这一行为。
   - 建议在 README 或 `requirements.txt` 的注释中写明最低 Python 版本。

6. **`coverage_oracle` 中有一个不可达分支。**
   - 第 400 行的 `elif case["accessCapture"] != "enabled_throughout"` 已被前面的分支完全覆盖，永远不会执行。
   - 保留无害，删除后可读性更好。
   - 如果修改，会引起摘要变化并需要重审，因此建议与其他 P3 合并处理，或者直接不改。

---

## 四、更低侵入方向

当前收敛程度合适，不建议再扩大范围。有两点可以供后续任务参考：

- **不要在拥有权标记中保存"被覆盖前的提供方值"来做恢复。** 这会引入值存储和恢复时机两类新的复杂度。当前"只在 peer 边界覆盖、不恢复"的规则更简单，且只影响配置的内部 peer。
- **DIAG-02 和 DIAG-04 可以用同一张盘点表同时交付两份清单。** 一份是写入/复制点清单，另一份是"无法保持拥有权/清理顺序因而不注入"的路径清单。这样可以减少重复的文档工作。

---

## 五、放行意见

- 在我审查的附件范围内，`990d5ee75fd0878dc1f24753cccf3000701a94ea`（契约摘要 `26904145…aafd57a`）满足"无 P1/P2"的放行条件，可以由协调方记录为批准的冻结来源。
- 如果同时处理上述 P3，任何字节变化都会改变摘要，需要对新的准确 HEAD 再做一次复审。
- 运行时的一致性、拥有权连续性和最终 URL 的观察边界，仍由 DIAG-02 至 DIAG-07 的实际实现来验证。
