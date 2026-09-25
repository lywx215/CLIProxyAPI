# DIAG-06 交付：待审核

任务 ID：`01a0d7a0-b595-7d42-a767-96eba54ae48d`。
项目/worktree：`C:/Users/lywx2/.codex/worktrees/ac87/CLIProxyAPI`。
分支：`codex/diag-06-log-analyzer`。
初始 HEAD：`4996e7ae12b2af38f3bdc490eedf32e1887aeed6`；已先核对 Git 根目录、完整 SHA、tracked clean，随后创建指定分支。
交付 HEAD 为本报告所在本地提交，完整 SHA 在最终交接消息中提供，避免文档自引用。

## 准确依赖与范围

| 依赖 | 批准 SHA |
| --- | --- |
| DIAG-00 契约 | `bb291667f7b6bd7a1dab6f9b7f906b5871d1306c` |
| DIAG-02 gcli2api | `0b3a07e003ead2ba7a9f7827426c09f8ff996813` |
| DIAG-03 Aitoapi-custom | `a2d51383bc91751f23bc0f8927c19ef736593cea` |
| DIAG-05 R2 / 本任务基线 | `4996e7ae12b2af38f3bdc490eedf32e1887aeed6` |

冻结标识 `ai-proxy-diagnostics/1`，制品 `1.0.0-rc.1`。
`SHA256SUMS` 原始字节 SHA-256：
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。
72 个成员加清单共 73 个冻结文件的内容、成员集、Git/index/worktree 字节均未改变。
完整阅读本工作树方案、任务包、契约，并只读核对协调方最新交接/R2 取舍、02/03 交付及补充说明。
历史“未批准”描述按派发提示中的批准 SHA 处理，没有修改冻结材料。

**既有主体流程修改：无。** 没有修改运行时、路由、模型/账号选择、重试、usage、流控制、日志生成或跨项目业务源码。
独立分析包只复用现有 `internal/diagnostics/coverage.go` 的纯计算 `AssessCoverage`。
既有 `go.mod`、`go.sum` 未修改；无新增运行时外部依赖。

## 变更路径与行为

| 路径 | 内容 |
| --- | --- |
| `cmd/diag-analyze/main.go`、`main_test.go` | 独立 CLI；多输入、显式来源信任/已知丢失、trace/有作用域 caller 查询、text/JSON、退出码及真实编译后命令测试。 |
| `internal/diagnosticanalyzer/validate.go`、`record.schema.json`、`.gitattributes` | 冻结 schema 原字节嵌入；只支持该 schema 实际关键字、未知关键字闭合失败；重复键/UTF-8/数值/条件语义验证。 |
| `internal/diagnosticanalyzer/import.go` | 有界混流读取、巨行排空/续读、固定原因隔离、来源行号/大小/摘要/boot、结构等价去重、冲突 payload 全保留。 |
| `internal/diagnosticanalyzer/analyze.go`、`analyze_test.go` | 分离本地拥有者与跨服务双边关系，逐边冲突、循环、覆盖、身份计数、来源作用域和报告；80 个冻结离线算法向量及专项。 |
| `internal/diagnosticanalyzer/testdata/` | 五份准确 SHA 的真实生产者合成请求输出，共 373 条；来源/摘要清单、冻结字节核验及独立 Python schema/语义复核脚本。 |
| `docs/diagnostics/diag-analyze.md` | CLI/API 边界、例子、信任、usage、覆盖、容量、各项硬上限及错误语义。此单文件按仓库已有 `docs/*` 忽略规则显式加入提交。 |
| `coordination/diagnostics/20260924/DIAG-06-validation/`、本报告 | 测试、编译、失败历史、实际 CLI 树和多 producer JSON 摘要。 |

来源信任默认 false；仅 `-trust ALIAS` / API `Input.Trusted` 显式设置。日志的 service/instance、peerConfigured、caller trust、process capabilities 均不能赋予导入信任。
没有向冻结 bundle 增加 trust 字段；工具当前直接接收 bundle 中的各本地记录文件，不实现 bundle 导入。
同事件同内容保留所有导入出处，受信和未受信副本共同出现仍保守不升级；同身份不同内容、跨 trace/span 复用均保留冲突。
查询之前分析全部有界输入；查询外相关冲突节点明确 `contextOnly`、不膨胀查询计数；选中 boot 的所有 process 冲突变体也保留。

调用图要求唯一的完整 call/server 终局和双边已知 service/deployment、可选响应 ID、配置调用方内部 ID 核对。
缺本地 owner 单独显示，不抹去可独立核对的远端证据。歧义、未受信来源、冲突、循环、终局存根、受限导入均不产生相关 verified 边。
歧义本地 owner 采用共享组，不把每个 call 与全部候选父节点重复展开；600 条记录压力用例验证端点线性有界。
循环检测先于 verified 发布；墙钟仅原样保留，不推断父子或跨主机耗时。

计数分别为 observed server/request、call、resource/server/retryScope/attempt。
attemptId 优先，数字 fallback 仅接受显式 `-unique-attempt-scope SERVICE/SCOPE` 操作者担保；当前三个 producer 不需要此 fallback。
gcli `[1,2,1,2]` 不合并独立 ID；compaction 多 call 可同 attempt。
server 声明 callCount 与已观察调用数量不符会显示缺口/矛盾，巨大计数不展开分配。

DEBUG 覆盖保持缺失/unknown/none/partial/full 区别，终局存根存在但不完整。
序号缺口、迟发现的 owner 冲突、process 冲突最终反映为 partial；独立 call 可在 server 封存后出现。
process capability 只说明潜在能力；缺 converted 不证明业务转换没执行。
usage 不重算、不跨 attempt 累加、不把 87 当异常；source/basis/null/observed zero、upstream/delivered 原样分列。
所有 usage 标为 last-observed，不能从 v1 缺失的通用 final 保证推断为最终值。
gcli 文件 ≥16,773,120 字节只有疑似尾部缺口提示，并保留来源大小/boot；不标确定丢失。

## 命令与验证结果

Windows，Go `go1.26.0 windows/amd64`，Python `3.12.10`，`CGO_ENABLED=0`。
最终 Go 验证保留默认 checksum 验证，`GOPROXY=off` 禁止依赖下载。
Python 设置 `PYTHONDONTWRITEBYTECODE=1`，只复用临时目录
`C:/Users/lywx2/AppData/Local/Temp/gcli-diag02-validation-deps` 中冻结 requirements 版本的校验依赖。
没有安装或修改项目依赖，也未向契约目录生成 `__pycache__`。

| 完整命令 | 退出码与证据 |
| --- | --- |
| `gofmt -w .` | 0。仅本任务新增 Go 保留格式结果；既有文件产生换行/stat 标记，先确认 `git diff --quiet`=0，再仅 `git restore --worktree -- '*.go'` 恢复本任务格式化造成的既有工作副本变化。最终既有 Go 无 diff。 |
| `gofmt -w internal/diagnosticanalyzer cmd/diag-analyze` | 0，最终新增 Go 均已格式化。 |
| `go test -timeout 120s ./internal/diagnosticanalyzer ./cmd/diag-analyze` | 最终 0；`related-tests.txt`。含实际 `go build` 后执行 CLI、80 个冻结离线算法向量、所有 record fixture、373 条 producer 记录。 |
| `go test -timeout 10m ./...` | 最终 0；`full-go-test.txt`。上一轮在最后边保护补充前遇到 11 个 Application Control 拦截、整体退出1，原日志 `full-go-test-before-final-edge-guard.txt` 保留；源码修订后按要求执行最终整套，未更名程序/改策略绕过。 |
| `go build -o C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/server.exe ./cmd/server` | 0；`build.txt`。未运行 server。 |
| `go build -o C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/diag-analyze.exe ./cmd/diag-analyze` | 0；最终工具 `final-tool-build.txt`。 |
| `python contracts/diagnostics/v1/validate.py` | 0；`contract.txt`：3 schemas、53 fixtures、9 example lines、242 vectors。producer 头/peer/Aito 映射向量仅算冻结 oracle，不冒称 reader 实现这些 producer 接入。 |
| `python internal/diagnosticanalyzer/testdata/verify_artifacts.py` | 0；`artifacts.txt`：73 冻结 Git/index/worktree 文件、嵌入 schema、373 producer 记录。五份样例另逐字节与各准确依赖 SHA 的 `git show SHA:path` 比对通过。 |
| `python internal/diagnosticanalyzer/testdata/validate_producers.py` | 0；`producer-schema.txt`：373 条记录同时通过原冻结 Python schema 和 semantic oracle。 |
| `C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/diag-analyze.exe -input pair=contracts/diagnostics/v1/examples/bilateral-basic.jsonl -trust pair -format text` | 0；`cli-tree.txt`：2 requests / 1 call / 1 attempt，双边 verified 树及出处。 |
| `C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/diag-analyze.exe -input gcli=internal/diagnosticanalyzer/testdata/gcli-zero.jsonl -input aito=internal/diagnosticanalyzer/testdata/aito-r1.jsonl -input cpa=internal/diagnosticanalyzer/testdata/cpa-r2-read.jsonl -trust gcli -trust aito -trust cpa -format json` | 0；`cli-producer-summary.json`：29 requests / 32 calls / 32 attempts，0 quarantine，未受限。完整 JSON 在上述临时目录 `producer-report.json`，摘要没有代替单元测试的完整 evidence。 |
| `git diff --check`、`git diff --cached --check` | 0（提交前）。 |

早期验证问题如实保留：

- 第一次语义向量适配器误把 input 包装层传给 `semanticIssues`，导致10个预期负例未命中；修正为 `input.record` 后所有80向量通过，未放宽期望或改契约。
- 一次新增包专项进程被 Application Control 拦截：`go-build1757827757/b277/diagnosticanalyzer.test.exe`，整体退出1；CLI包退出0。之后有实质源码/测试修订，最终专项通过。未用更名程序、关闭策略或循环重跑来绕过。
- 首次尝试同时设置 `GOPROXY=off; GOSUMDB=off` 导致 Go 自动工具链无法验证，测试和两个 build 均退出1、尚未编译。改为保留默认 checksum 验证，只禁止 proxy 下载。`toolchain-preflight-*.txt` 保留原错误。
- 误将既有 CPA 专用 `python internal/diagnostics/testdata/validate_records.py internal/diagnosticanalyzer/testdata/gcli-zero.jsonl internal/diagnosticanalyzer/testdata/gcli-incomplete.jsonl internal/diagnosticanalyzer/testdata/aito-r1.jsonl internal/diagnosticanalyzer/testdata/cpa-r2-read.jsonl internal/diagnosticanalyzer/testdata/cpa-r2-limits.jsonl` 用于纯 JSONL 的 gcli 样例，退出1（该脚本强制 `@diag `）。未改已有脚本或样例；新增双 framing 的离线校验驱动后全部373条通过，`producer-cpa-only-check.txt` 保留误用记录。

## 协调预审意见

- 覆盖计算后本地 owner 检查新增冲突：在发布前统一应用最终图冲突。用 schema 合法的父 server/call trace 或 requestId 不一致反例验证两侧 partial。
- 多冲突 server × 多 call 的二次方候选复制：共享歧义组一次表达；query 的同 span 上下文只展开一次，event 变体不反复扫描已有变体。600记录压力反例保留全部证据，端点数量有线性断言。
- process 冲突的环境/部署变体在 trace 查询中可能隐藏：最终 process 证据按本地 service/instance/boot 关联，保留完整资源不同的所有变体与出处；合法 process 记录查询回归通过。
- `gofmt -w .` 的既有文件标记已清理；没有将既有业务文件变化带入提交。

## 覆盖缺口与风险

- `verified` 是操作者控制的双边日志证据，不是密码学证明；schema 也不能证明 ID 形状的值一定脱敏。操作者必须提供可信导出和非敏感来源别名。
- 工具不实现 bundle 文件导入、在线/生产查询、new-api/浏览器内部 span、集中留存或跨部署内容指纹。
- `full` 仅相对于声明采集及已观察导出；完全未记录的崩溃/静默停写无法证明。达到工具上限会显式 limited 并禁止 verified；截断的隔离详情有 omitted 计数。
- 图存储保留节点与候选组，不展开歧义笛卡尔积；已歧义关系不会进一步展开全部潜在循环，其相关边已禁止 verified。文本树深度32后明确平铺剩余节点。
- 支持的 schema 验证器限定于精确冻结制品的关键字集合，不能当通用 JSON Schema 库；新增契约需要复审更新，不能仅替换文件放开未知关键字。
- Windows `CGO_ENABLED=0`，race **未运行**；Linux/POSIX/fork 和三个真实服务各两实例跨服务联调 **未执行**，属于后续 DIAG-07/协调方环境验收，不能以离线样例冒充。
- 第一次全量回归及专项曾遭 Application Control 拦截；最终全量退出0不抹去这些环境历史。

只在本任务新 worktree 修改。未调用生产模型、new-api、真实凭证/DB/Volume；未读取私密 Key；未修改其他项目或冻结契约；未派发子任务、发起 Claude 自审、推送、合并主分支或部署。
本地提交后停止，等待协调窗口按准确 HEAD 复核及实际 Opus 5.5 审核。状态：**待审核**。
