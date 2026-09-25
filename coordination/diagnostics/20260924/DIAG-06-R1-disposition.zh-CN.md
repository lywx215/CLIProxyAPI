# DIAG-06 R1 修订：待复审

任务 `01a0d7a0-b595-7d42-a767-96eba54ae48d`，仍使用
`C:/Users/lywx2/.codex/worktrees/ac87/CLIProxyAPI` 与分支 `codex/diag-06-log-analyzer`。
退回/本轮父提交：`189b53f71137241f3143a8f4678742cd84b93d98`；本轮完整新 HEAD 在交接消息提供。
最初开发基线：`4996e7ae12b2af38f3bdc490eedf32e1887aeed6`。
开工核对根目录、HEAD 和 clean 状态一致，未使用其他 worktree。

已完整读取协调方只读审查材料：
`G:/code/gemini30/CLIProxyAPI/coordination/diagnostics/20260924/reviews/DIAG-06-R1/review.md`
及同目录 `disposition.md`。实际/标准模型 `claude-opus-5-5`，结论 request_changes，0 P1 / 1 P2。
本任务没有调用审查模型；旧 HEAD 的测试通过不构成新 HEAD 审核通过。

## P2-1：任意输入缺口全局阻断 verified

先新增实际 `Analyze` 反例，在未修复实现上运行：

```powershell
go test -timeout 120s ./internal/diagnosticanalyzer -run '^TestOtherSourceGapBlocksVerification$'
```

退出1，[reproduction.txt](DIAG-06-R1-validation/reproduction.txt)。六例全部复现 `verified=1`：
A 为完整受控双边 pair，B 分别是立即 read_error、先读到无关合法记录后 read_error、空流但
声明 Size=1、合法无关记录但声明大小多1字节、空流 KnownLoss、无关记录 KnownLoss。
未读到相关候选不能证明未读部分没有竞争 receiver。读取错误还复现行号0/上一行。

修复只在独立分析模块中扫描来源状态并添加全局边 finding：

- 任一 `CompleteScan=false`：`source_scan_incomplete`。
- 任一操作者声明 `KnownLoss=true`：`export_known_loss`。
- 仍由原有 finding 为空才发布 verified 的规则阻断；不丢弃已读记录，不改写 source 原因，不把 I/O 错误或已知导出丢失设为 `limited`。
- 原有来源关联 span 的 partial 判断保留。其他来源的 span 覆盖仍描述已读 span/source 证据；全局唯一性不成立则边保持未验证。

同一六例修复后退出0，[fixed-regression.txt](DIAG-06-R1-validation/fixed-regression.txt)。
断言 verified=0、准确全局/来源原因、Limited=false、来源 CompleteScan 状态、已读 evidence 数量、
B 已读节点 partial，以及零字节错误的下一行行号。未放宽断言或改契约。

## 同批小项与取舍

- 零字节失败的 `Provenance.Line` 指向下一行；`Source.Lines` 仍只统计已扫描到字节的行，不虚构一条已读取行。
- CLI 在 `os.Open` 前 `os.Stat` 拒绝非 regular，再保留原有 `f.Stat` 检查。编译后实际 CLI 验证 Windows 目录返回2及固定脱敏原因。未在 POSIX 执行 FIFO 测试，也不声称此预检消除 TOCTOU；未新增平台或超时机制。
- 文档明确 coverage 向量直接测试复用纯函数，记录组装由独立 `Analyze` 专项覆盖。审查文字写27，但冻结 `vectors/coverage.json` 实际数组长度为 **26**；总数为 graph20 + scope12 + counts10 + coverage26 + semantic12 = 80。没有为凑数增改冻结向量。
- 文档明确 CRLF 的两个字节计入4096上限；LF边界记录经CRLF转换后超限会隔离。API 未提供 `Input.Size` 时没有生产者容量线索，不用 ScannedBytes 猜原始大小。
- 文档记录 environment=unassigned 在已知别名作用域下可能保守漏匹配；本轮不扩身份语义。
- 图向量子集 finding + 精确 verified 集合、行缓冲分配、重复 ended 分支、受控样例 Python json.loads、专用 schema 关键字处理均按协调取舍保持不变。
- 超长 legacy / 普通 JSON 混流的可用性疑点留 DIAG-07；本轮没有启发式切换 framing 或吞掉后续 JSONL。

## 样例来源与冻结制品

准确依赖不变：DIAG-00 `bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`；
DIAG-02 `0b3a07e003ead2ba7a9f7827426c09f8ff996813`；
DIAG-03 `a2d51383bc91751f23bc0f8927c19ef736593cea`；
DIAG-05 `4996e7ae12b2af38f3bdc490eedf32e1887aeed6`。
冻结 `ai-proxy-diagnostics/1` / `1.0.0-rc.1`，manifest SHA-256
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`。

本轮 `verify_artifacts.py` 再核验73个冻结Git/index/worktree文件和五份样例的本地摘要；
`validate_producers.py` 再以冻结 schema/semantic oracle 验证373条记录。这两个脚本**不执行跨仓 git show**。
协调方已对被审 HEAD 的五份样例与各批准 SHA 作逐字节比较，5/5通过；其非敏感
[producer-source-evidence.json](DIAG-06-R1-validation/producer-source-evidence.json)
从协调方审查目录复制，仅将CRLF归一为LF，包含检查时间、被审 SHA、各来源 SHA/路径/摘要与 gitByteEqual。
该证据属于协调方独立检查，本任务不冒称由本地摘要脚本完成。五份样例本轮没有修改。

## 验证台账

Windows Go `go1.26.0 windows/amd64`，`CGO_ENABLED=0`；Go验证设置 `GOPROXY=off`。
Python校验设置 `PYTHONDONTWRITEBYTECODE=1`、
`PYTHONPATH=C:/Users/lywx2/AppData/Local/Temp/gcli-diag02-validation-deps`，不安装新依赖、不产生契约缓存。
日志仅统一为UTF-8/LF并去除行尾空白，原结果与失败保留。

| 完整命令 | 退出码 / 证据 |
| --- | --- |
| `gofmt -w .` | 0；核对无额外Go内容diff后仅恢复无关文件的格式/stat变化，保留本轮五个Go变更。 |
| `go test -timeout 120s ./internal/diagnosticanalyzer -run '^TestOtherSourceGapBlocksVerification$'` | 修复前1，修复后0，分别见 reproduction / fixed-regression。 |
| `go test -timeout 120s ./internal/diagnosticanalyzer ./cmd/diag-analyze` | 0；[related-tests.txt](DIAG-06-R1-validation/related-tests.txt)，含真实CLI目录拒绝与全部既有专项。 |
| `go test -timeout 10m ./...` | 0；[full-go-test.txt](DIAG-06-R1-validation/full-go-test.txt)，本轮没有Application Control失败。 |
| `go build -o C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/server.exe ./cmd/server` | 0；[build.txt](DIAG-06-R1-validation/build.txt)，未启动server。 |
| `go build -o C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/diag-analyze.exe ./cmd/diag-analyze` | 0；同一build日志。 |
| `python contracts/diagnostics/v1/validate.py` | 0；3 schemas / 53 fixtures / 9 example lines / 242 vectors，contract.txt。 |
| `python internal/diagnosticanalyzer/testdata/verify_artifacts.py` | 0；73冻结文件、嵌入schema、373记录摘要，artifacts.txt。 |
| `python internal/diagnosticanalyzer/testdata/validate_producers.py` | 0；373记录，producer-schema.txt。 |
| `C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/diag-analyze.exe -input pair=contracts/diagnostics/v1/examples/bilateral-basic.jsonl -input other=C:/Users/lywx2/AppData/Local/Temp/diag06-validation-20260925-ac87/other.jsonl -trust pair -trust other -known-loss other -format json` | 0；other为空临时文件，verified=0、limited=false、全局 export_known_loss；[cli-known-loss.json](DIAG-06-R1-validation/cli-known-loss.json)。退出0仅表示扫描完成。 |
| `git diff --check`、`git diff --cached --check` | 0，提交前核验。 |

修改范围：`cmd/diag-analyze/main.go`、`main_test.go`；`internal/diagnosticanalyzer/analyze.go`、
`analyze_test.go`、`import.go`；使用/交付说明及本轮验证材料。既有主体流程、冻结制品、producer样例、
依赖锁定文件均无修改。未修改跨仓业务源码。

race因Windows CGO=0未运行；POSIX FIFO/平台竞争与三服务双实例真实联调未执行，保持DIAG-07缺口。
没有改安全策略、换名绕过拦截、生产调用、凭证/DB操作、派发新任务、自行Claude审核、推送、合并或部署。
本地新提交后停止，等待协调窗口按新HEAD与实际Opus5.5复审。状态：**待审核**。
