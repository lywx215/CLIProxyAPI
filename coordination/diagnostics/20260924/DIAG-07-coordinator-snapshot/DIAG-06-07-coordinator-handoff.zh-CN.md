# DIAG-06/07 协调方补充交接

本说明仅整理后续派发要求，不启动开发。DIAG-06 必须等 DIAG-02/03/05 的最终准确 HEAD 均审核通过；DIAG-07 等 DIAG-02/03/06。每项仍需新项目任务和隔离 worktree。

## 冻结身份

契约来源：bb291667f7b6bd7a1dab6f9b7f906b5871d1306c；ai-proxy-diagnostics/1，1.0.0-rc.1；SHA256SUMS 原始字节 SHA-256：ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4。使用 DIAG-00 已审核提交内的任务规范，派发时明确覆盖其中历史“候选未批准”的状态描述，不改变冻结制品。

## 已核实的生产者差异

- gcli2api 的 OpenAI completion_tokens 只含 candidates，reasoning 在 details 独立字段；Aitoapi 和 CLIProxyAPI 当前转换路径将 candidate+reasoning 写入输出总量。不得全局套一个公式或再加一次 reasoning。
- 相同87不是异常规则。只在合成请求证明各阶段数值来源，不能据此证明历史计费平台算法。
- gcli2api 的 attemptNo 可在外层续写中重新从1开始；按资源/server/retryScope/attemptId识别，不按 attemptNo 合并有独立 ID 的尝试。
- gcli2api server结束时会结算已知 attempt 观察；独立 call 的真实 EOF 可能后来才记录。不得用后到call重写原attempt结果，也不能跨span比较logSeq。
- Aitoapi/CPA新 timing 的 firstEffectiveOutputMs 使用本地 server-start 单调时间偏移，attempt totalMs 是本次尝试持续时间。旧版本或来源不明的 timing 不能跨服务直接相减。
- Aitoapi保留legacy seq、ACK、generation/deliveryOutcome身份，公共logSeq独立；HttpBoundary虽有适配器测试，但没有生产caller，不得虚报浏览器HTTP/images/version/VNC出站覆盖。
- 来源作用域未知的 callerRequestId 仅扩大候选，不能合并可信调用树；不凭同一traceId生成已验证边。

## gcli2api容量与缺口

最终批准DIAG-02后，读取其 docs/diagnostics/DIAG-06-07-handoff.md 的准确提交版本。本次候选说明：单boot文件硬上限16MiB，到上限或写失败可永久停写，无尾部停写事件，目录总量无界。接近16MiB减4096只能提示“疑似尾部截断/证据缺口”，不能证明丢失；小文件也可能缺尾。保留输入大小/来源/行数/boot边界，结合终局、序号、truncation和已知损失判断。不得发明冻结事件，不能把静默当作无请求或完整覆盖。未来生产启用前必须另行解决容量/留存/目录配额与部署httpx版本差异；本批不部署。

## 联调环境边界

每种服务至少两个真实被测隔离实例，提供方/new-api/浏览器才是模拟。真实服务流程不能被简单协议stub替代；CPA可以在自己的仓库开发隔离驱动，其他两仓只读取批准SHA入口，缺陷交回原任务。

Aitoapi QA使用Node24；跨仓库Python校验设置PYTHONDONTWRITEBYTECODE=1以避免__pycache__干扰严格契约目录核验。gcli使用其隔离runner/临时数据目录，不能影响真实log.txt、凭证或SQLite。所有地址精确loopback、隔离端口/工作目录/环境变量，禁用外网模型与启动自动抓取。

当前Windows无可用WSL/Docker daemon/CGO race工具链，POSIX fork测试跳过是未执行，不写通过。若DIAG-07无法提供Linux/POSIX实际证据，交付表必须明确未完成项并提交协调方评估，不用Windows多进程冒充fork。

记录普通全量验证中Windows Application Control的历史失败；不得关闭策略或改名程序绕过。临时验证目录删除被自动审批拒绝的，保留并排除Git，不重复绕过。

## 审核

每个最终HEAD先协调方diff/测试，再显式claude-opus-5-5；核对返回模型。API429无推理/空modelUsage不构成审核。修订改变HEAD后旧审查不复用。依赖未通过不得启动后续任务。只推送已审核任务分支，不合并main/master、不部署。

CLIProxyAPI取消时处理器可能先于executor清理结束：DIAG-05修订方向使用pending exchange计数，把server debugCapture标为interrupted，保留缺失attempt/conversion事实，不捏造EOF或追加server封存后记录。DIAG-06/07需依冻结规则将这类请求判为partial，并继续显示已观察到的基础call/server；不能以有server终局就补齐语义结果。最终派发以批准HEAD中实际实现及样例为准。

DIAG-02 R3批准补充：error attempt没有converted事件可能是诊断门控，不证明业务converter未执行；local/read incomplete缺HTTP EOF不等于上游失败，需并列实际converted结果。DIAG-07需核验harness shutdown_asyncgens晚到call终局在close()/进程退出前后是否落盘，硬杀缺call终局应显示terminalMissing。不要为此改冻结生产者HEAD，真实缺陷交回原任务。


# DIAG-05 R2 协调方取舍

准确 HEAD `4996e7ae12b2af38f3bdc490eedf32e1887aeed6`，Claude actual/canonical `claude-opus-5-5` approve，0 P1/0 P2。本窗口已核对实际修订、复现用例、完整 Go 回归/server 编译退出0及294记录/242向量/73冻结Git文件。批准推送任务分支，不授权合并主分支。

P3取舍：
- null finishReason 降为unknown是保守诊断，暂无真实触发证据；保持范围，不为推测修改主体流程。联调若观察到真实触发再退回。
- README聚合撤回措辞过强，以代码为准：upstream/delivered各自独立观测和撤回。上游超限不必导致转换后的实际交付统计也未知。此处澄清并传递06/07，不改已审HEAD制造审计循环。
- 位置差异最多16条other是有界噪声，前后缀裁剪为可选优化，非验收阻断。
- 未知finishReason维持unknown符合契约；不在本批扩提供方专属语义。
- R1样例及旧conductor_executor是历史证据；当前retryScope以R2 conductor_gemini_family为准。06/07不得冒充当前版本。
- Gemini已有读错误后DONE完成帧行为是既有业务行为；本批不改，诊断upstream error与converted未知分开展示，不能以完成帧覆盖读错误。

缺口：Claude未自己执行测试，测试由协调窗口独立运行；race/平台环境限制保留。Home与bootstrap重试读错误覆盖未执行，移交07明确范围。协调方补读未修改的sdk/api/handlers/claude/code_handlers.go，forwardClaudeStream只原样写chunk，经通用ForwardStream消费，没有message_stop早退分支；此为源码证据，不冒称已执行Claude读错误矩阵。

DIAG-06 R1非阻断疑点移交07：实际混合导出中超4096字节legacy行或裸JSON普通日志可能保守引发source partial；请验证实际日志模式，不以仅@diag夹具冒称混流可用，不启发式忽略合法JSONL。确有实用性缺陷交回06任务。


# DIAG-06 R2 协调方批准与取舍

准确HEAD9422a853a222aef0dbf67815888c53ef6f1ede77。实际/标准模型claude-opus-5-5 approve，0P1/0P2。父窗口实际diff、6例red→green、完整Go/server/tool及制品/实际CLI检查均通过，批准推送任务分支。未授权主分支合并或部署。

P3取舍：
1. 全局缺失使local树平铺是保守表达，未知输入也可能有重复owner；保留flat与边finding及完整节点证据，不为了更漂亮的树放宽关系。
2. Windows目录测试仅证明目录拒绝与脱敏错误，并非预Stat的red→green；新逻辑源码可证，POSIX FIFO未实测，移交07/部署前环境，不宣称已消除TOCTOU。
3. 新工具尚未正式发布，错误码改为not_readable_regular_file仍为exit2；使用说明涵盖此含义，报告下游脚本按当前准确版本，历史不存在正式兼容承诺。
4. 隔离行只影响自身来源的覆盖及相关边，不全局阻断其他完整受控来源；因为只有schema-valid记录可作为候选，无法把坏行中的不可信片段当身份。报告仍有源quarantine总数与退出1；操作者有真实导出丢失证据应显式KnownLoss，从而全局不verified。此边界在本取舍/07交接明确，不改变冻结契约或已审HEAD。
5. 节点次序及混合失败补测为非阻断建议，不为低风险重复用例扩大本轮。现有6例针对已证错误。
6. 顶层没有额外globalIncomplete布尔，调用方可查sources/limited/edge findings；无边时自然无verified。保持报告结构，不加重复状态。

混合文本中超长legacy/普通JSON保守导致partial仍交07实测，不能把未证实疑点写为失败。安全策略失败历史保留，最终实际parent完整回归0。Claude只做源代码审查，没有自己运行测试；race/POSIX和六实例属于未执行后续项。
