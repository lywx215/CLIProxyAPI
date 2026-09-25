# DIAG-07 协调方验收清单

此文件为调度/审核准备，不启动依赖未审完的开发任务，不包含业务实现。只有DIAG-06最终HEAD经协调方和Opus5.5批准后，新建任务及worktree。

## 固定输入

- gcli2api：0b3a07e003ead2ba7a9f7827426c09f8ff996813，<USERPROFILE>/.codex/worktrees/14f9/gcli2api，scripts/diagnostic_harness.py。
- Aitoapi：a2d51383bc91751f23bc0f8927c19ef736593cea，<USERPROFILE>/.codex/worktrees/ca98/Aitoapi-custom，scripts/diagnostics/isolatedServer.js。
- CPA诊断实现：4996e7ae12b2af38f3bdc490eedf32e1887aeed6，实际07基线为后续批准06提交。
- 契约：bb291667f7b6bd7a1dab6f9b7f906b5871d1306c，SHA256SUMS摘要ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4。

## 启动与隔离证据

所有进程绑定精确loopback，独立临时cwd/日志/数据/合成凭证。至少2 CPA+2 gcli+2 Aito同时存活，报告pid、端口、instanceId、bootId、开始/关闭结果，不以6个协议stub代替真实服务。假new-api、上游和浏览器允许模拟。所有外网出站禁用，不能读真实.env/auth/db，构建后从临时cwd启动；Windows隐藏子进程窗口。不更改安全策略。

gcli harness --root须为空，但自身不chdir，调用者必须设临时cwd。其CLI接受18个场景，上游可由CPA项目的独立合成驱动替代以按请求确定性注入。Aito harness构造器前临时chdir，导出start/close/reconnect；需要脚本控制时wrapper应在CPA项目，引用批准入口且不得复制/改写其业务代码。Windows终止进程不等于执行Node的SIGTERM handler，正常关闭须有可验证的close路径，再做刻意硬杀缺口场景。

## 矩阵与证据等级

每个场景标明 live（真实六实例链路执行）/component（批准任务局部测试复用）/synthetic-log（故障日志注入）/unexecuted。不得将混合版本的手工版本标签、缺损日志、模拟数据说成运行真实旧版本。

| 验收 | 必须交付的证据 |
| --- | --- |
| 四条目标链路 | new-api模拟→CPA→gcli、直gcli、直Aito、CPA→Aito，流式/非流式，双方调用边和精确出处 |
| 实例身份 | 各服务至少2并发实例；相同instance重启boot变化；多worker用真实多进程验证并明确无POSIX fork证据 |
| ID输入 | 无ID、仅request-id、标准traceparent、非法/重复头、两个调用方复用ID，内部ID不被覆盖，别名不合并可信树 |
| 重试 | 跨实例路由及429/503/嵌套重试，实际HTTP发送数、attempt独立计数，gcli重置No不被合并 |
| 生命周期 | 取消/读错误/错误帧/缺终止/阻断/空回复/工具/媒体；正常关闭flush、硬杀缺尾；未观察事件保持缺失 |
| 400修复 | 连续空消息、纯空白、清理后末尾model、工具对话及全空既有错误；不注入伪造用户文本 |
| 87/usage | 输出87的来源逐阶段证明；candidate+reasoning差异、重复累计usage不重复加、零与缺失分离，不声称还原历史平台计费算法 |
| 非流式限速 | 有限且确定目标参数，原始token来源、实际等待、完整响应可见时间；流转非流也覆盖；不能用仅首字或四舍五入速率断言失效 |
| DEBUG/基础记录 | DEBUG关闭仍关联；动态关闭的partial/unknown；普通模式不出现语义详细字段，缺日志明确标记 |
| 离线抗冲突 | 重复导入、坏行、时钟偏差、伪造/复用ID、来源不可信/重复父节点；不凭trace/time合并 |
| 混合版本 | 至少明确冻结当前与历史样例/未知schema混合的降级行为，真实旧进程如未运行明确标记 |
| 保持原业务 | 开关前后body/status/发送顺序/代理/取消/重试计数等价，流式背压用确定性握手验证而非只计总耗时 |

## 审核补充

- CPA当前scope conductor_gemini_family；R1样例只是历史版本。模型辅助认证/元数据call不误计为业务attempt。
- CPA本地观察超限为unknown；上游/实际交付聚合分别撤回；不能将last-observed usage冒充最终结果。
- Gemini现有读错误后DONE完成帧属既有行为，本批不擅自修；对比诊断error与客户端帧，另列风险。Claude handler源码不对message_stop早退；读错误实测与Home/bootstrap缺口须分别标注。
- gcli退出时shutdown_asyncgens晚到call记录须核验是否落盘；有server终局不证明call完整。文件接近16MiB仅疑似缺口，小文件也不能默认完整。容量/轮转/总配额是未部署的已知运维限制。
- Aito实际没有HttpBoundary生产caller；不要声称浏览器images/VNC外部HTTP已覆盖；公共logSeq与legacyseq/ACK/deliveryOutcome分开。
- Windows无CGO/race/WSL/Docker daemon，POSIX fork/race未执行不能写通过，后续部署门槛明确列出。

## 交付与回退

在CPA只新增联调驱动/夹具/报告/配置样例，不改其他仓库。缺陷由协调方返回02/03/05/06对应原新任务修订并重审，再使用新的固定SHA跑受影响矩阵。全量CPA Go回归+server/工具build，跨仓若需回归仅使用隔离入口。生成逐场景结果、脱敏日志、命令退出码、摘要、三仓准确SHA、配置和停用/回退说明。不得替部署者改远程配置或合并主分支。

最终本窗口复核实际证据与差异，并调用claude-opus-5-5进行DIAG-07及整体契约一致性审核；待审任务不能自行推送或宣布通过。

## 配置与可信性补读结论

三个批准实现均未新增生产入站认证信任适配器，callerAlias保持null/unknown，默认X-Request-Id为未核验查询别名。DIAG_PEERS只授权出站头注入，不认证调用方。正常双边关联应依离线操作者声明的受控导出来源、唯一trace/span父子证据以及可用响应身份核对；不要为了联调通过而启用未实现的callerTrust或伪造callerAlias。两个调用方复用相同ID时，当前运行时应保留多个候选和scopeUnknown，不能声称能从匿名header识别其真实调用方。

配置文档应清楚区分：CPA/gcli读取边界可刷新peer快照，Aito生产仅启动读取DIAG_PEERS/ACCESS_LOG_ENABLED，测试reloadPeers/setAccessEnabled不等同于生产热更新。所有资源environment/deployment/instance是本地配置；gcli无已确认平台replica适配，不能猜主机名代表稳定实例。
