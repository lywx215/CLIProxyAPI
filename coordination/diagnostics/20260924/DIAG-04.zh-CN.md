# DIAG-04：CLIProxyAPI 通用追踪基础

项目：CLIProxyAPI
分支：`codex/diag-04-cpa-tracing`
依赖：DIAG-00
基线规则：DIAG-00 审核通过的精确 HEAD。
已核对远程主分支 SHA：`fde3903689b8c9882d1f0233587fe33e2edfdf77`（仅无依赖主分支任务直接使用；依赖任务须等待协调窗口填写批准 HEAD）。

## 共同约束

你是本批任务的一个全新 Codex 开发窗口。本窗口为协调方新建，不得复用旧窗口、派发其他开发任务或在其他仓库修改代码。
仅在创建时分配的新隔离 worktree 中开发；先确认 git 根目录、当前 HEAD、工作区干净状态和提示中给定基线。基线与提示不符时先报告协调窗口，不覆盖已有改动。创建给定 codex/diag-* 分支。读取你所在分支实际适用的 AGENTS.md/项目约束。
本批范围是通用追踪＋当前 Gemini→Antigravity 和 Aitoapi 既有重点语义诊断。new-api 不修改；生产模型调用、真实凭证/数据库/Volume、部署、合并或推送 main/master 均禁止。gcli2api 不更新 panel-version.txt，不恢复已停止维护的 GeminiCLI 或管理项目。审批钩子提示面板更新时选择 n。
优先独立模块、中间件、共享出站/日志边界和少量只读观察点；不重写路由、模型/账号选择、重试策略、协议转换或流式发送。共享契约修改必须先报告协调方，不自行分叉版本。
完成实现、专项与项目回归后本地提交，等待本批协调窗口审核。不要自行调用 Claude 代替协调审核，不自行推送或宣称审核通过。协调窗口在审核准确 HEAD 后才放行推送。
最终报告必须包含：任务 ID；项目/worktree/分支；基线与 HEAD 完整 SHA；契约来源 SHA/摘要（如适用）；变更文件和主体流程接入点及原因；每个测试命令/退出结果；脱敏样例位置；协议覆盖/缺口；已知风险；未执行动作。状态写“待审核”。
退回修订继续在本任务新建窗口和原 worktree，完成后新提交并报告准确 HEAD，旧审核不自动有效。

## 本任务职责

在独立模块、Gin 入口、基础访问日志与最终 HTTP client 装饰边界接入追踪。资源身份由进程决定；外部 ID 不替代内部键；DIAG_PEERS 精确匹配；保留旧请求/响应头含义。最终 client 配置完成后才装饰，不提前包装 context 或缓存 transport。保证 Devin 类型断言、代理/连接复用、usage transport 顺序和 MarkUpstreamAttempt 不重复计数。WS/自定义路径列出准确覆盖矩阵，不把未覆盖路径声称为全覆盖。不要开发各提供方专用详细诊断。

## 验证与交付

契约向量、所有受影响 client 返回分支、代理/Devin/默认 transport、响应头/重定向、并发多实例、取消/Close/EOF 一次结算、DEBUG/基础日志开关；相关 Go 测试、go test ./...、server 编译。按项目约束 gofmt。


## v1 契约消费与审核门槛（DIAG-00 候选补充）

契约候选：`contracts/diagnostics/v1`，`ai-proxy-diagnostics/1`，制品 `1.0.0-rc.1`。当前未获批准；除 DIAG-01 的独立业务修复外，依赖任务不可自行使用草稿开工。
协调窗口批准后提供准确来源 Git SHA 与 SHA256SUMS 原始字节 SHA-256。消费仓库按字节纳入测试资源并记录来源，同时加入 `contracts/diagnostics/v1/** text eol=lf`（复制目录不同时调整路径），核验工作区与 Git 暂存字节；不运行时跨仓库导入。不改变已有 ID/seq/管理统计含义，不私自扩展公共 schema 或分叉版本。

共同验收：合法/非法/重复头、未来版本、配置 origin/路径边界、调用方来源作用域、双边证据/冲突、缺父节点、实例/worker/重启、缺失/零值、DEBUG/基础日志独立门控、终局序号与完整性。runtime 测试须针对真实处理边界；DIAG-00 oracle 通过不代表运行时实现通过。
报告每个命令及退出结果、完整 baseline/HEAD、契约来源/摘要、脱敏样例和覆盖缺口。协调窗口代码检查后再调用 Claude，准确 HEAD 通过才放行推送；任何修订重新提交/审核。无 P1/P2 才放行。保持最多三个开发任务并行，任务自身不得派发其他开发任务。

应用 headers/outbound/peers/source-scope/coverage 向量于真实入口及最终 client 边界，所有分支给出逐项覆盖。call 终局观察 EOF/Close/错误/取消并且只结算一次，不能以 RoundTrip 返回头为成功终局。


## R1 运行时边界要求

读取框架解析后保留多值的头集合，不绕过框架读 socket；自定义 ID 不额外 trim，512 字节只测可观察值。Node 对 traceparent/tracestate 均用 rawHeaders/headersDistinct。分别运行 http-ingress 真实 HTTP 用例和标为 parser_unit 的独立解析器负例，不把线上的 OWS 当可恢复输入。

出站接入必须交付现有追踪头写入/复制点清单：文件/函数、实际调用参数来源（自动入站复制或显式业务构造）、目标、最终顺序及重定向行为。不能仅因 extra_headers API 有白名单就断言路由自动泄漏或删除其显式能力；没有自动复制的路径不改。所有已证实的自动复制源头过滤标准追踪头/X-Diag-*；最终 peer 边界替换后设置自身本地拥有权，重定向新副本先清本模块头、再新构造业务头、最后按实际发送 URL/request-target 重检。非 peer 必须清掉本模块拥有的追踪头，保留显式业务头，不按值猜来源。Aitoapi 仅在实际有对应出站路径时适用，不为了本规范新增浏览器协议或出站能力。

复现 peer→第三方、peer→同源未允许路径、peer→peer，以及清理后显式新业务头，验证不会泄漏/重复，不改变既有重定向策略。仅配置 peer 的响应 X-Diag IDs 可采集。无可靠拥有权/最终边界的路径不注入，明确覆盖缺口。

## 派发信息

仍等待依赖审核，未创建新任务。projectId、真实 threadId、worktree 和依赖准确 SHA 由协调窗口实际派发时记录；不得把旧主分支 SHA 或本契约候选当批准基线。
