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
