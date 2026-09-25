# DIAG-00 R2 批准记录与窄范围收尾取舍

R2 由协调窗口调用 Claude，结论 approve，无 P1/P2。批准仅针对准确 HEAD `990d5ee75fd0878dc1f24753cccf3000701a94ea`、契约摘要 `26904145b3f1713577f41cd4bbd588ffe48b5f2670b91705537e386f1aafd57a`。
完整只读来源评审已复制为 [review.zh-CN.md](review.zh-CN.md)，仅规范化行尾空白和末尾换行；未复制审查 prompt 或其他项目源码。本开发窗口没有自行调用 Claude。

按协调要求，派发前在原 worktree 做以下小修订。**任何新字节不在上述批准范围内，新 HEAD 仍待协调方复核；不推送、不启动依赖、不实施业务代码。**

| R2 P3 | 收尾取舍 |
| --- | --- |
| 1：control-value 标签 | 改为 parser_unit。含换行的值是独立解析器负例，不声称 HTTP 框架会把它交给应用；expected 不变 |
| 2：§6 旧概述 | 直接替换公共信封字段清单，去掉顶层 stage，补齐 recordKind/spanKind/serverSpanId/contextSource/callerAliasScope/data；直接更新 debugCoverage 完整优先级并链接当前契约，明确 schema/README/向量为一致的机器契约准据，不只在末尾追加说明 |
| 3：Go/httpx 重定向差异 | 说明 redirect oracle 是实际携带诊断头的连续跳模型，不能强迫所有客户端复制方式一致。Go RoundTripper 先克隆，不修改 req/ireq；下一跳实际已无本模块头时标记可为空。真实继承路径仍必须清理，最终不泄漏和逐跳匹配不变 |
| 4：数字 host | 限定非规范数字拼写的拒绝规则针对 IPv4；合法 IPv6 展开/压缩形式解析后按 RFC5952 规范化比较，oracle/expected 不变 |
| 5：Python 版本 | 说明本制品最低支持的验证基线为 Python 3.12，实际已验证 CPython 3.12.10 和锁定依赖。不声称旧版本兼容，较新解释器也须重跑向量 |
| 6：coverage 冗余分支 | 保留该无行为影响分支，按最小改动原则不修改 oracle 实现。既有枚举输入已由前置分支处理；全部 expected 不变 |

未新增测试矩阵、schema 字段或功能。继续复跑原有 242 个向量、摘要和 Git 字节校验，结果及新提交信息见本轮交付报告/最终回复。R2 对旧 HEAD 的批准不能自动传递到本次收尾提交。

## 本次验证与交付

- 状态：待复核；原分支 `codex/diag-00-contract`，原 worktree `C:/Users/lywx2/.codex/worktrees/622a/CLIProxyAPI`。
- 初始基线：`fde3903689b8c9882d1f0233587fe33e2edfdf77`；本轮父提交：`990d5ee75fd0878dc1f24753cccf3000701a94ea`。新完整 HEAD 由最终回复报告，避免在提交内自引用。
- 新契约 SHA-256：`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。
- R2 只读源评审 SHA-256：`c2310c1d1be9d343a0d273cd82ffc6a5af5475a490de8ccee6f5c4022ee34b3e`；提交副本只规范化行尾空白和末尾换行。
- `python contracts/diagnostics/v1/validate.py --write-manifest`、普通 `validate.py`：均退出 0；3 schemas、53 fixtures/schema cases、9 JSONL 行、242 vectors 通过。验证依赖按锁定 requirements 安装到工作区临时目录，运行时设置 PYTHONPATH 和 PYTHONDONTWRITEBYTECODE=1。
- 对比父提交：全部 242 个向量的 id/input/expected 不变，oracle 文件字节不变；§6 信封清单与 schema 属性集合完全一致。
- Git index 与工作区全部 73 个契约文件字节相同；72 个清单条目通过 Python 与独立 PowerShell Get-FileHash 核验。
- `go build -o .diag-00-build.exe ./cmd/server`、`git diff --cached --check` 均退出 0。没有业务源码修改，不运行无关回归；临时依赖和二进制在提交前清理。
- 变更仅涉及完整方案/审查/台账、契约 README、向量说明与单个 boundary 标签、摘要清单和 R2 评审处置；脱敏样例与运行时覆盖缺口保持不变。未新增功能、矩阵、业务代码，未自行调用 Claude、推送、部署或派发任务。
