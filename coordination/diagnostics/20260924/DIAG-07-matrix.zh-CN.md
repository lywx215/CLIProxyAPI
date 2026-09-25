# DIAG-07 逐场景矩阵

**待代码审核；整体验收被Windows策略阻塞。**

R2新增[21项范围/双边回归](DIAG-07-R2-validation/replay-03/checks.json)与[345项历史下游语义重放](DIAG-07-R2-validation/downstream-replay/semantic-checks.json)。属于既有导出重放或明确合成日志，没有将任何unexecuted项目升级为live；完整模式的两服务/四目标断言仍待合规六实例执行。

`live`表示实际服务进程/网络执行的明确子集，本次均不能理解为六实例验收；组件和人工日志不提升为live。

| 验收条目 | 证据等级 | 结论/缺口 | 证据 |
|---|---|---|---|
| 六实例同时存活与隔离 | unexecuted | run-02：CPA启动被策略阻止。run-06只2gcli+2Aito；smoke只2CPA+2Aito | [文件](DIAG-07-run-02/failure.json) |
| 模拟new-api→CPA→gcli，流/非流/内部流转非流 | unexecuted | driver已拼装真实边界，但未取得此链路执行证据 | [文件](DIAG-07-validation/blocked-binary.json) |
| 模拟new-api→直连gcli，流/非流/内部流转非流 | live | 独立子集；gcli1 collected、gcli2 native nonstream；Gemini/OpenAI均执行 | [文件](DIAG-07-run-06/semantic-checks.json) |
| 模拟new-api→直连Aito，流/非流 | live | 独立子集；原Express/handler/registry/generation/converter/真实WS | [文件](DIAG-07-run-06/semantic-checks.json) |
| 模拟new-api→CPA→Aito，流/非流 | live | 早期4进程smoke，4条verified；不是六实例或最终参数构建 | [文件](DIAG-07-offline-02/controlled-pair.json) |
| 无ID/request-id/标准traceparent/非法/重复头/复用ID | live | gcli/Aito逐响应ID断言；CPA仅smoke无ID，CPA完整头矩阵未执行 | [文件](DIAG-07-run-06/semantic-checks.json) |
| 相同instance多worker及重启 | live | gcli与Aito独立进程/boot/目录；CPA重启未执行，不能冒充POSIX fork | [文件](DIAG-07-run-06/semantic-checks.json) |
| POSIX fork/容器worker/CGO race | unexecuted | 环境缺口；后续合规Linux/CGO环境执行 | [文件](DIAG-07-coordinator-snapshot/DIAG-06-07-coordinator-handoff.zh-CN.md) |
| 跨gcli实例外层CPA重试+嵌套429/503 | unexecuted | CPA被拒绝，不能用直接gcli重试代替 | [文件](DIAG-07-run-02/failure.json) |
| gcli内层重试与外层续写attemptNo重置 | live | HTTP发送4次、attemptId4个、No=[1,2,1,2]；非CPA跨实例 | [文件](DIAG-07-run-06/semantic-checks.json) |
| Aito重试/浏览器重连/网络错误 | live | 实际dispatch和callback，既有queue/ACK逻辑不改 | [文件](DIAG-07-run-06/semantic-checks.json) |
| gcli取消/真实读断/部分内容后错误帧/缺终止 | live | 客户端IncompleteRead保留partial body，诊断和实际帧分别保存 | [文件](DIAG-07-run-06/requests.json) |
| 内容拦截/空输出/工具/媒体/思考/缺失及零usage | live | gcli工具媒体、Aito工具思考；Aito媒体未live；工具碎片交付计数null不强写success | [文件](DIAG-07-run-06/semantic-checks.json) |
| 正常close/flush和硬杀 | live | 正常回执exit0；kill前已观察dispatch计数，但未证明kill时仍活动；known-loss不造终局 | [文件](DIAG-07-run-06/analysis-known-hard-kill-command.json) |
| gcli server后迟到call落盘 | live | 最终所有已观察server的callCount均匹配，13条call晚于owner server | [文件](DIAG-07-run-06/analysis.json) |
| 特定shutdown_asyncgens迟到时序 | unexecuted | 未隔离证明call发生于close writer之后/asyncgens期间；需要原02入口可观察生命周期专项 | [文件](DIAG-07-run-06/close.json) |
| 400清理/连续空/空白/末尾model/工具/全空错误 | live | no-prefill模型3.7；不在限定范围的2.5不承诺删末尾model；假provider保留空contents400 | [文件](DIAG-07-run-06/semantic-checks.json) |
| 87来源/重复累计/candidate+reasoning/缺失与零 | live | gcli首帧usage交付87、仅尾usage交付null；Aito87+13=100；不是历史计费证明 | [文件](DIAG-07-run-06/semantic-checks.json) |
| CPA非流tokens/rate主导、TTFT主导、collected全程 | unexecuted | 100t/s+10ms及1000t/s+100ms配置可审，但受阻binary未执行；响应完成时间+throttle事件仍待补 | [文件](DIAG-07-validation/blocked-binary.json) |
| 限速来源/实际等待/抵扣/取消组件 | component | 本轮既有确定时间和真实handler组件重跑0；不代替live限速 | [文件](DIAG-07-validation/throttle-components.txt) |
| DEBUG关闭/动态关闭/基础日志关闭 | component | gcli/Aito关闭另有live证据；此行标组件门控；三服务全关及动态链路未live | [文件](DIAG-07-validation/component-tests.txt) |
| 包装前后body/status/顺序/背压/代理/取消/既有统计 | component | 现有真实executor等价及确定性背压/取消组件0；跨服务全面live等价未执行 | [文件](DIAG-07-validation/component-tests.txt) |
| Aito DEBUG前后响应体/状态等价 | live | 排除协议生成id/created后JSON相同；没有据此声称全部帧/背压/统计等价 | [文件](DIAG-07-run-06/requests.json) |
| 观察上限unknown vs实际parse_error | component | DIAG05真实语义/读错/限制测试，本轮重跑；不是live超大输入 | [文件](DIAG-07-validation/component-tests.txt) |
| Gemini读错后DONE/response.completed差异 | component | 保留既有转换行为及诊断error；未修业务；Home/bootstrap仍未覆盖 | [文件](DIAG-07-validation/component-tests.txt) |
| 重复导入/伪造父/偏时钟/坏行/未知schema/来源不可信 | synthetic-log | 基于真实日志显式变换；0/1退出按场景断言；不伪装live输入 | [文件](DIAG-07-offline-02/checks.json) |
| 匿名复用callerRequestId查询 | live | 实际导出多个候选；callerAliasunknown，不伪造principal | [文件](DIAG-07-offline-02/anonymous-alias.json) |
| 实际原始混流stdout | live | CPA12accepted+22legacy+1控制JSON quarantine；gcli诊断在sidecar，stdout仅legacy/控制行 | [文件](DIAG-07-raw-analysis/summary.json) |
| 长legacy/普通JSON/近16MiB容量线索 | synthetic-log | 人工输入。近阈值只suspected；不证明实际停写或小文件完整 | [文件](DIAG-07-offline-02/checks.json) |
| 实际producer达上限停写/轮转/总配额 | unexecuted | 已批准限制：16MiB/boot硬停、无轮转/目录总额；生产启用前另验收 | [文件](DIAG-07-coordinator-snapshot/DIAG-06-07-coordinator-handoff.zh-CN.md) |
| 当前+历史候选导出混合 | synthetic-log | 早期R1候选阶段导出文件被批准基线作为历史资料保留；不是批准旧进程 | [文件](DIAG-07-offline-02/historical-log.json) |
| 实际旧CPA DIAG04滚动 | unexecuted | 协调允许50d335a18a2495bd2fbbb30ac47d09476a8a14b6只读临时构建；本轮未执行，不能手工标签冒充 | [文件](DIAG-07-coordinator-snapshot/DELIVERY_STATUS_CN.md) |
| 浏览器内部HTTP/VNC/images | unexecuted | Aito无生产HttpBoundary caller；不在本轮覆盖声称内 | [文件](DIAG-07-coordinator-snapshot/DIAG-06-07-coordinator-handoff.zh-CN.md) |

## 补充Aito独立live

[aito-controls/checks.json](DIAG-07-aito-controls/checks.json)：伪流87+13实际交付100；浏览器dispatch握手后关闭DEBUG，真实server coverage=interrupted。两项退出0，仍不是六实例。

## run-06逐请求/行为索引

这些73行覆盖HTTP/行为检查；真正语义断言共340项另见semantic-checks.json。HTTP成功不等于语义成功或完整采集。

| 场景ID | 等级 | HTTP/行为 | 语义断言数 | 真实记录出处 |
|---|---|---|---|---|
| topology-gcli1-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 7 |
| native-gcli1-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 12 |
| topology-gcli1-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 16 |
| native-gcli1-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 22 |
| topology-gcli2-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli2.jsonl) line 7 |
| native-gcli2-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli2.jsonl) line 12 |
| topology-gcli2-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli2.jsonl) line 16 |
| native-gcli2-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli2.jsonl) line 22 |
| topology-aito1-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 5 |
| native-aito1-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 9 |
| topology-aito1-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 13 |
| native-aito1-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 17 |
| topology-aito2-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito2.jsonl) line 5 |
| native-aito2-nonstream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito2.jsonl) line 9 |
| topology-aito2-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito2.jsonl) line 13 |
| native-aito2-stream | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito2.jsonl) line 17 |
| headers-gcli1-request-only | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 27 |
| headers-gcli1-standard | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 32 |
| headers-gcli1-invalid | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 37 |
| headers-gcli1-duplicate | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 42 |
| headers-gcli1-reuse-a | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 47 |
| headers-gcli1-reuse-b | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 52 |
| headers-gcli1-duplicate-id | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli1.jsonl) line 57 |
| headers-aito1-request-only | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 21 |
| headers-aito1-standard | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 25 |
| headers-aito1-invalid | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 29 |
| headers-aito1-duplicate | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 33 |
| headers-aito1-reuse-a | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 37 |
| headers-aito1-reuse-b | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 41 |
| headers-aito1-duplicate-id | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 45 |
| semantics-gcli1-thought13 | live（独立子集） | 200 / 通过 | 9 | [记录](DIAG-07-run-06/gcli1.jsonl) line 61 |
| semantics-gcli1-tail13 | live（独立子集） | 200 / 通过 | 9 | [记录](DIAG-07-run-06/gcli1.jsonl) line 66 |
| semantics-gcli1-zero | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/gcli1.jsonl) line 71 |
| semantics-gcli1-missing | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/gcli1.jsonl) line 76 |
| semantics-gcli1-tool | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/gcli1.jsonl) line 81 |
| semantics-gcli1-media | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/gcli1.jsonl) line 86 |
| semantics-gcli1-empty | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/gcli1.jsonl) line 91 |
| semantics-gcli1-blocked | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/gcli1.jsonl) line 96 |
| semantics-gcli1-incomplete | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/gcli1.jsonl) line 102 |
| semantics-gcli1-errorframe | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/gcli1.jsonl) line 106 |
| semantics-gcli1-readerror | live（独立子集） | 200 / 通过 | 8 | [记录](DIAG-07-run-06/gcli1.jsonl) line 112 |
| semantics-gcli1-retry | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/gcli1.jsonl) line 120 |
| semantics-gcli1-nested | live（独立子集） | 429 / 通过 | 8 | [记录](DIAG-07-run-06/gcli1.jsonl) line 129 |
| semantics-aito1-reasoning | live（独立子集） | 200 / 通过 | 9 | [记录](DIAG-07-run-06/aito1.jsonl) line 49 |
| semantics-aito1-zero | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/aito1.jsonl) line 53 |
| semantics-aito1-missing | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/aito1.jsonl) line 57 |
| semantics-aito1-tool | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/aito1.jsonl) line 61 |
| semantics-aito1-empty | live（独立子集） | 502 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 66 |
| semantics-aito1-blocked | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 70 |
| semantics-aito1-truncated | live（独立子集） | 200 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 74 |
| semantics-aito1-retry | live（独立子集） | 200 / 通过 | 7 | [记录](DIAG-07-run-06/aito1.jsonl) line 80 |
| semantics-aito1-http429 | live（独立子集） | 429 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 85 |
| semantics-aito1-http503 | live（独立子集） | 503 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 90 |
| semantics-aito1-network | live（独立子集） | 504 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 93 |
| semantics-aito1-thought | live（独立子集） | 502 / 通过 | 6 | [记录](DIAG-07-run-06/aito1.jsonl) line 98 |
| semantics-gcli1-anti_nested | live（独立子集） | 200 / 通过 | 8 | [记录](DIAG-07-run-06/gcli1.jsonl) line 139 |
| cleaning-trailing-model | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 145 |
| cleaning-multiple-empty | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 150 |
| cleaning-whitespace | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 155 |
| cleaning-tool | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 160 |
| cleaning-all-empty | live（独立子集） | 400 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 164 |
| debug-on-aito1 | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 102 |
| debug-off-aito1 | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/aito1.jsonl) line 105 |
| equivalence-aito1 | live（独立子集） | 行为观察 / 通过 | 0 | [请求/控制回执](DIAG-07-run-06/requests.json) |
| cancel-gcli1 | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli1.jsonl) line 169 |
| cancel-aito1 | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 110 |
| debug-off-gcli | live（独立子集） | 200 / 通过 | 4 | [记录](DIAG-07-run-06/gcli3.jsonl) line 4 |
| aito-browser-reconnect | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito1.jsonl) line 114 |
| aito-restart | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito3.jsonl) line 5 |
| aito-multiworker | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/aito4.jsonl) line 5 |
| gcli-multiworker | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli4.jsonl) line 7 |
| gcli-restart | live（独立子集） | 200 / 通过 | 3 | [记录](DIAG-07-run-06/gcli5.jsonl) line 7 |
| hard-kill-active-dispatch | live（独立子集） | 行为观察 / 通过 | 0 | [请求/控制回执](DIAG-07-run-06/requests.json) |
