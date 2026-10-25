# Gemini Enterprise 原生格式兼容差异与逐项验证清单

本清单用于逐项核对 Gemini Enterprise 与 Gemini AIS、Gemini CLI、Antigravity 的 Gemini 原生请求和响应行为。当前 Enterprise 实现能够处理文本、多轮历史及部分内置工具结果，但不能据此认定它具有完整的 Gemini GenerateContent 语义。每一项都需要分别确认字段是否保留、语义是否实现，以及调用方是否正确解析结果。

## 版本与适用范围

| 项目 | 基线 |
| --- | --- |
| 整理日期 | 2026 年 10 月 24 日，Asia/Shanghai |
| 后端固定提交 | `0d13b48ece3ed69fa138747f7ae02b1a36d41302` |
| 基线分支 | `codex/gemini-enterprise-20261023` |
| 比较入口 | Gemini 原生 `generateContent` 与 `streamGenerateContent`；必要时对照 OpenAI SDK 和 new-api 转换后的同义请求 |
| Enterprise 实际上游 | Discovery Engine `assistants:streamAssist` |
| 原生能力判据 | 有效字段或等价语义得到保留，返回信息满足约定；HTTP 200、错误用例通过、模型出现在目录中均不能单独证明支持 |
| 基线之外的工作 | 其他窗口已发布的函数降级提交 76d0fb8f 单独叠加说明；其他未提交的媒体等适配不计入此固定基线 |

基线章节代码链接固定指向 0d13b48e，当前版本叠加说明另指向 76d0fb8f，避免工作区变化改变对照结论。日期和历史结果属于对应版本，不构成后续版本的自动验收结果。

用户已明确要求先验证、再审定修改方案。本清单保留17:56只读阶段及18:27开始的新一轮真实Google直连；最新结论以“89项真实直连Google验证”节为准。修改实现、配置、提交、推送或部署均不在本阶段执行。

### 渠道对照的含义

- **AIS**：`GeminiExecutor` 及 Gemini 到 Gemini 的归一化逻辑，通常访问 Generative Language API。它会修复角色、签名、工具 Schema、函数结果名称，追加必要的边界 user，并按模型封顶输出长度，不能概括成完全原样透传。
- **Gemini CLI**：`GeminiCLIExecutor` 访问 Cloud Code Assist。此快照中未找到 Gemini 到 `gemini-cli` 的内建翻译注册；没有匹配翻译器且没有插件时，注册表只更新根级 model 后回退原请求。CLI 执行器却按 `request.*` 处理字段。因此 CLI 列是执行器代码参考，必须先核验实际运行环境是否由插件或其他入口补齐转换链。
- **Antigravity**：保留 Gemini 请求的主体结构并作包装、Schema 清理、签名及函数回传修复。Gemini 路径内建逻辑会在最终 Payload 规则之前删除 `request.generationConfig.maxOutputTokens`，并删除 `request.safetySettings`；最终用户 Payload 规则仍可能重新设置这些值。已有结束原因可被保留，缺省时也可能合成 STOP。
- **Enterprise**：独立的文本 normalizer 将历史、系统指令打包为 `query.text`，再重建 Gemini 响应。当前限制主要来自这一适配合同，不能全部推断为 Google Enterprise 产品的能力上限。

依据：[AIS 归一化][src-ais-normalize]、[AIS 执行器][src-ais-executor]、[CLI 执行器][src-cli]、[翻译注册表回退][src-registry]、[内建翻译注册][src-translator-init]、[Antigravity 输入转换][src-ag-normalize]、[Antigravity 最终请求][src-ag-request]、[Enterprise 请求构建][src-ge-executor]。

## 验证状态与结果判定

下列“基线行为”是固定源码的静态结论，除标注历史记录的条目外，不能当作真实上游验收结果。已有离线回归覆盖情况与新字段的适配结论分别记录。

清单共 89 个编号，分为历史结构 8 项、媒体 6 项、工具函数 14 项、生成配置 24 项、思考签名 5 项、顶层选项 4 项、本地校验 8 项、响应流式 10 项、响应边界 4 项、API 入口 3 项和错误 3 项。“历史通过”仅指历史记录中的模型、入口和样本范围，不表示该行所有变体都做过真实调用。本轮本地行为验证也不能自动升级为原生能力支持。

| 结果 | 使用条件 |
| --- | --- |
| 待验证 | 尚未完成该项要求的离线、真实入口和客户端检查 |
| 原生支持已确认 | 字段经合法规范化后进入上游，且支持所要求的原生语义；有对应响应和客户端证据 |
| 适配支持已确认 | 用提示词、格式重建等方式实现约定范围，差异明确并可接受；不能写成完整原生支持 |
| 明确不支持 | 真实内容无法无损表达，入口明确拒绝且不发送生成请求；拒绝用例通过不等于能力支持 |
| 忽略已确认 | 请求成功，但字段没有进入有效上游请求或没有实现其语义；不记为原生适配通过 |
| 失败 | 字段被误拒绝、内容丢失、参数失效却冒充生效、响应错误或客户端不兼容 |
| 待模型权限 | 账号或模型权限条件未满足；不据此判断字段适配失败 |
| 环境受阻 | 测试程序被系统策略等阻止启动；不记为测试通过，也不据此判断实现失败 |
| 不适用 | 目标 API 或模型本身不提供该功能，必须写明判断依据 |

已确认的安全和完整性保护无需为了“兼容”关闭。调整校验前，应先证明样本对目标 API 和模型有效，并说明如何避免丢失其实际语义。

## 各渠道主要差异

表中“保留”表示存在对应代码处理或转发路径，不代表每个模型、权限和目标 API 版本都支持。

| 能力 | AIS | Gemini CLI | Antigravity | Enterprise 基线 |
| --- | --- | --- | --- | --- |
| 多轮与系统指令 | 保留原生结构，修复边界角色 | 执行器处理包装请求，转换链需先核验 | 保留包装后的结构，修复边界角色 | 序列化为文字上下文，系统指令不再是原生独立字段 |
| 多模态输入 | 有原生媒体处理路径 | 有图片适配代码，端到端待确认 | 有 inlineData 等处理，部分远程文件来源会拒绝 | 当前文本入口拒绝实际媒体 |
| 自定义函数 | 声明转换、调用与结果回传修复 | 有工具 Schema 清理代码 | 有函数名称、声明和结果修复 | 声明、调用和结果均不支持 |
| 采样与停止参数 | 多数保留，输出长度按模型封顶 | 按 request.generationConfig 清理和保留 | 有模型及字段特定清理，输出长度有删除规则 | 除响应 MIME 外基本忽略 |
| 思考配置和签名 | 接入思考处理，清理不兼容签名 | 有思考处理代码 | 有思考与签名回放处理 | thinkingConfig 不消费；历史签名忽略，隐藏思考不进入文本 |
| JSON Schema | responseSchema 转换到目标字段 | 清理响应 Schema | 转换并清理响应 Schema | Schema 忽略，JSON 由提示词和结果校验实现 |
| safetySettings | 有传递及默认安全设置处理 | 不存在 Enterprise 式全局白名单，入口仍需实测 | 内建路径删除该字段 | 输入字段忽略 |
| cachedContent | 原生请求保留路径 | 包装及实际目标能力待确认 | 转发能力取决于实际目标 | 忽略，不加载所引用缓存 |
| countTokens | 有实现 | 有实现，转换链需核验 | 有实现 | 直接不支持；usage 使用本地估算 |
| Gemini 响应 | 同协议响应透传 | 有上游响应处理路径 | 重建时尽量保留已有终态及部分元数据 | 单 candidate，成功统一 STOP，缺失多项原生元数据 |
| JSON 流式 | 无 Enterprise 这项统一整段校验机制 | 以实际执行路径为准 | 以实际模型执行路径为准 | 等完整 JSON 通过校验后才发送正文 |
| 响应大小 | 常用 SSE 扫描上限 50 MiB | 常用 SSE 扫描上限 50 MiB | 常用 SSE 扫描上限 50 MiB | 可见输出 4 MiB、32768 个可见事件；总上游 32 MiB、SSE 单行 16 MiB |

## 当前版本只读验证

2026 年 10 月 24 日 17:56（Asia/Shanghai）只读核对：主工作区、远程目标分支、线上应用版本及 Zeabur RUNNING 部署均为 `76d0fb8f73f3fc28b4d7f0a2c729787a64362a6c`。该提交由另一个窗口此前完成发布，本阶段未执行发布操作。业务配置与已保存基线一致，当前账号的稳定资源和 OAuth 客户端绑定与既有真实探测记录相同。

### 新版独立复核结果

| 项目 | 本阶段结果 | 结论边界 |
| --- | --- | --- |
| 现有 Enterprise translator 测试 | 34 个顶层测试，249 个包含父级和子用例的通过事件，exit 0 | 新版文本降级和现有解析、输出断言通过，不代表原生函数调用已实现 |
| 现有双 SDK 离线脚本 | OpenAI 3.36.1、Google GenAI 2.32.0，22 项通过，exit 0 | 本地假上游和 SDK 协议解析，不是新增 Google 真实生成验收 |
| 版本稳定性 | 两组运行前后均为固定 76d0fb8f，工作区 clean；SDK 脚本 SHA 未变 | 未混入其他窗口的工作树改动，没有新增或修改测试源文件 |
| 输出与流式代码差分 | Output、Parser、媒体分类、最终校验、Token helper、操作门禁共六个文件与 0d13b48e 相同 | 旧版 17 个边界检查可作同源码行为依据，但不能写成已在新版重新执行全部边界 |
| 系统与线上状态 | 本轮选定测试无策略拦截；只读线上版本与配置核对成功 | 不替代历史全量受阻记录，不修改系统策略，不恢复或改写服务器历史 |

本阶段新增 Google 生成调用为 0，源码、服务器配置和部署操作数均为 0。此前 171 个诊断用例含独立临时诊断源，它们属于前一阶段且已清理归档；本阶段没有再次新增这类源文件。

### 函数行为的版本叠加

下方逐项表仍描述固定 0d13b48e 基线。核对当前函数行为时，按以下增量解释，不能继续把所有旧拒绝结论直接套到线上。

| 对应编号 | 0d13b48e 基线 | 76d0fb8f 当前行为 |
| --- | --- | --- |
| `GE-TLS-01` | Gemini 函数声明在工具白名单处拒绝 | 形状合法的声明被忽略，声明 Schema、说明和默认参数不写入有效上游字段或可执行提示；坏类型仍拒绝 |
| GE-TLS-02、GE-TLS-03 | 非空函数调用和结果历史拒绝 | 历史名称、参数、关联信息和结果转成标注 JSON 文本；不执行函数，也不保留原生结构化调用、角色或配对调度语义 |
| GE-TLS-04 至 GE-TLS-08 | 部分调用模式拒绝，AUTO/NONE 等只接受后忽略 | 合法控制项接受后忽略；required、指定函数、ANY 的原生调用义务未实现，不能用普通文本回答当作完成强制调用；VALIDATED 的约束也没有原生落实 |
| GE-HST-06 的 OpenAI 相邻入口 | 只接受文本聊天角色 | 新增 tool/function 历史的文本表达，并映射到会话上下文；不代表 Gemini 可以直接使用任意 function role |
| `GE-VAL-07` | Payload 最后应用且仅一次 | 继续保留该顺序；新增处理和服务端警告在最终 Payload 之前 |
| GE-OUT、GE-LIM、GE-API 的相关实现 | 本清单已记录输出和 API 限制 | 六个相关文件未改变，大小预算、JSON 缓冲、引用清理和 countTokens 边界并未因忽略函数而消失 |

旧版 legacy `functions` 单独字段原本可能按未知字段忽略，`function_call=none` 也原已允许，不能笼统写成所有旧式函数字段曾经都返回 400。当前 legacy 的 `required` 字符串仍不是合法的旧式选择形式。

新版依据：[函数降级处理][src76-functions]、[输入归一化][src76-request]、[降级测试][src76-functions-tests]、[现有 SDK 脚本][src76-sdk]。这是对已存在版本的核对，不代表本阶段另行选择或实施该策略。

### 可复用的真实上游证据

已核对另一窗口保存的脚本、摘要、加密原始响应及来源校验，避免重复消耗模型额度。

| 真实样本 | 已确认结果 | 可支持的结论 |
| --- | --- | --- |
| 合法文本 StreamAssist 控制请求 | 200，包含 IN_PROGRESS 到 SUCCEEDED 和可见控制标记；响应资源父级匹配所保存 engine | 该资源的文本路径可用；独立控制请求快照未完整归档，不能从响应重建其确切 model/token 请求值 |
| 顶层注入 tools | 400 INVALID_ARGUMENT，未知字段 tools | 这一种 StreamAssist 注入形状被 Google schema 拒绝，不证明所有 Google 工具路径都不可用 |
| toolsSpec 内注入 functionDeclarations | 400 INVALID_ARGUMENT，未知字段 functionDeclarations | 这一种按请求传声明的形状被拒绝；不能推导 toolRegistry 或 agent 资源能力不可用 |
| 不透明 streamGenerateContent 探测 | 当前资源、v1alpha 和所用封包返回 404 Method not found | 当前不能将它当作已验证可替换入口；不能推广到所有企业环境、区域、可见性或协议封包 |
| 新版六次真实 SDK 文本降级 | 两个 Pro 模型各三次：声明普通、历史结果普通、历史结果 SSE；均有文本及正常结束，账号用量各增加三次 | 证明已测样本文本降级可用，客户端函数实际执行次数为 0；不证明原生工具闭环 |
| 十四项真实服务错误检查 | 七模型乘普通/SSE，使用 functionDeclarations=false 的坏类型 | 证明坏类型的本地拒绝和公开错误行为；不是合法函数请求适配或 Argon 权限验收 |

旧探测最初的摘要因 Google 错误为数组而误报未识别字段拒绝；原始结果保留，复核版本已按原始加密响应确认两个 400，不用复核文件覆盖首次结果。当前稳定账号绑定与旧探测一致，不等于每份缺失的原始出站请求都可独立重建。

六个旧真实 SDK 样本均未取得客户端可见的忽略字段响应头，后续应用日志有六条函数忽略警告。服务端可诊断与客户端能感知降级是两个不同问题；不能因为测试夹具能返回该响应头，就假定 new-api 实际已看到提示。

当前公开的 [GenerationSpec][official-ge-generation-spec] 及旧版 [AssistGenerationConfig][official-ge-old-generation] 仅列 modelId，没有给出把 temperature、停止词或 JSON Schema 原样放入该对象的合同；[ToolsSpec][official-ge-tools] 也不是标准 GenerateContent 的按请求函数声明格式。不能简单移除本地校验后直接透传这些字段，并据此预期兼容。

## 89 项真实直连 Google 验证（2026-10-24）

本节是用户明确授权后的新一轮实证，优先于前文只读阶段和后文基线表的“待验证”状态。使用现有 OAuth 客户端、主体和资源，未更换项目、创建配置或修改代码。时间为 Asia/Shanghai；生产源码参照仍为 76d0fb8f，固定审计源码为 0d13b48e。

请求时间 2026-10-24T18:27:48.901743+08:00 至 2026-10-24T18:34:55.956306+08:00，共 **72 次 Google 官方 API HTTPS 请求**，实测最高 **3 个并发**：69 次 StreamAssist（含合法生成、字段拒绝及结构对照）、1 次 streamGenerateContent、1 次 assistant 元数据 GET、1 次权限查询。另有一次 OAuth Token 刷新用于凭证取得，不计入这72次。

所有请求均由本机直接发送到 discoveryengine.googleapis.com 或 cloudresourcemanager.googleapis.com，禁用环境 HTTP 代理和重定向，未经过 ultra-cli2、new-api 或本地假上游。请求与完整响应均有 SHA256，原始内容及凭证仅以当前 Windows 用户 DPAPI 加密留存；隐藏思考只汇总字段与数量。

**30 个 HTTP 200 中只有 28 个是 SUCCEEDED 答案**；另一个是元数据 GET，另一个是别名对照的 SKIPPED。其余为40个400、1个403、1个404。没有429、断流或因本轮工具预算截断的样本，不补写未发生的真实结果。

### 状态口径与覆盖

89 个编号均有独立记录，编号唯一且无遗漏。一个公共字段容器已被明确拒绝时，依赖它的模式共用阻断证据；没有重复发送同样的404/400来凑89次调用。计数是编号主状态，不是89项完整原生能力的通过率。

| 主状态 | 项数 | 含义 |
| --- | ---: | --- |
| 上游样本已验证 | 5 | 当前样本的上游能力得到实际响应或语义证据；不等于代理已完整适配、全部模型或全部变体通过。 |
| 候选路径被上游拒绝 | 33 | Google明确拒绝当前候选路径，或前置公共容器阻断；不能推广成整个产品不支持该功能。 |
| 当前原生方法不可用 | 1 | 当前资源/API/官方外层封装返回Method not found；没有据此断言所有企业环境不可用。 |
| 部分观测／语义待定 | 23 | 只确认部分字段、结构、文字适配或源响应；完整原生语义、客户端效果或子变体仍未证实。 |
| 本地验证项 | 21 | 主要判据是本地normalizer、Payload、parser、路由、预算或SDK行为；即使附有源响应观察，也不是Google限制。 |
| 前置条件不足 | 6 | 缺有效媒体/缓存/已配置测试工具或可确认的清理条件；不是“不支持”或已证实操作权限缺失。 |

权限与订阅另按实际错误标记：Argon的单次模型控制为400 FAILED_PRECONDITION，正文明确免费试用不提供该模型，需要付费订阅；这是**订阅资格受限**，不是字段400或IAM 403。权限查询的403 PERMISSION_DENIED带SERVICE_DISABLED，表示该查询依赖的服务未启用；上传、removeContextFile和sessions.delete各操作权限均仍为未知，不能写成“已确认缺权限”。未修改IAM或启用任何服务。

### 本轮关键发现

- 普通控制和alt=sse控制都成功；513轮历史与超过4MiB的空白填充上下文均完整发送，首中尾标记按顺序取回。空白填充只验证字节门槛，不证明高token密度的上下文上限。
- 续写、空占位和系统要求选择均返回正确标记，但保留了合成提示的尾句点；无句点严格字面断言未过，原始结果与复核均保留，不把包含标记升级为严格输出通过。续写没有重复已有前缀。
- 真实内置代码产生executableCode及codeExecutionResult，随机合成输入的SHA256与本机值完全一致；把该真实结果再作为可见文字回传也能取回。后者是文本回放，不是原生事件往返。
- 空webGroundingSpec产生真实来源metadata；URL提示样本的grounding为空。搜索可用不能推导指定URL已抓取，来源数据也尚未在本轮真实new-api客户端渲染验证。
- **公开GenerationSpec字段表并不完整：thinkingConfig被Google实际识别。** budget=128、LOW档位、includeThoughts true/false均有成功样本；错误类型和未知子键被400拒绝，排除仅凭200猜测容器存在。预算和档位效果未证实；同prompt/模型的includeThoughts true和false均有4个thought:true节点，没有观察到关闭效果。
- 当前StreamAssist拒绝tools、functionDeclarations、toolConfig、urlContextSpec以及所测普通生成配置候选。合法原生参数被放入错误目标合同仍会被拒绝；不能通过单纯移除本地白名单解决。
- 当前资源的官方model+httpBody封装streamGenerateContent仍404。图片inlineData与远程fileData候选也被明确拒绝；上传式媒体读取因清理前提未确认而未执行，不能把这两种结论混为产品无媒体能力。
- 上游既有合成session两次调用可准确回忆随机标记，第二次未携带标记或旧历史；这是Google会话状态已验证，当前代理未回传/复用该内部状态。未显式调用session创建或删除接口；不能据此保证Google内部没有生成会话。

- 会话标志存在合同差异：63个请求携带isSessionLess=true，其中25个成功答案均返回非空sessionInfo.session；[官方请求合同](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist#request-body)则说明该标志应避免建立会话并返回空session字段。本轮没有查询持久化状态，也没有删除这些引用；不能把“发送了sessionless标志”写成“已证明无会话创建”。

### 七模型控制请求

| 请求的modelId | 真实结果 | 边界 |
| --- | --- | --- |
| gemini-3.8-flash | 200；SUCCEEDED，控制标记核对通过 | 单个文本控制；不代表全部字段或流式矩阵通过 |
| gemini-3.7-flash | 200；SUCCEEDED，控制标记核对通过 | 单个文本控制；不代表全部字段或流式矩阵通过 |
| gemini-3.6-flash | 200；SUCCEEDED，控制标记核对通过 | 单个文本控制；不代表全部字段或流式矩阵通过 |
| gemini-3.5-flash | 200；SUCCEEDED，控制标记核对通过 | 单个文本控制；不代表全部字段或流式矩阵通过 |
| gemini-3.1-pro-preview | 200；SUCCEEDED，控制标记核对通过 | 另有alt=sse控制；字段矩阵按适用模型分配 |
| gemini-2.5-pro | 200；SUCCEEDED，控制标记核对通过 | 单个文本控制；不代表全部字段或流式矩阵通过 |
| gemini-4-argon | 400 FAILED_PRECONDITION；免费试用不提供，订阅资格受限 | 仅一次权限/订阅控制；未重复、未做Argon字段矩阵 |

Google这些样本未返回可用于逐项验证的原生modelVersion；表内记录请求modelId及实际响应，不虚构内部版本确认。两种SDK的既有本地回归和历史真实发布继续单列，本轮直连HTTP不能代替代理、SDK及实际new-api的端到端验收。

### 89 项逐项记录

每行证据标签对应本轮google-calls.json中的精确请求和响应摘要，原始包为同标签的-request.dpapi/-response.dpapi。本地项复用此前已执行诊断或既有回归，并非本轮重发Google。

#### HST：历史结构

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-HST-01` 257、513、514 轮历史 | 上游样本已验证 | 513轮正常历史及514轮大字节历史均SUCCEEDED；首中尾标记全部正确。发送完整role-labelled JSON文字上下文；Google接收的是query.text，不是原生contents数组。257轮未另发重复请求。 | `hst-01-513turns`、`hst-02-over4mib` |
| `GE-HST-02` 规范化上下文超过 1 MiB，并覆盖 2 MiB、4 MiB | 上游样本已验证 | query.text超过4MiB，请求体4,277,156字节，首中尾标记均正确。主体为空白填充，证明该样本无1MiB/4MiB字节门槛，不证明高信息密度或任意token长度都可用。原始请求及哈希加密保存；无截断、摘要。 | `hst-02-over4mib` |
| `GE-HST-03` 最后一个非空轮次为 assistant 或 model | 部分观测／语义待定 | 末尾assistant已有PREFIX_，真实返回CONTINUATION_OK.且不重复前缀。原合成提示以句点结束；标记和续写行为正确，但无句点严格字面断言失败并保留。是文本适配续写，不是原生prefill；本轮只做普通直连，已有离线/历史SDK普通和流式另列。 | `hst-03-assistant-tail` |
| `GE-HST-04` OpenAI 空串、null 或缺省 assistant content；Gemini 空 model parts 数组或仅签名 Part | 部分观测／语义待定 | 两个已归一化空assistant占位样本均回答最后非空user，带尾句点和外层换行，无句点严格字面断言未过。两者在StreamAssist层为相同的空文字表达，不能由此证明Google识别null、缺省parts或签名Part；这些源格式变体由已有本地占位回归验证。 | `hst-04-empty-assistant`、`hst-04-signature-only` |
| `GE-HST-05` systemInstruction 与 user 存在不同要求 | 部分观测／语义待定 | role-labelled上下文中的系统标记SYSTEM_OPTION被选中，响应带尾句点，无句点严格字面断言未过；仅一次经验性优先级样本。没有发送原生systemInstruction，不能保证等同原生系统层级。 | `hst-05-system-priority` |
| `GE-HST-06` 缺省 role，以及显式 assistant、function、其他非法 role | 本地验证项 | 源role缺省、推断、合法性和角色修复属于本地normalizer；Google仅收到合并query.text。复用request-probes/history测试及76版本审计；API-03原生入口不可用，无法在该入口直接检验源role。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-HST-07` 一个 Content 内多个 text Part，含换行、空格、Unicode | 本地验证项 | 多个text Part的无分隔拼接、Unicode及边界属于本地转换行为；原文顺序在已有本地测试核对，不能把单个query.text回答当作原生Part边界保留。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-HST-08` Content 附加字段或 Part 新增字段 | 本地验证项 | Content/Part白名单与元数据空值处理属于源输入本地校验；共享原生入口404。未知字段是否应保留仍待适配决策，本轮没有改白名单。 | 既有本地诊断／回归；无独立Google请求 |

#### MED：媒体输入

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-MED-01` text 加有效图片 inlineData | 部分观测／语义待定 | 本机解析有效PNG已作为query.parts.inlineData候选直连，Google400 Unknown inlineData；只否定该形状。上传式真实图片理解未验：清理权限无法确认，未上传。 | `resource-inline-png-candidate` |
| `GE-MED-02` text 加有效 fileData 图片 URI | 部分观测／语义待定 | 使用Google官方公开scones图片gs URI作为query.parts.fileData候选，Google400 Unknown fileData；没有读取文件内容，不能证明远程引用读取功能不支持。 | `resource-filedata-public-reference-candidate` |
| `GE-MED-03` 短音频 inlineData 或有效音频文件引用 | 前置条件不足 | 缺经过验证的小型MP3/既有合成音频引用，且附件清理权限未确认；不拿WAV或假字节判断音频理解能力。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-MED-04` 短视频及 videoMetadata 的时间段或采样配置 | 前置条件不足 | 缺有效小型MP4及可确认的清理前提；未创建视频生成任务，不能把视频理解标不支持。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-MED-05` 小型 PDF 或文档文件 | 前置条件不足 | 已本地生成并严格解析三页PDF，未上传。testIamPermissions403 SERVICE_DISABLED，add/remove/delete权限各自仍未知，故PDF理解保持前置不足。 | `resource-cleanup-permissions` |
| `GE-MED-06` Part 级 mediaResolution、videoMetadata 等附加媒体选项 | 前置条件不足 | 原生入口不可用且尚无上传媒体基线；videoMetadata/媒体选项未独立验证，不在纯文本上附加选项冒充合法媒体测试。 | 既有本地诊断／回归；无独立Google请求 |

#### TLS：工具与函数

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-TLS-01` functionDeclarations 及受目标支持的另一拼写，另测空数组或 null | 候选路径被上游拒绝 | 两个含合法函数名、说明和对象参数Schema的声明均被Google400明确拒绝：顶层tools、toolsSpec.functionDeclarations为Unknown name。仅否定这两条当前StreamAssist候选路径；未据此宣称所有Google接口/模型不支持函数。 | `tls-01-top-tools`、`tls-01-toolsspec-functions` |
| `GE-TLS-02` 历史 model functionCall，含合法参数及调用标识 | 部分观测／语义待定 | 历史调用名称、id和参数标记在文字适配后真实回答正确。未向Google传原生functionCall，也未执行客户端函数；原生结构化历史回传仍未验成。 | `tls-02-call-history-text` |
| `GE-TLS-03` user functionResponse，含结构化结果 | 部分观测／语义待定 | 历史调用id与结构化结果标记经文字表达均可准确取回；无真实函数调度、配对或函数调用结果闭环，不能标原生functionResponse支持。 | `tls-03-result-history-text` |
| `GE-TLS-04` functionCallingConfig.mode 为 ANY | 候选路径被上游拒绝 | 公共NONE控制请求因顶层toolConfig未知而400；ANY/VALIDATED/AUTO及allowedFunctionNames共享容器阻断，未通过已拒绝容器逐值重发。不能将它当作各模式的独立原生语义判定。 | `tls-call-config-container` |
| `GE-TLS-05` functionCallingConfig.mode 为 VALIDATED | 候选路径被上游拒绝 | 公共NONE控制请求因顶层toolConfig未知而400；ANY/VALIDATED/AUTO及allowedFunctionNames共享容器阻断，未通过已拒绝容器逐值重发。不能将它当作各模式的独立原生语义判定。 | `tls-call-config-container` |
| `GE-TLS-06` functionCallingConfig.mode 为 AUTO | 候选路径被上游拒绝 | 公共NONE控制请求因顶层toolConfig未知而400；ANY/VALIDATED/AUTO及allowedFunctionNames共享容器阻断，未通过已拒绝容器逐值重发。不能将它当作各模式的独立原生语义判定。 | `tls-call-config-container` |
| `GE-TLS-07` NONE、空 functionCallingConfig 对象、null 配置 | 候选路径被上游拒绝 | 公共NONE控制请求因顶层toolConfig未知而400；ANY/VALIDATED/AUTO及allowedFunctionNames共享容器阻断，未通过已拒绝容器逐值重发。不能将它当作各模式的独立原生语义判定。NONE有真实样本；空对象、null分支仍按本地用例区分。 | `tls-call-config-container` |
| `GE-TLS-08` allowedFunctionNames 及其目标支持的拼写 | 候选路径被上游拒绝 | 公共NONE控制请求因顶层toolConfig未知而400；ANY/VALIDATED/AUTO及allowedFunctionNames共享容器阻断，未通过已拒绝容器逐值重发。不能将它当作各模式的独立原生语义判定。 | `tls-call-config-container` |
| `GE-TLS-09` 空 googleSearch 开关 | 上游样本已验证 | 空toolsSpec.webGroundingSpec真实200 SUCCEEDED，观察到2条来源metadata，证明当前模型/资源的内置web grounding样本可用；不等于全部GoogleSearch选项或客户端引用映射通过。 | `tls-09-web-grounding` |
| `GE-TLS-10` googleSearch 内合法非空配置 | 候选路径被上游拒绝 | 官方WebGroundingSpec公开为空类型；将GoogleSearch的合法timeRangeFilter作为迁移候选放入该对象，Google400 Unknown name拒绝。未将其冒称合法StreamAssist非空选项，其他选项不据此逐项下结论。 | `tls-10-option-migration-candidate` |
| `GE-TLS-11` urlContext 与一个已知内容的测试 URL | 部分观测／语义待定 | toolsSpec.urlContextSpec被Google400拒绝；URL读取提示+webGroundingSpec请求SUCCEEDED。没有自控网页的独有值或访问日志，单凭正文/来源不能证明实际抓取，故URL原生读取尚未通过。 | `tls-11-url-candidate`、`tls-11-url-prompt-grounding` |
| `GE-TLS-12` codeExecution 与可核对计算 | 上游样本已验证 | Google实际返回1个executableCode和1个codeExecutionResult；对本轮随机合成字符串的SHA256与本机值一致。证明该资源可由执行提示触发真实内置Python；不证明codeExecution同名配置透传。 | `tls-12-code-prompt` |
| `GE-TLS-13` 本渠道返回的 executableCode、codeExecutionResult 作为 model 历史回传 | 部分观测／语义待定 | 将上一真实代码和执行结果按可见文字再传给Google，成功复取正确摘要；只验证事件转文字的回传，不是原生代码事件保真往返。空代码/结果的源合法性仍属本地项。 | `tls-12-code-prompt`、`tls-13-code-replay-text` |
| `GE-TLS-14` 目标 API 新增或其他合法 Tool 类型 | 前置条件不足 | 其他合法Tool依赖明确的已配置toolRegistry或受支持工具前提；当前assistant只读元数据未找到可安全复用测试引用。本轮不创建registry/agent、不执行未知外部动作。 | 既有本地诊断／回归；无独立Google请求 |

#### GEN：生成配置

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-GEN-01` temperature | 候选路径被上游拒绝 | 候选 `generationSpec.temperature` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-01` |
| `GE-GEN-02` topP | 候选路径被上游拒绝 | 候选 `generationSpec.topP` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-02` |
| `GE-GEN-03` topK | 候选路径被上游拒绝 | 候选 `generationSpec.topK` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-03` |
| `GE-GEN-04` maxOutputTokens | 候选路径被上游拒绝 | 候选 `generationSpec.maxOutputTokens` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-04` |
| `GE-GEN-05` stopSequences | 候选路径被上游拒绝 | 候选 `generationSpec.stopSequences` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-05` |
| `GE-GEN-06` seed | 候选路径被上游拒绝 | 候选 `generationSpec.seed` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-06` |
| `GE-GEN-07` candidateCount | 候选路径被上游拒绝 | 候选 `generationSpec.candidateCount` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-07` |
| `GE-GEN-08` responseMimeType 为 text/plain 或 application/json | 候选路径被上游拒绝 | 候选 `generationSpec.responseMimeType` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-08` |
| `GE-GEN-09` responseMimeType 为 text/x.enum 或其他目标合法值 | 候选路径被上游拒绝 | 候选 `generationSpec.responseMimeType` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。与GEN-08共用responseMimeType字段拒绝，未重复更换MIME值；不是单独的枚举生成验收。 | `gen-08` |
| `GE-GEN-10` responseSchema | 候选路径被上游拒绝 | 候选 `generationSpec.responseSchema` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-10` |
| `GE-GEN-11` responseJsonSchema | 候选路径被上游拒绝 | 候选 `generationSpec.responseJsonSchema` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-11` |
| `GE-GEN-12` responseModalities | 候选路径被上游拒绝 | 候选 `generationSpec.responseModalities` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-12` |
| `GE-GEN-13` speechConfig | 候选路径被上游拒绝 | 候选 `generationSpec.speechConfig` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-13` |
| `GE-GEN-14` imageConfig | 候选路径被上游拒绝 | 候选 `generationSpec.imageConfig` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-14` |
| `GE-GEN-15` generationConfig.mediaResolution | 候选路径被上游拒绝 | 候选 `generationSpec.mediaResolution` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-15` |
| `GE-GEN-16` responseLogprobs | 候选路径被上游拒绝 | 候选 `generationSpec.responseLogprobs` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-16` |
| `GE-GEN-17` logprobs | 候选路径被上游拒绝 | 候选 `generationSpec.logprobs` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-17` |
| `GE-GEN-18` presencePenalty | 候选路径被上游拒绝 | 候选 `generationSpec.presencePenalty` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-18` |
| `GE-GEN-19` frequencyPenalty | 候选路径被上游拒绝 | 候选 `generationSpec.frequencyPenalty` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-19` |
| `GE-GEN-20` responseFormat | 候选路径被上游拒绝 | 候选 `generationSpec.responseFormat` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-20` |
| `GE-GEN-21` translationConfig | 候选路径被上游拒绝 | 候选 `generationSpec.translationConfig` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-21` |
| `GE-GEN-22` audioTranscriptionConfig | 候选路径被上游拒绝 | 候选 `generationSpec.audioTranscriptionConfig` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-22` |
| `GE-GEN-23` enableAffectiveDialog | 候选路径被上游拒绝 | 候选 `generationSpec.enableAffectiveDialog` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。音频、图片、视频或对应特定模型的功能前提另列，普通文本模型的字段拒绝不能代替模态验收。 | `gen-23` |
| `GE-GEN-24` enableEnhancedCivicAnswers | 候选路径被上游拒绝 | 候选 `generationSpec.enableEnhancedCivicAnswers` 被Google400明确报未知字段。仅验证当前StreamAssist字段映射不成立；未确认对应原生参数的功能语义，不归因于模型能力。 | `gen-24` |

#### THK：思考与签名

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-THK-01` thinkingConfig.thinkingBudget | 部分观测／语义待定 | thinkingBudget=128在2.5Pro真实200 SUCCEEDED；wrong-type与未知子键均400，证实Google识别thinkingConfig及其结构。预算落实和执行深度未证实；不能按公开字段表缺失就标不支持。 | `thinking-config-container`、`thk-schema-wrong-type`、`thk-schema-unknown-option` |
| `GE-THK-02` thinkingConfig.thinkingLevel | 部分观测／语义待定 | thinkingLevel=LOW在3.1Pro真实200 SUCCEEDED；容器Schema识别已证实，但没有档位的因果效果或上游思考Token证据。 | `thinking-config-container`、`thk-02-level-low`、`thk-schema-wrong-type`、`thk-schema-unknown-option` |
| `GE-THK-03` generationConfig.thinkingConfig.includeThoughts | 部分观测／语义待定 | 2.5Pro同prompt、同模型只切includeThoughts，true/false均200且各4个thought:true节点；没有观察到关闭效果。只记录数量，不输出隐藏正文；字段接收不等于语义生效。 | `thinking-config-container`、`thk-03-thoughts-true`、`thk-03-thoughts-false`、`thk-schema-wrong-type`、`thk-schema-unknown-option` |
| `GE-THK-04` 文本附带 thoughtSignature 或 thought_signature，以及 new-api 占位值 | 本地验证项 | Part直接层签名按已有本地用例忽略，双别名冲突由本地校验；函数对象内部元数据仍有静态验证缺口，本轮未验证，不能宣称全部位置都过滤。Google未返回可供真实回放的thoughtSignature；不能把assistToken当签名。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-THK-05` model 历史 thought:true、仅元数据 Part，以及 user/system 标记为 thought:true | 本地验证项 | 隐藏model历史过滤与user/system隐藏标志校验属于本地；完整文字上下文回答不能验证原生hidden Part语义。已有本地回归保留，不伪造上游测试。 | 既有本地诊断／回归；无独立Google请求 |

#### OPT：顶层选项

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-OPT-01` safetySettings | 候选路径被上游拒绝 | 合法safetySettings数组在StreamAssist顶层被Google400 Unknown name拒绝。仅否定该候选映射，不评价产品默认安全策略或其他Google API的安全参数。 | `opt-01-safetysettings-candidate` |
| `GE-OPT-02` cachedContent | 前置条件不足 | 当前assistant只读元数据未找到有效缓存引用；未创建缓存，也未用不存在的cachedContent ID伪造失败。 | `resource-assistant-get` |
| `GE-OPT-03` labels、目标合法的新顶层选项及忽略提示 | 本地验证项 | 可选字段忽略、忽略字段提示头及其64字段/4096字节预算都是本地行为，Google响应不负责携带该提示头。64/4096提示预算本轮仅源码确认，未新跑该阈值边界；不能用直连Google200证明代理忽略参数已生效。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-OPT-04` Enterprise sessionInfo、assistToken 或会话复用 | 上游样本已验证 | 仅复用旧已知合成session，同身份/绑定/engine均确认；两条请求200SUCCEEDED，第二条未带nonce/完整旧历史但准确回忆随机标记。未创建agent/cache/registry，也未删除旧session；这证明样本的上游会话上下文，不证明代理已回传/复用sessionInfo或assistToken。 | `response-session-store`、`response-session-recall` |

#### VAL：校验与最终Payload

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-VAL-01` 同一 JSON 对象出现重复键 | 部分观测／语义待定 | 原始JSON中generationSpec.modelId重复且同值，Google200 SUCCEEDED并正确回答。源JSON重复键在本地拒绝已有证据；本轮未测试异值取首/取末，不建议据此放宽确定性解析保护。 | `val-01-duplicate-key` |
| `GE-VAL-02` 非对象、多个 JSON 值、非法 JSON 或非法 UTF8 | 部分观测／语义待定 | 原始字节直连：非对象、连续两个JSON和坏JSON均400；无效UTF8字节被Google200处理并生成。后者只说明上游解析宽容，不能证明无损保留，也不构成移除本地UTF8保护的理由。 | `val-02-nonobject`、`val-02-multiple-json`、`val-02-broken-json`、`val-02-invalid-utf8` |
| `GE-VAL-03` 超过 64 层 JSON 嵌套 | 本地验证项 | 64层递归是本地JSON安全/完整性校验；当前StreamAssist不接收Gemini generationConfig/schema，不能用未知字段拒绝代替合法深Schema验收，也不创建agent获取任意Struct入口。已有边界诊断保留。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-VAL-04` camelCase 与 snake_case 两种别名同时出现 | 部分观测／语义待定 | generationSpec与generation_spec同值同时出现，GoogleHTTP200但answer.state=SKIPPED，没有成功答案。不能算别名功能通过；只覆盖该StreamAssist别名对，源Gemini各别名仍由本地拒绝规则单独判断。 | `val-04-alias-both` |
| `GE-VAL-05` 空或 null 的 contents、parts、tools、generationConfig、toolConfig | 部分观测／语义待定 | Google对query.text为空串/null均400；源contents/parts/tools/generationConfig/toolConfig不是同一字段域，其空值差异只能由本地原始输入用例验证，不能用这两个错误推广。 | `val-05-query-empty`、`val-05-query-null` |
| `GE-VAL-06` 在最终 Payload 添加 generationConfig、原生 contents 或未知业务字段 | 候选路径被上游拒绝 | 最终StreamAssist body中添加合法形状的原生contents和generationConfig，两字段均被Google400 Unknown name明确拒绝；不能仅取消本地白名单而直接透传这些字段。 | `val-06-native-fields` |
| `GE-VAL-07` 完整历史转换后使用 Payload 覆盖或过滤 | 本地验证项 | 内建转换→最终Payload仅应用一次→只读校验的顺序只能在本地executor/公共入口回归验证；直接Google无从观察该顺序。已有通过结果保留，没有为了探测恢复过滤字段。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-VAL-08` StreamAssist 本身存在而本地白名单未开放的字段和模式 | 部分观测／语义待定 | assistant GET200；非空query.parts文本控制200 SUCCEEDED且标记正确，证明存在额外合法合同。非空fileIds、文件session、agentsSpec和RESEARCH/AGENT未验，未执行未知agent。 另有63个请求携带isSessionLess=true，其中25个成功答案仍返回非空sessionInfo；持久化未知，不能宣称无会话创建。 | `resource-assistant-get`、`resource-query-parts-control`、`control-streamassist-pro`、`control-streamassist-sse` |

#### OUT：响应与流式

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-OUT-01` candidate 数量、index、role 和多候选 | 本地验证项 | 本地单candidate/index=0/role=model是重建合同。fresh StreamAssist 返回answer/replies，样本没有原生candidates；不能由此断言所有Google入口都只支持一候选。 | `control-streamassist-pro` |
| `GE-OUT-02` 正常停止、长度停止、安全阻断等结束原因 | 本地验证项 | fresh真实状态为IN_PROGRESS后SUCCEEDED；本地成功重建STOP，没有读取原生finishReason。上游SKIPPED/坏尾/后续错误不应伪成功；未伪造MAX_TOKENS/SAFETY。 | `control-streamassist-pro`、`control-streamassist-sse` |
| `GE-OUT-03` modelVersion、responseId、promptFeedback、safetyRatings、概率等元数据 | 部分观测／语义待定 | 已逐字段记录fresh answer/name/createTime/replyId等来源字段及原生命名字段是否存在；opaque assistToken/sessionInfo仅记录字段存在，不导出值。不存在的原生字段不能伪造或泛化为产品不支持。 | `control-streamassist-pro`、`control-streamassist-sse`、`tls-12-code-prompt` |
| `GE-OUT-04` thought 输出和 thoughtSignature 回传 | 部分观测／语义待定 | fresh真实thought:true回复存在，本地会过滤；观察样本中未见thoughtSignature。thinkingConfig已被Google实际识别，不能称参数不支持；true/false或预算/level被接受不等于控制语义已证实，隐藏文本不公开。 | `control-streamassist-pro`、`thinking-config-container`、`thk-02-level-low`、`thk-03-thoughts-false`、`thk-03-thoughts-true`、`thk-schema-unknown-option`、`thk-schema-wrong-type` |
| `GE-OUT-05` grounding、来源、Unicode 引用区间和混合代码 Part | 部分观测／语义待定 | web-grounding样本真实返回references与segments，逐字节比对其形状和当前索引合同；URL提示样本textGroundingMetadata为空，不能用正文或200证明URL引用能力。本轮未在代理和最终new-api上验证真实来源渲染。 | `tls-09-web-grounding`、`tls-11-url-prompt-grounding` |
| `GE-OUT-06` application/json 的流式首个正文与分片 | 本地验证项 | JSON正文缓存至完整校验后发送，以及清理后丢失Sources/Supports/Annotations均为本地行为。既有专用诊断已复现；不让Google生成大JSON或假引用来验证本地缓冲。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-OUT-07` JSON 模式返回坏 JSON、Markdown 围栏、非对象 JSON | 本地验证项 | 本地Finish检查JSON语法/可选对象根并处理围栏；不是responseSchema验证。Google源响应200不等于满足本地JSON合同，已有JSON SDK及本地反例单列。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-OUT-08` 上游媒体或未知内容种类 | 部分观测／语义待定 | fresh控制中的inlineData为application/json+suggestions，严格形状UI元数据不算生成图像/音频/视频。媒体候选的schema错误和SERVICE_DISABLED前置失败不能泛化为媒体产品能力不存在；本地实际媒体拒绝另有离线证据。 | `control-streamassist-pro`、`resource-cleanup-permissions`、`resource-filedata-public-reference-candidate`、`resource-inline-png-candidate` |
| `GE-OUT-09` JSON 数组、连续 JSON、NDJSON、SSE、粘包、拆包、坏尾及缺少成功终态 | 部分观测／语义待定 | fresh数组JSON和SSE均通过严格UTF8/完整framing检查，真实SUCCEEDED终态已观察。本地拆包、坏尾、缺终态、DONE不足等完整性保护仍用既有诊断，不要求上游故意输出损坏协议。 | `control-streamassist-pro`、`control-streamassist-sse` |
| `GE-OUT-10` 已开始输出后取消、上游错误、断流或后续非法事件 | 本地验证项 | Started包括IN_PROGRESS/隐藏thought，即使客户端未见正文也不自动重放；取消、坏尾和后续错误不伪STOP属于本地保护。正常fresh SSE不能证明取消/错误重放行为，本轮未中断Google生成。 | 既有本地诊断／回归；无独立Google请求 |

#### LIM：本地响应预算

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-LIM-01` 内部 Text 累积 4 MiB 前后 | 本地验证项 | 4,194,304内部Text字节接受，+1拒绝；UTF8按字节，8字节代码例渲染24字节计入。这是本地预算；已有内存探针与相同源码对照，不要求Google生成16/32MiB验证，也不把Google诊断64MiB读取预算当产品限制。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-LIM-02` 32768 个 text/code/execution 逻辑事件前后 | 本地验证项 | 32766 text+1 code+1 execution接受，第32769可见事件拒绝；4097 grounding与done不计入。这是本地预算；已有内存探针与相同源码对照，不要求Google生成16/32MiB验证，也不把Google诊断64MiB读取预算当产品限制。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-LIM-03` 上游响应累计读取 32 MiB 前后 | 本地验证项 | 上游原始总量33,554,431接受，33,554,432含等号及+1均拒绝；源字节与可见文本不同。这是本地预算；已有内存探针与相同源码对照，不要求Google生成16/32MiB验证，也不把Google诊断64MiB读取预算当产品限制。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-LIM-04` SSE 单行 16 MiB 前后 | 本地验证项 | SSE含分隔符总行长16,777,216可接受；LF内容16,777,215、CRLF内容16,777,214；再增1字节失败。这是本地预算；已有内存探针与相同源码对照，不要求Google生成16/32MiB验证，也不把Google诊断64MiB读取预算当产品限制。 | 既有本地诊断／回归；无独立Google请求 |

#### API：操作入口

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-API-01` 原生 countTokens 与 usageMetadata | 本地验证项 | 代理原生countTokens明确未实现；usageMetadata为本地Gemma计数而非Google官方计数。未取得公开合法的本资源countTokens方法，未猜测/穷举方法；不能推出Google产品没有任何计数能力。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-API-02` 原生普通/流式操作与相邻 Responses、Claude 或跨协议入口 | 本地验证项 | source=response及显式operation门禁是本地客户端路由合同。fresh直接Google普通/SSE成功只能证明上游可调用，不替代Responses/Claude/new-api路由验证。 | `control-streamassist-pro`、`control-streamassist-sse` |
| `GE-API-03` Enterprise 官方 streamGenerateContent 作为可能的原生聊天入口 | 当前原生方法不可用 | 使用官方外层model+httpBody(contentType=application/json、base64原生contents)直连当前assistant:streamGenerateContent，Google404 Method not found。仅锁定当前资源/区域/API/封装，不推断所有租户或Google原生API不可用。 | `control-streamgeneratecontent` |

#### ERR：错误与客户端

| 编号／项目 | 主状态 | 实际观察与未验证边界 | 证据标签 |
| --- | --- | --- | --- |
| `GE-ERR-01` 本地字段错误、上游错误和流内错误的公开信息 | 本地验证项 | 不泄露渠道/Token/签名/正文是代理公开错误的本地要求。直接Google错误含Google产品或资源字段不构成代理隐私失败；源错误加密保留，公开分类不透传原始敏感值。 | 既有本地诊断／回归；无独立Google请求 |
| `GE-ERR-02` 本地 400、上游 400、模型权限、429、断流 | 部分观测／语义待定 | 复用root新模型控制：六个非Argon模型成功；Argon400 FAILED_PRECONDITION且明确免费试用权益限制，归entitlement_blocked，不是403 IAM。另有已测native入口404 MethodNotFound；资源API403的SERVICE_DISABLED不能写成已证实缺upload/delete IAM。未故意耗尽quota、伪造Token或重复Argon。 | `model-2-5-pro`、`model-3-5-flash`、`model-3-6-flash`、`model-3-7-flash`、`model-3-8-flash`、`model-4-argon`、`control-streamgeneratecontent` |
| `GE-ERR-03` Gemini SDK、OpenAI SDK 和 new-api 对普通及流式错误的解析 | 本地验证项 | 现有22项本地SDK回归及历史真实SDK结果单列。共享handler错误envelope不是Enterprise独有；本轮直连Google没有经过运行中的代理或用户new-api实例，不能记为new-api端到端错误解析通过。 | 既有本地诊断／回归；无独立Google请求 |

### 方案审定前的顺序

1. 优先评估已被上游识别的thinkingConfig候选，先证明预算/档位实际效果，并澄清includeThoughts与StreamAssist状态思考的关系；在此之前不承诺参数兼容。实施时仍须走中央思考归一化和最终Payload屏障。
2. 函数、采样、停止词、Schema及原生Part不能直接塞入当前StreamAssist。若需原生语义，先取得真正可用的原生控制入口或有明确合同的资源工具桥接，再决定T0/T1/T2；本轮不选择默认降级策略。
3. 媒体与缓存先补合法资源和清理条件。权限诊断SERVICE_DISABLED、实际操作IAM与Argon订阅是三个不同前提；本轮未启用API、授权角色、升级订阅或更换资源。
4. 本地Part/角色/null、JSON输出、引用映射、错误提示及四项预算独立审定。真实new-api、各协议SDK、流式取消和错误回归应在方案确认后的实现阶段完成，不能用本轮上游200替代。

### 本轮证据与保留范围

本轮证据位于enterprise-native-compatibility-doc-20261024/upstream-89-direct-20261024：

| 文件 | 用途 |
| --- | --- |
| google-call-summary.json、google-calls.json | 72次请求的来源、方法、状态、并发、字节数及请求/响应SHA256 |
| case-results-89.json | 89个唯一编号、主状态、完整原始分组记录和限制；编号集合核对通过 |
| root-cases.json、request-cases.json、resource-cases.json、response-cases.json | 四个并发分工的独立记录 |
| request-thinking-followup.json | 隐藏thinkingConfig的字段识别及includeThoughts true/false对照 |
| synthetic-semantic-review.json | 标记顺序、代码摘要严格相等，以及尾句点导致的严格字面不相等 |
| resource-preflight.json、resource-preflight-reviewed.json | 首版权限布尔歧义及后续unknown修正，原始403保留 |
| request-cases-before-thinking-followup.json | 保留首轮THK模板说明，最终记录已纠正为容器接收，未覆盖原始请求/响应 |
| response-session-result.json、response-source-observations.json | 真实既有合成会话回忆与源响应字段观察 |
| document-and-workspace-validation.json | 文档编号/证据/隐私核对、工作区和其他未提交改动保留结果 |

除本文件补充文档外，未修改仓库源码或既有测试，没有提交、推送、部署、服务器配置写入、IAM/系统策略变更、附件上传、创建缓存/agent或删除历史。会话复用用例在既有自有合成session追加两条测试；未显式调用会话创建/删除接口。63个请求携带isSessionLess=true，其中25个成功答案均返回非空sessionInfo；是否产生持久会话未验证，不能据请求标志保证不保存。历史Windows Application Control拦截记录和以前各轮原始证据均保留。

## 89 项逐项适配方案

本节将89项实证转换为待审定的实施方案。结论是：先做好完整文本、明确的格式转换及通用错误，再补齐真实可用能力；不能靠整体放宽校验获得原生函数、采样、Schema或媒体支持。当前没有实施任何代码、配置或部署变更，也没有新增Google调用。

方案以76d0fb8f为已发布实现参照，前端参照110c204。另一窗口未提交的媒体/用量工作不属于已验收能力，后续实施前必须核对其最新归属与交界。原始72次请求及89项测试状态继续保留，本节不会将提案改写成测试通过。

### 分类与优先级

| 类别 | 项数 | 可以承诺的范围 |
| --- | ---: | --- |
| A 可无损兼容的限定分支 | 8 | 已知等义的文本角色/配置别名、确定的空操作、NONE以及framing和生命周期本地合同。仅指每行明确的子分支，不表示整个Gemini原生请求保真。 |
| B 需要转换或显式降级 | 29 | 文本历史、续写、系统文字、工具历史、搜索/代码、JSON及响应/错误转换。保留可见业务数据，原生角色、解码控制、状态或时序差异必须公开说明。 |
| C 当前合同不支持的分支 | 31 | 当前合同没有合法映射、没有真实来源，或本地完整性/预算有意拒绝。先提供准确处理与诊断，不代表Google整个产品不具备该功能。 |
| D 前置条件不足 | 21 | 媒体、缓存、受控工具、会话以及思考效果/公开元数据等仍有资源、合同或语义门槛。字段被接受不能代替功能验收。 |

这四类是方案分组，与原实证“5项样本验证、33项候选拒绝、1项方法不可用、23项部分观测、21项本地、6项前置不足”不同。D包含实施前尚需效果/生命周期证据的项目；C中的重复键、坏JSON、超深输入及超预算输出属于保留保护，不作为“删掉限制”的任务。A也不是新增8项原生验收通过。

| 优先级 | 项数 | 执行意图 |
| --- | ---: | --- |
| P0 | 27 | 先解决丢内容、假成功、强约束被忽略、错误泄露、历史/续写及取消重放等正确性问题；不等待媒体权限就能完成其中本地部分。 |
| P1 | 34 | 完善已可行的协议转换、别名/null、来源/JSON/元数据及参数能力诊断。 |
| P1 前置 | 5 | 先验证思考效果、公开思考元数据及原生入口，不直接开启功能；门槛通过后再实施。 |
| P2 | 6 | 提示式翻译候选、深度及四项预算的明确需求与扩展；建议首批保留保护默认值。 |
| P2 前置 | 17 | 媒体、缓存、registry/agent、会话等资源和合同未满足前不进入功能实现。 |

每行优先级表示主要工作；同一行可以先完成P0/P1的通用诊断，而其原生能力仍等待门槛。没有必要改动全部89项对应的代码，已正确的路径只保留并补充受影响的验收。

### 需要审定的行为

| 决议 | 建议方案 | 影响与待定项 |
| --- | --- | --- |
| 不支持参数的处理 | 区分严格语义与明确同意的文本兼容；强制函数、硬长度/停止、Schema等不能默认假装生效。 | 严格拒绝会让当前部分200变400；保留忽略又不能承诺约束。默认值、按服务/客户端的作用范围及降级入口须先确定。本节不将另一窗口的既有忽略行为视为新决议。 |
| 降级的可见性 | 公开信息保持通用，只带安全源字段；不将提示塞进模型正文。 | 响应头可能被new-api剥离，服务端日志也不能代替客户端知情；需在实际链路验证诊断/配置约定，再决定可支持的降级范围。 |
| thinkingConfig | 先做效果门槛，再有限映射至已识别字段；沿用中央思考管线。 | budget/LOW已接收，includeThoughts两值均4节点；不能据此保证预算或开关。缺反馈时只能列候选透传，不作为生产能力保证。 |
| JSON输出 | 建议首批保留完整校验后发送，只修正真实来源保留和可验证引用。 | 真增量会产生已发送正文无法回滚的问题；本地Schema校验也不能当原生受约束解码。两项如需要，分别定义合同。 |
| 会话与媒体状态 | 建议默认继续客户端完整历史，状态复用/附件另设隔离与生命周期设计。 | 63个带sessionless标志的请求仍有25个成功响应返回会话引用；持久性未知。assistToken用于反馈/支持，不是续聊或思考签名令牌。 |
| 响应预算 | 建议先保留四项现有保护，改善定位；实际需求成立后再审定有界配置及内存优化。 | 32MiB含等号行为、4MiB文字、事件数和SSE行共同约束，不能只提高一项或改系统策略。 |

以上是具体待审定选择；本轮不修改现有默认行为，也不创建配置开关或调用外部资源。

### 修改范围

下表是可能涉及的最小模块集合，逐项表只引用实际相关模块。实现时以届时明确的提交为基线，先读差分，保留其他窗口改动；不要按本表批量重写文件。

| 模块 | 预期文件范围 | 修改边界 |
| --- | --- | --- |
| 请求 | internal/translator/geminienterprise/request.go、history.go、ignored_functions.go | 源输入、文本历史、已知别名、MIME及工具降级；优先复用76d0fb8f逻辑，不能把不支持字段直接加到上游。 |
| 校验 | internal/translator/geminienterprise/validation.go | 确定JSON解析、字段/类型/别名、最终目标合同。仅为已证实能力增量扩展，不整体撤掉白名单。 |
| 执行 | internal/runtime/executor/gemini_enterprise_executor.go；internal/runtime/executor/helps/ | 最终Payload顺序、普通/SSE、开始/取消/错误、资源调用和支持代码。辅助代码放helps；连接后不增加读写超时。 |
| 思考 | internal/thinking/apply.go、types.go、validate.go、convert.go；internal/thinking/provider/ | 沿用规范表示→模型归一化→provider转换；必要时增加Enterprise applier，不能在translator里另建一套思考管线。 |
| 输出 | internal/translator/geminienterprise/stream.go、response.go、media.go | 真实来源的文本/代码/grounding、framing、JSON和预算；不捏造缺失元数据或概率。 |
| 路由 | sdk/cliproxy/executor/operation.go；sdk/cliproxy/auth/gemini_enterprise_operation.go、conductor_request_scoped_errors.go；sdk/api/handlers/ | 两协议入口、操作与错误作用域；共享handler变更必须回归其他渠道，不凭模型同名串用其他provider。 |
| 诊断 | validation.go；helps/gemini_enterprise.go；sdk/api/handlers/中的共享错误处理 | 通用错误/安全源字段、应用或忽略情况、流内失败；不增加渠道前缀，不透传资源、Token或原文。 |
| 鉴权 | internal/auth/geminienterprise/；现有凭证和资源绑定的调用处 | 仅在媒体/缓存/会话的已授权调用需要时检查操作权限与资源绑定；不重建OAuth、不换主体或自动改IAM。 |
| 媒体 | 与其他窗口的Enterprise媒体实现对齐后确定；现有候选涉及helps/enterprise_media.go、media_request.go、media_response.go及相应handler/SDK操作 | 这些候选尚未纳入本次固定版本实证，不能覆盖、重复实现或提前宣称可用；先评审交界与提交归属。 |
| 用量 | internal/runtime/executor/helps/gemini_enterprise_tokens.go及现有用量输出处 | 保留估算来源和已知数值，未知不填0；不将本地Gemma改称Google原生或账单统计。 |
| 管理前端 | 后端现有/v8/management配置/能力合同；前端src/services/api/geminiEnterprise.ts、src/features/providers/descriptors.ts、src/features/authFiles/components/AuthFileModelsModal.tsx及六语言i18n | 仅在审定新增策略/能力展示后涉及；不新增v0适配，不改OAuth绑定、额度数据或用户配置；UI不展示实现细节。 |

若审定增加管理页策略/能力展示，前后端按现有集成指南协作：仅使用/v8/management、保留未知配置键、六语言文案同步。只有前端实际改变后，才从确定提交构建dist/index.html并核对static/management.html字节一致；本轮不改或构建产物。现有OAuth客户端、主体、资源、密钥、服务器配置及历史记录均不重建。

### 开通门槛

门槛未满足时可以先改诊断和本地保护，但不得宣布对应原生能力已适配。当前Argon另有已证实的试用订阅限制：继续沿用Pro的本地规范化规则，正向生成验收等待资格变化；不能把它改写成字段400或禁用整个账号。

| 门槛 | 满足条件 |
| --- | --- |
| 语义策略 | 确定不支持参数的拒绝/显式降级规则、作用范围和默认值；强制义务不能靠静默忽略变成成功。 |
| 原生入口 | 同一有效资源的合法原生控制成功或取得正式可用的替代合同；当前streamGenerateContent仍404，不能仅修改URL。 |
| 媒体闭环 | 存在真实有效fixture及明确获准的上传/读取/removeContextFile/sessions.delete范围；SERVICE_DISABLED的查询失败不能代替各操作授权。 |
| 媒体模型 | 对应输入/输出模态的有效模型与资格、合法参数和目标接口。当前七个文本模型控制不代替模态验收。 |
| 输出媒体 | 真实媒体输出、MIME/字节、普通或支持的流式合同及两客户端读取链。 |
| 工具资源 | 已存在、受控、与本资源一致且动作范围已获授权的测试registry/agent；不能执行未知外部动作。 |
| 思考效果 | 不仅是字段接收，还需预算/档位/开关的正式语义和可核验效果；只有200时仍标候选透传，不能声称限制落实。 |
| 公开元数据 | 确认Google返回的可公开摘要/签名合同及真实字段来源；不将UI状态思考或assistToken当原生签名。 |
| 会话隔离 | 主体/资源/模型绑定、并发/生命周期/历史重叠规则及持久性已明确；不得依sessionless标志保证不落会话。 |
| 缓存读取 | 同主体可访问的有效缓存引用、正确API合同和过期/隔离规则；不新建或猜不存在ID凑验证。 |
| URL访问证据 | 已获准的只读自控页面、随机独有内容与访问日志，能区分抓取和标题搜索。 |
| 翻译语义样本 | 明确允许提示式翻译降级，并有多语言质量判据；字段被上游拒绝的原始证据保持不变。 |
| 概率来源 | 实际可用的原生token概率及配置合同，缺失不能估造。 |
| 模型特定合同 | 该开关对具体模型有正式意义，不根据名字或措辞猜支持。 |
| 计数来源 | 真实countTokens或可审计usage的合法合同；本地估算不能替代原生计数。 |
| JSON流式策略 | 确认继续完整校验后发送还是另建显式增量合同；建议先保留默认缓冲。 |
| 响应预算策略 | 确有需求后确认默认值、边界含等号及上限；先保留保护与原始历史记录。 |
| new-api联调 | 有真实new-api配置/实例用于后续验收，最终客户端能识别降级/失败；不能只用合成夹具或服务端日志证明。 |

SERVICE_DISABLED只证明权限查询依赖服务未启用，当前上传/清理各操作授权仍未知。后续由有权管理员确认或提供获准的查询路径；不自动启用API、改IAM、升级订阅、创建缓存/agent或删除会话。

### 逐项修改与验收

每行同时列出适配范围和限制。C行中的“原生入口”门槛指将来提供该能力的条件，不阻塞先提供准确的不支持诊断；D行先补条件，B行的转换必须由已审定的语义策略覆盖。

#### HST 历史与内容

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-HST-01` 257、513、514 轮历史；范围：完整可见历史转文字；不是原生角色通道 | B／P0 | 保留全部轮次、文字及顺序，统一两种客户端入口；不重设256条上限，不摘要、截断或补虚构user。 模块：请求、执行、路由。 | 257/513/514轮分别核对规范化业务投影的角色、顺序、UTF8长度及SHA；首中尾标记正确；普通/SSE与两SDK一致。 |
| `GE-HST-02` 规范化上下文超过 1 MiB，并覆盖 2 MiB、4 MiB；范围：长文本字节保真；上游token容量另计 | B／P0 | 保留现有无1MiB输入门槛；Payload最终覆盖后仍不裁剪。补高信息密度长历史验证，不把空白填充成功解释为任意上下文都可用。 模块：请求、校验、执行。 | 1MiB/2MiB/4MiB及边界前后完整投影一致；空白与有效信息样本分开；上游容量错误明确失败，不能只答末轮后记全量通过。 |
| `GE-HST-03` 最后一个非空轮次为 assistant 或 model；范围：末尾assistant/model的文本续写 | B／P0 | 沿用最后非空assistant的续写提示，完整保留已有前缀；重新编写无歧义合成提示，保留本次尾句点严格断言失败。 模块：请求、执行。 | 普通/SSE均不重复前缀、不造user；新增输出与预先明示的精确字面要求一致；含标记与严格相等分开计，不能追加裁剪修正模型正文。 |
| `GE-HST-04` OpenAI 空串、null 或缺省 assistant content；Gemini 空 model parts 数组或仅签名 Part；范围：已定义的空占位；签名状态不保真 | B／P0 | 对空串、null、缺省assistant、空model parts及仅签名占位分别归一化；保留占位与最后非空模式，禁止把所有null都视为空数组。 模块：请求、校验。 | 逐个源变体本地断言并经两SDK普通/SSE；回答最后有效轮次，空占位不删除有效上下文；全空请求错误；无句点精确断言使用无歧义提示。 |
| `GE-HST-05` systemInstruction 与 user 存在不同要求；范围：系统要求作为文字上下文 | B／P1 | 保留系统文字与顺序，明确不提供原生systemInstruction优先级保证；严格依赖原生系统层级的请求暂不承诺支持。 模块：请求、诊断。 | 冲突和非冲突多样本核对系统文字完整；一次选择正确不记层级等价；不得把系统正文搬成可泄露诊断。 门槛：原生入口。 |
| `GE-HST-06` 缺省 role，以及显式 assistant、function、其他非法 role；范围：明确文本role别名；不是任意角色修复 | A／P1 | 仅规范化已知等义的assistant/model文本角色；缺省role遵守已定义规则。function/tool走独立历史转换，未知或矛盾角色拒绝。 模块：请求、校验。 | 各角色及缺省/null/非法类型单测；同义文本的最终业务投影相同；不得把未支持的函数或隐藏内容当普通user静默接受。 |
| `GE-HST-07` 一个 Content 内多个 text Part，含换行、空格、Unicode；范围：可见text拼接保真，Part边界丢失 | B／P1 | 保持现有无额外分隔拼接，不注入空格；记录原Part到文字的投影，避免代码、Unicode及引用偏移出错。 模块：请求、输出。 | 换行、空格、组合字符、emoji及代码原文和顺序逐字节相等；不宣称Part身份、原生多模态边界仍存在。 |
| `GE-HST-08` Content 附加字段或 Part 新增字段；范围：已知可空字段的无操作分支 | A／P1 | 保留已定义inlineData/fileData等已知可选字段为null时的空操作；未知字段不一概删除，实际内容走能力判断，签名按THK-04处理。 模块：请求、校验。 | 已知null不误拒绝且文本相同；同名非null实际内容不能消失；未知业务字段/错误类型报安全字段路径；不扩大为所有元数据均无语义。 |

#### MED 媒体输入

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-MED-01` text 加有效图片 inlineData；范围：图片理解；inlineData直接映射已拒绝 | D／P2（前置） | 先核对其他窗口媒体实现及安全文件闭环，再选择上传文件并引用的转换；不再把inlineData塞入query.parts。 模块：媒体、鉴权、请求、执行。 | 真实有效PNG含独有图形/文字，文本提示不泄露答案；上传、读取、回答及自有附件清理闭环；两协议普通/SSE核对，失败不忽略图片继续答。 门槛：媒体闭环。 |
| `GE-MED-02` text 加有效 fileData 图片 URI；范围：合法远程图片/文件引用 | D／P2（前置） | 确认可用的官方引用合同；如需取回后上传，限定授权来源、无凭证跨域转发，并保留内容/MIME/错误。 模块：媒体、鉴权、请求。 | 已知可读引用与不存在/无权限引用区分；不跟随带凭证跨域重定向；真实读到独有内容，不能用模型猜图或只答提示通过。 门槛：媒体闭环。 |
| `GE-MED-03` 短音频 inlineData 或有效音频文件引用；范围：音频理解 | D／P2（前置） | 取得合法小型MP3或已授权合成音频引用及支持模型后设计输入转换，保留MIME与音频语义。 模块：媒体、请求、执行。 | 音频中独有短语不出现在文字提示；识别正确，静音/坏音频/无权限明确分类；不拿WAV替代MP3作不支持结论。 门槛：媒体闭环、媒体模型。 |
| `GE-MED-04` 短视频及 videoMetadata 的时间段或采样配置；范围：视频理解及时间段 | D／P2（前置） | 先建立有效MP4读取，再验证时间段与采样配置；没有对应合同的选项拒绝，不当作视频生成。 模块：媒体、请求、执行。 | 有效短MP4在不同时间段有不同独有标记；按起止时间读取，忽略选项必须显式；坏文件与不支持选项分开。 门槛：媒体闭环、媒体模型。 |
| `GE-MED-05` 小型 PDF 或文档文件；范围：PDF/文档读取 | D／P2（前置） | 复用已校验三页合成PDF；确认上传与清理操作权限后再读，未确认前不创建附件。 模块：媒体、鉴权、执行。 | 首中尾页独有值全部准确，不能只读第一页；真实文件引用、错误与清理可追溯；不把SERVICE_DISABLED视为操作权限已查明。 门槛：媒体闭环。 |
| `GE-MED-06` Part 级 mediaResolution、videoMetadata 等附加媒体选项；范围：Part媒体精度、时间段等选项 | D／P2（前置） | 依实际媒体类型和目标合同逐个设计映射；不可表达的选项明确拒绝或经同意降级，不改成普通文本。 模块：媒体、请求、校验。 | 先有媒体基线，再仅改变一个选项核对最终请求和可观测效果；合法/非法/缺省分支分别验；文本输入附选项不算媒体验收。 门槛：媒体闭环、媒体模型。 |

#### TLS 工具与函数

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-TLS-01` functionDeclarations 及受目标支持的另一拼写，另测空数组或 null；范围：原生自定义函数声明/调度 | C／P0 | 保留当前不具备原生函数的能力事实；按审定语义策略处理声明，严格模式明确拒绝，允许的文本模式也不能声称声明生效。 模块：请求、校验、诊断。 | 有效声明含名称/说明/嵌套参数Schema；空/null/错误类型分测；声明与执行能力分开，客户端实际执行次数为0时不能记函数通过。 门槛：原生入口、语义策略。 |
| `GE-TLS-02` 历史 model functionCall，含合法参数及调用标识；范围：历史functionCall转标注文字 | B／P0 | 保留名称、id、参数值与顺序；按已知协议字段处理元数据，不整体假定安全，也不递归删除args同名业务键。 模块：请求、诊断。 | 含嵌套对象、数组、Unicode的调用信息投影逐值一致；协议签名与args业务键分别断言；不得生成新的可执行调用或泄露签名。 |
| `GE-TLS-03` user functionResponse，含结构化结果；范围：历史functionResponse转标注文字 | B／P0 | 保留关联id、名称、结构化结果、错误结果与历史顺序；只作为已完成历史，不伪造回调执行。 模块：请求、输出。 | 多调用交错/同名不同id/失败结果均能正确关联；结果不只留伴随文字；两SDK与实际new-api普通/SSE文本回放一致。 |
| `GE-TLS-04` functionCallingConfig.mode 为 ANY；范围：ANY必须调用的原生义务 | C／P0 | 不把ANY改成AUTO或普通回答；在必须执行客户端函数时返回通用不支持错误，改变现有忽略行为须在策略决议中明示。 模块：请求、校验、诊断。 | 无可执行路径时禁止伪tool_calls与成功函数统计；严格拒绝在生成前发生；显式全文本降级也不得计作ANY语义通过。 门槛：原生入口、语义策略。 |
| `GE-TLS-05` functionCallingConfig.mode 为 VALIDATED；范围：VALIDATED约束 | C／P0 | 先确认对应目标对VALIDATED的正式语义，当前不提供原生验证模式；不笼统将其定义成必须调用，也不静默当AUTO。 模块：请求、校验、诊断。 | 目标定义、声明合法性与选择约束分别核对；公共toolConfig拒绝不冒充该模式独立验证；执行前安全报错或显式降级。 门槛：原生入口、语义策略。 |
| `GE-TLS-06` functionCallingConfig.mode 为 AUTO；范围：AUTO的可选工具文本降级 | B／P1 | 仅在调用方接受无客户端函数能力时保留普通文本路径；明确没有原生函数选择功能，不能把未执行的调用说成成功。 模块：请求、诊断。 | AUTO本身不强制调用；严格/允许降级两策略都有可观测结果，actual new-api不能只依赖可能被剥离的响应头知晓降级。 门槛：语义策略、new-api联调。 |
| `GE-TLS-07` NONE、空 functionCallingConfig 对象、null 配置；范围：自定义函数NONE及已定义空配置 | A／P1 | NONE保证不产生客户端函数调用；仅将确定等同缺省的空/null配置归一化。该保证不延伸为禁止全部Google内置搜索/代码。 模块：请求、校验。 | NONE、空对象、null逐层区分；缺mode不随意补ANY；自定义调用/执行计数为0；其他内置工具开关按各自合同处理。 |
| `GE-TLS-08` allowedFunctionNames 及其目标支持的拼写；范围：allowedFunctionNames的调用约束 | C／P0 | 没有函数调度路径时不承诺名称白名单生效；与mode/声明联合校验，拒绝或显式整体降级，不能只删除名称。 模块：请求、校验、诊断。 | 多个候选名、未知名、空数组及缺mode分测；无真实调度不得记候选约束通过；源字段安全定位。 门槛：原生入口、语义策略。 |
| `GE-TLS-09` 空 googleSearch 开关；范围：GoogleSearch空开关映射内置web grounding | B／P1 | 保留空webGroundingSpec与搜索提示，完善真实来源到客户端的映射；不扩展成所有GoogleSearch选项透传。 模块：请求、输出、执行。 | 真实references/segments与正文相符；两SDK及new-api普通/SSE可读，Unicode引用范围正确；没有来源时不编造引用。 门槛：new-api联调。 |
| `GE-TLS-10` googleSearch 内合法非空配置；范围：GoogleSearch非空选项 | C／P1 | 维护具体选项能力表；timeRangeFilter的当前映射被拒绝，不删除后伪称时间约束生效，也不据一个字段否定所有未来选项。 模块：请求、校验、诊断。 | 每个拟支持选项先有合法目标合同，再单独录包和语义对照；被拒绝选项明确标出，不因空开关成功批量通过。 门槛：原生入口、语义策略。 |
| `GE-TLS-11` urlContext 与一个已知内容的测试 URL；范围：URL读取提示的尽力转换 | B／P1 | 保留URL与失败说明；在没有自控页面内容/访问证据前不承诺原生urlContext或成功抓取。需要严格抓取保证时拒绝或使用已验证的其他合同。 模块：请求、输出、诊断。 | 自控只读测试页含随机独有值并有访问日志；成功值、来源、访问失败一致，搜索标题/正文声称已读不算；不暴露测试凭证。 门槛：URL访问证据。 |
| `GE-TLS-12` codeExecution 与可核对计算；范围：提示触发真实内置代码执行 | B／P1 | 保留执行提示与返回的真实代码/执行结果；没有执行事件时不声称运行成功，不假定同名codeExecution配置已透传。 模块：请求、输出、执行。 | 随机输入摘要或计算结果可核对；实际executableCode/codeExecutionResult均存在；文本模拟、仅答案正确与真实执行分开记。 |
| `GE-TLS-13` 本渠道返回的 executableCode、codeExecutionResult 作为 model 历史回传；范围：真实代码事件转文字再回传 | B／P1 | 保留语言、代码、outcome/output和顺序；空对象是否有真实内容单独处理，不能用说明标记冒充代码/结果。 模块：请求、输出。 | 真实执行→返回事件→客户端历史回传闭环；原始代码/结果字节一致，空代码/空结果测试；明确原生事件边界已转为文字。 |
| `GE-TLS-14` 目标 API 新增或其他合法 Tool 类型；范围：其他合法Tool/registry/agent | D／P2（前置） | 取得现有、受控且无未授权外部动作的工具资源及明确合同后设计桥接；不创建registry或执行未知agent凑验收。 模块：请求、执行、鉴权。 | 绑定合法资源、调用/结果配对和访问权限都可验证；工具动作有当前用户授权；失败不退成普通文本后记工具成功。 门槛：工具资源。 |

#### GEN 生成配置

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-GEN-01` temperature；范围：temperature的原生采样控制 | C／P1 | 当前无已验证映射；不以提示词温度替代。仅当具体模型正式合同明确忽略该项时，可另行评估无操作归一化，不能套用到2.5Pro等消费该参数的模型。 模块：请求、校验、诊断。 | 逐模型能力表及最终请求值可查；不以单次输出随机性判断生效；严格/降级策略可见，不能统一删字段后记支持。 门槛：原生入口、语义策略。 |
| `GE-GEN-02` topP；范围：topP的原生采样控制 | C／P1 | 与temperature独立记录和处理；当前路径不透传，不用近似文字要求代替概率控制。 模块：请求、校验、诊断。 | 合法值、缺省、非法类型/范围及具体模型原生忽略约定分开；有合法合同后核对值，不用一次回答推断效果。 门槛：原生入口、语义策略。 |
| `GE-GEN-03` topK；范围：topK的模型特定采样控制 | C／P1 | 先确认模型正式支持topK，再寻找合法合同；当前不把未识别字段当任意Struct透传。 模块：请求、校验、诊断。 | 支持/不支持模型、范围和最终值分开验；参数拒绝与模型不支持分开分类。 门槛：原生入口、语义策略。 |
| `GE-GEN-04` maxOutputTokens；范围：maxOutputTokens的原生硬上限 | C／P0 | 不得静默忽略并让调用方以为成本/长度上限已生效；也不通过截断已生成正文冒充原生停止。 模块：请求、执行、诊断。 | 小上限应有可核验约束及真实停止来源；不支持时生成前明确处理；客户端取消/后处理裁剪不能记原生token预算支持。 门槛：原生入口、语义策略。 |
| `GE-GEN-05` stopSequences；范围：stopSequences的原生停止 | C／P0 | 当前无无损映射；不追加自然语言停止要求或裁剪正文后声明等价。 模块：请求、输出、诊断。 | 合成目标会跨过停止词；普通/SSE停止语义、Unicode跨分片、来源结束原因均可核验；无合同则明确不支持。 门槛：原生入口、语义策略。 |
| `GE-GEN-06` seed；范围：seed控制 | C／P1 | 保留当前不支持事实；不要将伪随机提示、相同答案或本地随机数当上游seed。 模块：请求、校验、诊断。 | 合法整数及最终请求可追溯；相同seed不承诺绝对确定性；无上游映射不得标为已应用。 门槛：原生入口、语义策略。 |
| `GE-GEN-07` candidateCount；范围：candidateCount大于1 | C／P1 | 支持单候选的既有输出，不复制一个答案凑多个candidate；大于1需要真实多候选合同。 模块：请求、输出、诊断。 | 1与大于1分支；真实候选数/index/结束原因独立；请求多候选时不能默认降到1而无明确策略。 门槛：原生入口、语义策略。 |
| `GE-GEN-08` responseMimeType 为 text/plain 或 application/json；范围：text/plain与application/json | B／P1 | 保留文本模式和JSON提示加本地语法校验；明确不是原生受约束解码，复用OUT-06/07的流式规则。 模块：请求、输出、执行。 | 两MIME、普通/SSE、JSON各根值类型分测；JSON不受围栏/旁白破坏；失败不包装为空成功。 |
| `GE-GEN-09` responseMimeType 为 text/x.enum 或其他目标合法值；范围：text/x.enum及其他原生MIME | C／P1 | 当前不以自由文本替代受限枚举；如审定增加本地枚举校验，应作为独立降级合同，不能称原生闭词表解码。 模块：请求、校验、诊断。 | 合法枚举及越界值测试；本轮共享MIME字段拒绝不冒充每个值已执行；类型和目标模型前提明确。 门槛：原生入口、语义策略。 |
| `GE-GEN-10` responseSchema；范围：responseSchema的原生约束 | C／P0 | 当前JSON模式只校验语法；不继续把Schema未落实表现为原生支持。可选本地Schema校验另审，不自动修补答案或重放生成。 模块：请求、输出、诊断。 | 必需字段/类型/枚举/嵌套正反例；合法JSON但违Schema不能通过；本地校验与原生约束解码分别标记。 门槛：原生入口、语义策略。 |
| `GE-GEN-11` responseJsonSchema；范围：responseJsonSchema及其子集 | C／P0 | 不把responseSchema改名或提示词视为实现；如引入本地子集校验，先列支持关键字及拒绝行为。 模块：请求、输出、诊断。 | 逐关键字有效Schema及反例；不支持关键字明确处理，不丢约束后记全Schema兼容。 门槛：原生入口、语义策略。 |
| `GE-GEN-12` responseModalities；范围：responseModalities的真实输出模态 | D／P2（前置） | 先有对应模型、输出合同和客户端能力，再决定媒体操作映射；不能仅移除generationSpec字段或给文本模型加开关。 模块：媒体、请求、输出、路由。 | 目标模态与实际非空内容/MIME一致；图片/音频不能只返回描述文字；普通/SSE及错误统计正确。 门槛：媒体模型、输出媒体。 |
| `GE-GEN-13` speechConfig；范围：speechConfig | D／P2（前置） | 取得合法语音模型/接口后逐项映射声音、语言等设置；文本返回不视为语音成功。 模块：媒体、请求、输出。 | 真实可解码音频、MIME和选项生效证据；无权限/不支持音色/错误参数分开。 门槛：媒体模型、输出媒体。 |
| `GE-GEN-14` imageConfig；范围：imageConfig | D／P2（前置） | 核对并复用其他窗口媒体工作，只有合法图片模型和合同后才映射尺寸/比例；不重复建设。 模块：媒体、请求、输出。 | 实际图片尺寸/比例/数量与请求一致；有效模型、可解码字节、普通及支持的流式形式分别验。 门槛：媒体模型、输出媒体。 |
| `GE-GEN-15` generationConfig.mediaResolution；范围：generationConfig.mediaResolution | D／P2（前置） | 先建立有效媒体输入，再区分生成级与Part级精度合同；不将两者混用。 模块：媒体、请求、校验。 | 同一真实媒体仅改精度值；最终值及可核验效果独立记录，不拿文字样本通过替代。 门槛：媒体闭环、媒体模型。 |
| `GE-GEN-16` responseLogprobs；范围：responseLogprobs | C／P1 | 当前无可用概率来源，不能伪造概率/用文本自述估计；保持明确不支持。 模块：请求、输出、诊断。 | 若未来开通须有真实token及数值来源；缺失不填0、固定值或假日志概率。 门槛：原生入口、概率来源。 |
| `GE-GEN-17` logprobs；范围：logprobs数量等配置 | C／P1 | 与responseLogprobs前提联动，但独立校验值和范围；不因接受整数就宣称概率功能实现。 模块：请求、输出、诊断。 | 真实候选token概率数量/顺序与合法请求相符，错误范围明确；不能以GEN-16共享来源冒充参数生效。 门槛：原生入口、概率来源。 |
| `GE-GEN-18` presencePenalty；范围：presencePenalty | C／P1 | 当前无原生映射，不将“减少重复”等提示词当相同参数。 模块：请求、校验、诊断。 | 具体模型支持、范围、实际发出的值均可核对；不以回答措辞变化单独验收。 门槛：原生入口、语义策略。 |
| `GE-GEN-19` frequencyPenalty；范围：frequencyPenalty | C／P1 | 独立维护能力和诊断，不与presencePenalty合并判支持。 模块：请求、校验、诊断。 | 合法/非法范围、缺省和最终值独立测试；不能静默删除后报已应用。 门槛：原生入口、语义策略。 |
| `GE-GEN-20` responseFormat；范围：responseFormat的确定文本/JSON子集 | B／P1 | 仅把已确认等义的TEXT_PLAIN/APPLICATION_JSON分支映射现有模式；Schema、音频及未知新分支不整体忽略。 模块：请求、校验、输出。 | 目标版本合法结构的正反例；旧MIME与新格式不可冲突覆盖；JSON语法校验不代替Schema，其他分支走明确不支持/前置门槛。 |
| `GE-GEN-21` translationConfig；范围：translationConfig的显式文本翻译降级候选 | B／P2 | 可将目标语言和可见文本构造成明确翻译任务，但须标为提示式转换，不承诺原生翻译控制；未验证前不默认打开。 模块：请求、输出、诊断。 | 目标语言设置完整录包、语种/源内容/代码边界正确；多语种固定样本人工或可靠判据核验；严格原生需求仍拒绝。 门槛：翻译语义样本、语义策略。 |
| `GE-GEN-22` audioTranscriptionConfig；范围：audioTranscriptionConfig | D／P2（前置） | 先有音频输入和支持转录配置的目标，再映射languageCodes/模式；不让纯文本模型承担转录验收。 模块：媒体、请求、输出。 | 真实音频包含独有语句，配置/转录结果相符；语言、逐字模式及非法配置独立验证。 门槛：媒体闭环、媒体模型。 |
| `GE-GEN-23` enableAffectiveDialog；范围：enableAffectiveDialog | D／P2（前置） | 先确认实际支持该功能的模型和会话/模态合同，当前文本合同不承诺开关生效。 模块：请求、媒体、诊断。 | 目标模型正式支持及实际字段均可核对；不能只用语气变化认定生效，不支持模型安全报错。 门槛：媒体模型、模型特定合同。 |
| `GE-GEN-24` enableEnhancedCivicAnswers；范围：enableEnhancedCivicAnswers | C／P1 | 当前不提供这一特定功能开关；不把普通回答质量或提示词指令当同一平台功能。 模块：请求、校验、诊断。 | 先确认目标模型正式合同，再测合法值和有效请求；无合同不静默伪启用。 门槛：原生入口、模型特定合同。 |

#### THK 思考与签名

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-THK-01` thinkingConfig.thinkingBudget；范围：thinkingBudget；Schema已识别 | D／P1（前置） | 保留候选映射至generationSpec.thinkingConfig；取得效果证据后接中央思考归一化及模型合法边界，不能绕开最终Payload。 模块：思考、请求、校验、执行。 | 最终预算值可录包，非法类型/子键被拒绝；需上游可审计反馈或正式合同配合因果验证；延迟/节点数不是token预算证据。 门槛：思考效果。 |
| `GE-THK-02` thinkingConfig.thinkingLevel；范围：thinkingLevel；LOW样本已接收 | D／P1（前置） | 按具体模型已验证档位转换，不把budget/level双写冲突交给上游不确定选择。 模块：思考、请求、校验。 | 中央规范、suffix覆盖及档位/预算关系测试；档位值真实发出且有可核验控制证据；仅200保持候选状态。 门槛：思考效果。 |
| `GE-THK-03` generationConfig.thinkingConfig.includeThoughts；范围：includeThoughts的控制与公开边界 | D／P1（前置） | true/false均有4节点，先澄清状态思考与可公开摘要的合同；保留不回显隐藏思考默认，不以本地过滤冒充上游开关生效。 模块：思考、请求、输出、诊断。 | 同模型同prompt仅改开关，区分上游可公开摘要与内部状态；缺语义差异不标生效；日志/客户端不泄露隐藏正文，不造思考Token。 门槛：思考效果、公开元数据。 |
| `GE-THK-04` 文本附带 thoughtSignature 或 thought_signature，以及 new-api 占位值；范围：Part签名兼容接收与丢弃；不回放原生状态 | B／P0 | 保留字符串/null占位兼容，限定Part层处理；先建立合法嵌套fixture，再处理确认属于协议的元数据，args/response同名业务键保留。 模块：请求、校验、诊断。 | 签名不进提示/公开错误/日志；业务字符串同名键不误删；签名双别名保持严格，不能把assistToken当签名或宣称全部嵌套已过滤。 |
| `GE-THK-05` model 历史 thought:true、仅元数据 Part，以及 user/system 标记为 thought:true；范围：隐藏model历史过滤 | B／P0 | 仅按确认的model历史隐藏规则过滤；user/system标记不能用于任意删除正文；过滤后无有效输入应明确失败。 模块：请求、校验、诊断。 | 隐藏内容不外泄，可见内容逐字节保留；仅元数据/签名/全隐藏组合分测，无伪造user、无空请求成功。 |

#### OPT 顶层选项

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-OPT-01` safetySettings；范围：safetySettings的调用方可配置控制 | C／P0 | 当前不声称设置已应用；不以提示词替代安全阈值，也不改变Google自身平台策略。 模块：请求、校验、诊断。 | 合法设置无映射时明确拒绝或经审定告知无法落实；不能静默报告已启用/关闭指定阈值。 门槛：原生入口、语义策略。 |
| `GE-OPT-02` cachedContent；范围：有效cachedContent读取 | D／P2（前置） | 取得同主体/资源兼容的有效缓存与读取合同；不同Google API的缓存引用不能直接互换。 模块：请求、鉴权、执行。 | 缓存首中尾独有值未随user请求提供但准确取回；失效/越权/不存在区分；无有效引用不创建或猜ID验收。 门槛：缓存读取。 |
| `GE-OPT-03` labels、目标合法的新顶层选项及忽略提示；范围：可选元数据与降级可见性 | B／P0 | 建立已应用/转换/忽略/拒绝的明确分类，只对已证明无操作的已知项默认忽略；通用诊断不带渠道或资源，不能只依赖new-api可能剥离的头。 模块：请求、诊断、管理前端。 | 64字段/4096字节前后及超出提示明确；实际new-api可见性核验；未列入忽略头不等于字段生效，业务内容不能悄然丢失。 门槛：语义策略、new-api联调。 |
| `GE-OPT-04` Enterprise sessionInfo、assistToken 或会话复用；范围：代理对上游session状态的受控复用 | D／P2（前置） | 默认保留完整客户端历史；如引入会话状态，先解决主体/资源/模型隔离、重叠历史及生命周期。assistToken是反馈/支持标识，不当续聊令牌。 模块：执行、鉴权、路由、诊断。 | 同绑定会话回忆、跨绑定拒绝、完整历史不重复注入；并发隔离/过期/失败不重放；sessionless返回非空引用的持久性不作无证保证。 门槛：会话隔离。 |

#### VAL 校验与Payload

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-VAL-01` 同一 JSON 对象出现重复键；范围：含重复键的原始JSON | C／P1 | 保留确定性拒绝，不跟随上游宽容解析去选取首值或末值；被忽略字段内部同样检查。 模块：校验、诊断。 | 原始字节同值/异值重复、嵌套重复均按既定合同报400；单键合法请求不误拒绝，不先经map消除重复后测试。 |
| `GE-VAL-02` 非对象、多个 JSON 值、非法 JSON 或非法 UTF8；范围：无效JSON/非对象/多根值/非法UTF8 | C／P1 | 保留输入完整性保护；不为匹配Google对坏UTF8的宽容行为而替换字节。 模块：校验、诊断。 | 原始非法字节/多根值/截断JSON均生成前失败；正确UTF8及转义字符保真；错误不带正文。 |
| `GE-VAL-03` 超过 64 层 JSON 嵌套；范围：超过本地64层的输入 | C／P2 | 先保留深度保护及明确原因；只有合法目标需要更深结构且资源成本可控时再审定扩展，不直接删限制。 模块：校验、诊断。 | 实际计层口径的边界前后测试，包含工具Schema与未消费字段；无栈崩溃、无摘要或重排绕过；不把未知字段400当深层合法性结果。 |
| `GE-VAL-04` camelCase 与 snake_case 两种别名同时出现；范围：已知等义、类型合法且同值的配置别名 | A／P1 | 在最终Payload之前将白名单配置别名规范为一种拼写；同值可合并，异值拒绝。签名/不透明状态/未知别名不按本规则放宽。 模块：请求、校验。 | 每个别名单写与同值双写的规范业务投影相同；异值/错误类型/受保护状态冲突报错；不让Google自行决定并产生SKIPPED。 |
| `GE-VAL-05` 空或 null 的 contents、parts、tools、generationConfig、toolConfig；范围：已定义的空配置默认分支 | A／P1 | 对已知无业务语义的空/null可选配置作确定归一化；contents/parts的形状与有效文本要求单独处理，不全局删除null。 模块：请求、校验。 | 逐字段列缺省/null/空对象/空数组矩阵；必需数组不能被修复成虚构内容；有效业务投影不变，与HST-04占位规则一致。 |
| `GE-VAL-06` 在最终 Payload 添加 generationConfig、原生 contents 或未知业务字段；范围：最终Payload中的原生contents/generationConfig等未知业务字段 | C／P0 | 保留最终目标合同校验；只在已证实且审定的合同内扩白名单，不允许原生字段绕过转换直接注入。 模块：校验、执行。 | 覆盖/过滤的精确字段可定位；不支持路径生成前失败；thinking候选需独立门槛，不能开放任意嵌套对象。 门槛：语义策略。 |
| `GE-VAL-07` 完整历史转换后使用 Payload 覆盖或过滤；范围：最终Payload单次语义屏障 | A／P0 | 保留内建翻译/注入/清理完成后应用规则一次；后续只读校验与传输封装，不恢复被过滤字段。 模块：执行、校验。 | 普通/SSE/重试或未来其他路径均有顺序断言；覆盖值原样进入请求，过滤必需项明确失败，配置应用次数恰为1。 |
| `GE-VAL-08` StreamAssist 本身存在而本地白名单未开放的字段和模式；范围：query.parts之外的files/agents/模式扩展 | D／P2（前置） | 按能力分支设计，非空query.parts文字已有证据；fileIds、RESEARCH、AGENT等分别取得合同和受控资源，不整体扩开白名单。 模块：请求、校验、执行、媒体。 | 每新增字段独立有效样本/权限/错误/输出验收；不以空fileIds或query.parts成功代替agent；sessionless标志与实际状态分开。 门槛：媒体闭环、工具资源、会话隔离。 |

#### OUT 响应与流式

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-OUT-01` candidate 数量、index、role 和多候选；范围：单candidate/index/role重建 | B／P1 | 保留单候选重建；普通/SSE保持稳定且通用的公开标识，不把资源名当原生响应ID；多候选按GEN-07处理。 模块：输出、执行。 | 单候选内容完整、index=0/role=model一致；流中ID与终态稳定；不复制候选、不虚构上游modelVersion。 |
| `GE-OUT-02` 正常停止、长度停止、安全阻断等结束原因；范围：成功状态到STOP的有限映射 | B／P0 | 将SUCCEEDED映射为适配成功，明确非原生完整finishReason；SKIPPED/FAILED/坏尾不能变STOP，缺来源不造MAX_TOKENS/SAFETY。 模块：输出、执行、诊断。 | SUCCEEDED、SKIPPED、FAILED和缺终态分别验；HTTP200但逻辑跳过不得计生成成功；输出开始后的失败按客户端错误合同结束。 |
| `GE-OUT-03` modelVersion、responseId、promptFeedback、safetyRatings、概率等元数据；范围：有真实来源的响应元数据 | B／P1 | 只映射定义与来源吻合的字段；缺modelVersion/概率/安全评分时省略。OpenAI必需代理ID与原生responseId区分。 模块：输出、诊断、用量。 | 逐字段来源表和缺失分支；不将请求modelId充当已核验内部版本，不把replyId/resource/assistToken直接泄露或等同原生ID。 |
| `GE-OUT-04` thought 输出和 thoughtSignature 回传；范围：可公开思考摘要及原生签名 | D／P1（前置） | 取得可公开摘要/签名的实际合同与来源前，保留隐藏内容过滤，不凭thought:true推出可回放签名。 模块：思考、输出、请求。 | 真实可公开结构及签名闭环可核验；无来源不补造，思考Token不从事件数估出；原始隐藏正文不进客户端或日志。 门槛：公开元数据、思考效果。 |
| `GE-OUT-05` grounding、来源、Unicode 引用区间和混合代码 Part；范围：grounding来源与引用区间转换 | B／P1 | 保留真实来源，按目标协议计算UTF8字节/字符区间；代码渲染/JSON清理后重新核对，不能映射的区间不编造。 模块：输出、执行。 | 中文/emoji/多Part/代码前后范围逐片核对；真实Google样本经两SDK及new-api可读；URL无来源样本不假生成引用。 门槛：new-api联调。 |
| `GE-OUT-06` application/json 的流式首个正文与分片；范围：JSON完整校验后的缓冲流式 | B／P1 | 建议保留默认整段校验；独立保存真实来源数据，文本变换后仅输出可验证引用。真增量JSON另审定，不混作现有功能。 模块：输出、执行。 | 首个正文时间和实际分片被记录；有效JSON终态解析正确，坏JSON不得成功结束；引用数据/偏移在围栏和空白清理后仍可信。 门槛：JSON流式策略。 |
| `GE-OUT-07` JSON 模式返回坏 JSON、Markdown 围栏、非对象 JSON；范围：JSON提示、去围栏与语法/对象根校验 | B／P1 | 保留有限格式清理；Gemini application/json可允许各合法JSON根，OpenAI json_object要求对象；不隐含实现Schema。 模块：输出、诊断。 | 对象/数组/字符串/数字/bool/null、围栏、坏JSON各有正反例；不自动改写业务值、重试生成或包装空对象掩错。 |
| `GE-OUT-08` 上游媒体或未知内容种类；范围：真实媒体和未知输出种类 | D／P2（前置） | 先与媒体工作对齐真实输出合同；当前严格UI-only suggestions可忽略，实际媒体/未知内容不能只留伴随文本后成功。 模块：媒体、输出、执行。 | UI blob与真正图片/音视频明确区分；真实MIME/字节与客户端格式正确；不支持内容路径保留明确失败，不能计文本成功。 门槛：输出媒体、媒体模型。 |
| `GE-OUT-09` JSON 数组、连续 JSON、NDJSON、SSE、粘包、拆包、坏尾及缺少成功终态；范围：已支持framing的完整重组 | A／P0 | 保留数组JSON、合法连续JSON/NDJSON/SSE及分片重组；必须真实成功终态，坏尾与单独DONE不放宽。 模块：输出、执行。 | 粘包/拆包/多行/UTF8跨块/坏尾/终态后垃圾均有断言；正文顺序和字节一致，无重复文本，真实普通/SSE控制均可读。 |
| `GE-OUT-10` 已开始输出后取消、上游错误、断流或后续非法事件；范围：开始、取消、失败与不重放 | A／P0 | 保留Started含上游IN_PROGRESS/隐藏thought的定义；取消释放资源，已开始生成不自动重复，错误不伪造正常STOP。 模块：输出、执行、路由。 | 首正文前/中/后取消及上游错误用确定同步测试；上游调用次数、连接释放、失败统计与SDK终止一致，不加连接后的网络超时。 |

#### LIM 本地输出预算

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-LIM-01` 内部 Text 累积 4 MiB 前后；范围：超过本地Text预算的输出 | C／P2 | 先保留4MiB预算和明确错误；如确有需求再审定有界配置及内存优化，不静默截断。 模块：输出、执行、诊断。 | 4,194,304字节接受，+1失败；UTF8/代码Markdown开销计入；失败不回正常STOP，不要求Google生成巨量内容测本地边界。 门槛：响应预算策略。 |
| `GE-LIM-02` 32768 个 text/code/execution 逻辑事件前后；范围：超过本地可见事件预算 | C／P2 | 先保留32768事件上限；记录与网络分片/grounding/done的口径，不能只提高SSE行预算宣称问题解决。 模块：输出、执行、诊断。 | 32768接受/32769失败；text/code/execution计入，grounding/done不计；同内容不同网络分片结果一致。 门槛：响应预算策略。 |
| `GE-LIM-03` 上游响应累计读取 32 MiB 前后；范围：达到本地32MiB总读取预算 | C／P2 | 当前严格小于32MiB的合同先保留；是否改为含等号或允许配置必须显式审定，同时评估并发内存。 模块：输出、执行、诊断。 | 33,554,431接受，33,554,432及+1失败；有效终态/非法尾分开；与Text/SSE其他预算独立，禁止换framing隐藏超限。 门槛：响应预算策略。 |
| `GE-LIM-04` SSE 单行 16 MiB 前后；范围：超过本地SSE单行预算 | C／P2 | 先保留含换行16MiB行预算；如扩展需与总读取/Text预算联合设计，不照搬其他渠道50MiB。 模块：输出、执行、诊断。 | 含换行16,777,216接受、+1失败；LF/CRLF内容边界分别16,777,215/16,777,214；错误源和单位明确。 门槛：响应预算策略。 |

#### API 操作入口

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-API-01` 原生 countTokens 与 usageMetadata；范围：官方countTokens与原生usage缺失 | C／P1 | 原生countTokens仍明确不支持；已有Gemma usage保留估算来源说明，不改称Google账单值或给未知思考/缓存计数填0。 模块：用量、执行、路由、诊断。 | 操作拒绝与估算展示分开；未知字段缺省，已知输入/输出/总量算术正确；不猜测官方方法，也不把本地估算冒充原生计数。 门槛：原生入口、计数来源。 |
| `GE-API-02` 原生普通/流式操作与相邻 Responses、Claude 或跨协议入口；范围：已声明的Gemini/OpenAI客户端入口 | B／P0 | 明确operation/source与目标适配；不根据同名模型或URL隐式串入Responses/Claude/其他provider，新增入口须独立合同。 模块：路由、执行、请求、输出。 | 两SDK普通/SSE、本地全7模型路由矩阵；不支持操作生成前失败，真实调用核对账号/资源绑定；actual new-api转换链单列。 门槛：new-api联调。 |
| `GE-API-03` Enterprise 官方 streamGenerateContent 作为可能的原生聊天入口；范围：当前资源streamGenerateContent方法 | C／P1（前置） | 保留404实证；需有效合同/可见性/资源入口及最小成功控制后才设计原生桥接，不只换URL或穷举路径。 模块：鉴权、执行、请求、输出。 | 合法官方封装、真实模型和同资源控制200且有有效正文；再做原生字段/函数/普通SSE，不用一次404给所有字段贴产品不支持。 门槛：原生入口。 |

#### ERR 错误与客户端

| 编号与适配范围 | 分类／优先级 | 修改方案与模块 | 验收标准与门槛 |
| --- | --- | --- | --- |
| `GE-ERR-01` 本地字段错误、上游错误和流内错误的公开信息；范围：公开错误的通用安全转换 | B／P0 | 保留可信状态、通用错误码和源字段定位；禁止渠道前缀、资源ID、Token、签名及原文透传，内部明细留受保护诊断。 模块：诊断、校验、输出、路由。 | 本地/上游/流内错误及恶意字段路径断言；正文不含Gemini Enterprise等内部渠道标识或敏感值，仍可定位源字段。 |
| `GE-ERR-02` 本地 400、上游 400、模型权限、429、断流；范围：按来源分类请求、权益、服务与流错误 | B／P0 | 区分未知字段400、Argon订阅400、SERVICE_DISABLED403、方法404和真实429/断流；保留HTTP来源，不将一个模型不可用禁用整个账号。 模块：诊断、执行、路由。 | 复用真实响应脱敏fixture；订阅/服务禁用与IAM未知有不同诊断；429/断流用合成行为验证并标来源，不虚构线上已发生。 |
| `GE-ERR-03` Gemini SDK、OpenAI SDK 和 new-api 对普通及流式错误的解析；范围：两SDK与实际new-api的错误呈现 | B／P0 | 统一共享handler的普通/流式错误合同；通用提示应到达最终客户端，不能只在管理日志中可见。 模块：诊断、路由、输出。 | OpenAI/Google SDK分别识别code/param/失败终态；实际new-api捕获普通和SSE错误、不显示渠道前缀、不吞错计成功；头被剥离场景单测。 门槛：new-api联调。 |

### 建议实施顺序与回归

| 阶段 | 主要范围 | 交付及进入下一阶段的条件 |
| --- | --- | --- |
| 一 先定语义合同 | 不支持参数策略、降级可见性、A/B/C/D能力边界 | 明确强约束处理与默认差异；形成可测试的普通/SSE错误与降级合同，保留现有生产行为直至实施获准。 |
| 二 P0正确性 | HST-01..04、函数历史/强制模式、签名过滤、最终Payload、错误、framing/取消 | 在干净基线实现最小改动；本地确认完整历史、无假调用/假成功、无隐私泄露、无已开始生成重放。与其他窗口媒体工作独立核对再合入。 |
| 三 P1转换和客户端 | 已知别名/null、搜索/代码/引用、JSON、元数据、参数诊断、用量来源 | 两SDK及实际new-api普通/SSE完成；真实能力和降级限制在最终调用链可识别，不把本地成功推广为原生语义。 |
| 四 并行补前置证据 | 思考控制效果、合法原生入口、媒体/缓存/工具/会话 | 只按已获授权条件验证；某一路受阻不阻塞其他模块。没有真实效果或合同则继续标前置不足，不为完成率开启功能。 |
| 五 有需求再扩预算 | 四项预算与累积性能 | 明确边界/默认、有效终态/内存/并发测试通过后再变更，不靠截断、换framing或系统策略绕过。 |

所有阶段验收同时满足以下口径：

1. 输入与Payload：用原始源字节测试重复键/非法UTF8，用规范业务投影比较文字、角色、顺序、结构化历史和引用。所有内建修改在最终Payload前完成，规则恰好应用一次，之后只读校验/封装；不截断或摘要历史。
2. 模型和入口：本地覆盖七模型、两SDK、普通/SSE的操作/错误矩阵；后续已授权的真实验收覆盖六个可用模型。Argon资格未变时复用现有限制证据；资格变化后才补正向矩阵，不能用Pro成功替代。明确区分Google直连、代理SDK和实际new-api。
3. 功能断言：源码/请求透传、上游结构接收、功能语义、最终客户端四层分别记录。拒绝路径验证上游生成调用为0；正向样本核对真实绑定，不凭同名模型或HTTP200。预算/概率/Schema不能靠文本自述、时延或正确一次输出推断。
4. 输出与失败：普通与SSE文字一致，JSON语法/对象根/Schema三类合同不混用；无原始资源/Token/签名/隐藏正文。SKIPPED、FAILED、缺终态、取消和后续错误不能计成功或触发重复生成。
5. 回归范围：修改的Go文件格式化，执行相关包/公共路由/离线双SDK及服务编译；共享handler改动覆盖其他渠道。四项预算用内存或受控假上游测试，时间/取消用可控时钟或确定同步。文档阶段不运行这些代码回归。
6. 环境与发布：Windows策略阻止执行时保留原始错误并标未执行，不改或绕过策略，不用编译替代执行；必要补测仅在获准环境。涉及前端才执行Bun相关验证、生产构建、受影响页面及产物一致性；提交、推送、部署、迁移或权限操作均留到后续明确授权的阶段。

### 方案核对记录

方案逐项对应89个唯一编号，每项均有范围、分类、优先级、模块、动作和验收标准；原实证状态及证据标签保存在本轮adaptation-plan-89.json。方案分组为A 8项、B 29项、C 31项、D 21项；没有将字段接收、方案拟支持或本地保护误记为新真实通过。

本轮文件位于enterprise-native-compatibility-doc-20261024/adaptation-plan-89-20261024：adaptation-plan-89.json为机器可核对台账，adaptation-plan-89.md为本节独立副本，plan-validation.json记录编号、来源、表格及工作区差分检查。原始Google验证目录和Windows历史记录不改写。

## 待审定的修改方向

以下只是基于验证结果的备选方案，均未实施。每项应先明确需求和可接受语义，再确定最小修改范围。

### 函数和原生请求策略

| 方案 | 可提供的行为 | 影响和待确认问题 | 实施前验收条件 |
| --- | --- | --- | --- |
| T0 保留当前文本降级 | 延续 76d0fb8f，合法声明和控制被忽略，历史保留为文字，正常文本请求继续 | 无原生函数调用；强制调用会变成普通回答。需明确客户端是否必须感知降级及提示形式 | 合法声明、强制模式、历史回传和普通/SSE；确认没有假 tool_calls、没有虚构执行，检查实际客户端的提示可见性 |
| T1 显式选择降级或严格拒绝 | 将“允许降级”和“必须保留原生义务”分开；历史文本兼容与新动作能力分别处理 | 默认值、作用范围、是否按声明或 mode 区分、配置还是调用方选择都待确认；当前没有可直接使用的此类开关 | 默认行为、显式选择、required/ANY/指定函数、客户端重试与错误解析；不静默把必须执行的动作说成完成 |
| T2 验证并桥接原生协议 | 以实际可用且已验证的上游合同保留结构化调用与结果，或将必要能力交给已验证的其他渠道 | 当前原生入口基线未成功。需要封包、权限可见性、调用 ID、参数、结果配对及新一轮模型验收；分流还会改变账号、额度或计费来源 | 先让原生控制请求成功，再完成一次真实函数回合、普通/SSE、错误和用量闭环；不能仅换 URL 或把提示词模拟当作原生实现 |

当前有真实证据支撑的是有限文本降级；原生桥接的前置条件仍未满足。对 T1 的默认值和粒度不预先作决定，避免将另一窗口的既有选择误当作本阶段的新实施授权。

历史函数对象内部的元数据还存在验证缺口：当前代码序列化整个函数对象，外层 Part 签名已过滤并不能证明嵌套位置也被过滤。应先确认合法客户端字段形状，区分协议元数据与 args/response 的业务键，再讨论过滤规则；目前仅记为静态风险，不能当作已经实测的泄露，也不能随意删除业务数据。

### 输出和流式策略

| 方案 | 最小方向 | 主要代价和必须验证的行为 |
| --- | --- | --- |
| R0 保持默认保护并明确诊断 | 保留现有限额、成功终态和坏尾校验；必要时完善通用错误说明 | 不增加能力上限；错误不能带渠道、签名或原始内容，失败不能变正常 STOP |
| R1 审定 32 MiB 的包含关系 | 选择保留“小于 32 MiB”，或另行调整为“不超过 32 MiB” | 先确定合同，再决定只改说明还是边界实现；精确阈值前后、有效终态和实际读失败分别验收 |
| R2 有界的服务端预算 | 分开配置文本字节、逻辑事件、总读取与 SSE 行预算，默认值和硬边界待审 | 单独提 SSE 到 50 MiB 仍会先遇到 32 MiB 总读取或 4 MiB Output；需测并发内存、CPU、策略快照和热更新隔离 |
| R3 先优化累积再决定提额 | 减少文本与 Part 的反复追加、重复物化 | 不预先承诺固定性能收益；普通/SSE/JSON、引用、用量、取消及失败结果须一致 |
| R4 单独处理 JSON 引用 | 可选择只保留真实来源，或对可验证区间做重新映射 | 围栏、前后空白、Unicode 和跨 Part 位置可能变化；无法映射的区间不能伪造 |
| R5 明确 JSON 缓冲和增量选择 | 保留完整校验后发送，或另行引入显式增量模式 | 增量已发送内容不能回滚，终态坏 JSON 必须明确失败；new-api 解析要实测，流式发送也不自动消除累积内存 |

任何方向均保留完整输入历史、最终 Payload 的单次语义屏障、已开始生成不自动重放，以及上游建立连接后不新增网络读写超时的约定。真实 new-api 实例、模型权限和仍未验证的能力另列证据，方案确认前不据此实施修改。

### 本阶段证据索引

本轮证据目录中的 `verification-only-20261024/readonly-runtime-result.json`、`requests-76d0fb8f.json` 和 `sdk-76d0fb8f-result.json` 分别记录只读线上状态、现有请求包和现有双 SDK 复核。三个并行报告为 `verification-only-current-request.md`、`verification-only-upstream.md`、`verification-only-response-plan.md`。

既有真实发布证据来自另一任务的 `enterprise-function-ignore-release-20261024`。已核对响应及摘要来源，但没有重写其原始摘要、加密响应或历史策略拦截记录。本阶段没有新增 Google 生成调用，也没有修改项目源文件、测试源、服务器配置、凭证绑定或部署。

## 验证前置条件

开始一项之前，记录以下条件。它们不计入后面的功能适配通过数量。

1. 记录待测提交、工作树状态、运行版本、SDK 和 new-api 版本。固定提交基线与未提交候选实现分别测试，不能把混合工作区结果写到固定版本名下。
2. 每个可用对照渠道先完成最小文本请求；缺少环境或转换链的列标为待验证，不阻断其他渠道。CLI 还要确认 `gemini` 到 `gemini-cli` 的实际翻译来源；没有转换链的请求不能作为字段行为基准。
3. 同名模型可能落入其他 provider。真实 Enterprise 调用应核对选中的账号索引和该账号对应的使用记录，不能只看返回的模型名。
4. 额度查询成功不证明模型生成权限、媒体权限或工具能力。Argon 在既有账号条件下仍按“待模型权限”处理；离线用例不代替其真实生成验收。
5. 每次只改变一个字段或一个明确场景。先使用真实有效的小型媒体、可访问的已知测试文件、有效 Schema 和有效缓存，避免用坏样本判断功能不支持。

## 历史与内容结构

依据：[Enterprise 输入转换][src-ge-request]、[历史 Part 处理][src-ge-history]、[历史回归用例][src-history-tests]、[续写回归用例][src-continuation-tests]。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-HST-01 | 257、513、514 轮历史 | 已取消 256 条限制 | 转发历史条数、角色、顺序及首中尾文字逐项一致，普通和流式均可完成 | 历史通过，待新版本复核 |
| GE-HST-02 | 规范化上下文超过 1 MiB，并覆盖 2 MiB、4 MiB | 已移除历史和最终 query.text 两处 1 MiB 限制，不截断、不摘要 | 比较完整可见历史的字节长度和摘要；不能只检查最后一句，也不把上游真实上下文上限当成本地限制 | 历史通过，待新版本复核 |
| GE-HST-03 | 最后一个非空轮次为 assistant 或 model | 使用续写提示，不追加虚构 user | 普通和流式均返回新增续写；已输出前缀不重复；记录这是文本适配续写，不是原生解码预填充 | 历史通过，待新版本复核 |
| GE-HST-04 | OpenAI 空串、null 或缺省 assistant content；Gemini 空 model parts 数组或仅签名 Part | 接受特定空占位，按最后非空可见轮次确定模式；占位保留 | 覆盖每种占位单独样本；不删除有效历史，不回答错误轮次；model 的 null 或缺省 parts 不可混同空数组 | 历史通过，待新版本复核 |
| GE-HST-05 | systemInstruction 与 user 存在不同要求 | 系统指令作为 JSON 文本上下文嵌入，不走原生 systemInstruction 通道；其 role 不作原生角色校验 | 对照系统要求的执行优先级；明确这种转换的语义边界，不能靠字段名存在或一次正确回答认定原生等价 | 待验证 |
| GE-HST-06 | 缺省 role，以及显式 assistant、function、其他非法 role | 缺省统一作 user；显式仅接受 user/model。AIS 和 AG 有更宽松的推断、修复逻辑 | 分开测试合法缺省与需要兼容修复的输入；记录角色变化，不静默改变对话语义 | 待验证 |
| GE-HST-07 | 一个 Content 内多个 text Part，含换行、空格、Unicode | 直接无额外分隔拼接文本，原生 Part 边界消失 | 文本原文与顺序不丢失；用例明确连接规则，分别核对代码块、相邻文字与引用的边界 | 待验证 |
| GE-HST-08 | Content 附加字段或 Part 新增字段 | Content 只允许 role/parts；Part 使用独立白名单；部分已知 null 字段按缺省忽略 | 区分可丢弃元数据和实际内容。有效文本附带可规范化元数据不得误拒绝，真实功能字段不得静默丢失 | 待验证 |

## 媒体输入

这些条目针对普通 Gemini 原生聊天入口，不能用另一个尚未发布的专用图片或视频入口通过来代替。AIS、CLI、AG 的处理也受模型和文件来源限制。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-MED-01 | text 加有效图片 inlineData | 非空媒体字段被文本历史白名单拒绝 | 同一有效图片在支持它的对照模型可用；Enterprise 应实际接收图片或明确不支持，不可删图后按文字成功 | 待验证 |
| GE-MED-02 | text 加有效 fileData 图片 URI | 非空 fileData 拒绝 | 分开验证上传文件引用、公开测试 URI、各目标可用的存储 URI；不把所有远程来源视为等价 | 待验证 |
| GE-MED-03 | 短音频 inlineData 或有效音频文件引用 | 作为非文本媒体拒绝 | 核对 MIME、有效字节及实际理解结果；音频正文不能被丢弃 | 待验证 |
| GE-MED-04 | 短视频及 videoMetadata 的时间段或采样配置 | 视频及非空附加媒体字段不支持 | 有效视频与区间配置分别验证；区分读取整个视频、指定片段和仅回答文字 | 待验证 |
| GE-MED-05 | 小型 PDF 或文档文件 | 当前文本请求不能直接接收该类媒体 | 使用含可核对标记的有效文件，验证内容确实被读取；不能把文件名当作内容 | 待验证 |
| GE-MED-06 | Part 级 mediaResolution、videoMetadata 等附加媒体选项 | 已知 null 可忽略，非空选项不在文本 Part 合同内 | null、合法非空值、非法类型各自成例；与 generationConfig 级同名字段区分 | 待验证 |

## 工具与函数

依据：[工具归一化][src-ge-tools]、[历史 Part 处理][src-ge-history]、[AIS 函数修复][src-ais-normalize]、[CLI 工具 Schema 清理][src-cli-schemas]、[AG 函数处理][src-ag-normalize]。

函数调用模式的行为验证依赖工具声明和回传已经可用。若请求先因声明被拒绝，只能记录声明失败，不能据此断言某个 mode 已独立验证。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-TLS-01 | functionDeclarations 及受目标支持的另一拼写，另测空数组或 null | 工具键先受白名单检查，空声明也不能假定按缺省处理 | 使用有效声明，保留名称、参数 Schema 和描述；空数组与 null 单独成例；无支持路径时明确拒绝，不可删除声明后记成功 | 待验证 |
| GE-TLS-02 | 历史 model functionCall，含合法参数及调用标识 | 非空 functionCall 拒绝 | 调用内容、参数和标识完整保留，或作明确语义等价映射；不能仅保留伴随文字 | 待验证 |
| GE-TLS-03 | user functionResponse，含结构化结果 | 非空 functionResponse 拒绝 | 至少完成一次调用和结果回传；与 AIS/AG 的名称、角色修复对照，不破坏配对关系 | 待验证 |
| GE-TLS-04 | functionCallingConfig.mode 为 ANY | 本地 400 | 在支持此模式的对照目标上建立有效样本，再验证强制调用语义；不能改成 AUTO 后宣称等价 | 待验证 |
| GE-TLS-05 | functionCallingConfig.mode 为 VALIDATED | 本地 400 | 先确认目标 API 和模型确实支持该模式，再检查约束是否落实 | 待验证 |
| GE-TLS-06 | functionCallingConfig.mode 为 AUTO | 仅接受后忽略 toolConfig | 检查实际工具选择语义及转发结果；HTTP 200 不证明模式生效 | 待验证 |
| GE-TLS-07 | NONE、空 functionCallingConfig 对象、null 配置 | NONE 接受后忽略；空对象缺 mode 会 400；toolConfig:null 按缺省且不列忽略，functionCallingConfig:null 跳过内层检查但整体 toolConfig 仍列忽略 | 各种形态分别验证；NONE 不可和“字段缺省”混记，空对象的兼容处理应有明确约定 | 本地差异已验证，功能语义待验 |
| GE-TLS-08 | allowedFunctionNames 及其目标支持的拼写 | 有 AUTO/NONE 时随整体忽略；仅给名称却缺 mode 时先因模式校验返回 400 | 候选名称约束实际生效；依赖函数声明、调用和相关 mode 的前置结果 | 本地差异已验证，功能语义待验 |
| GE-TLS-09 | 空 googleSearch 开关 | 映射到空 toolsSpec.webGroundingSpec，并加入搜索提示 | 真实上游具备搜索能力，来源及引用可核对；不能仅凭正文说“已搜索”通过 | 待验证 |
| GE-TLS-10 | googleSearch 内合法非空配置 | 配置成员列入忽略清单，不转发其语义 | 每个目标支持的配置分别捕获和验证；不能把空开关成功当作其所有选项成功 | 待验证 |
| GE-TLS-11 | urlContext 与一个已知内容的测试 URL | 转为读取 URL 的提示，并开启 web 开关；无原生 urlContext 字段透传 | 对照测试页中的独有值和访问证据，区分网页读取与仅搜索标题；失败时不能编造已读取 | 待验证 |
| GE-TLS-12 | codeExecution 与可核对计算 | 转为执行环境提示，没有同名原生工具配置映射 | 应观察到真实内置代码、执行结果及可核对答案；文字模拟执行不算通过 | 待验证 |
| GE-TLS-13 | 本渠道返回的 executableCode、codeExecutionResult 作为 model 历史回传 | 接受可见内置事件，转成带标记的代码和结果文本；空对象也可能生成非空说明标记 | 验证请求、响应、再回传闭环；与自定义 functionResponse 区分；空代码或空结果的合法性单独核对，不以说明标记冒充真实事件内容 | 历史离线通过，待真实复核 |
| GE-TLS-14 | 目标 API 新增或其他合法 Tool 类型 | 不属于 search/url/code 白名单即拒绝 | 先确认对应目标支持，再决定无损映射或明确拒绝；不根据字段名猜测上游能力 | 待验证 |

## 生成配置

Enterprise 对 generationConfig 仅消费响应 MIME，其他字段列为忽略。未知字段即使类型不合理也可能因未消费而请求成功，因此“没有报 400”不能证明参数兼容。

AIS 保留的参数范围较广，CLI 有 request.generationConfig 处理路径，AG 另有参数删除和 Schema 清理规则。下表每项都要先确认对应 API 和模型支持该参数；不能用 AG 默认删除的字段作为“原生已生效”对照。

当前 [Gemini 官方参考][official-gemini] 对 Gemini 3.6 Flash 及更新模型明确标注 temperature 和 topP 不再生效。对这些模型，字段被忽略本身不能归因于 Enterprise；应优先用仍支持该参数的模型判断额外差异。该参考也列出 responseFormat 等新配置，模型支持范围和合法取值需要按目标版本确认。

依据：[Enterprise 生成配置处理][src-ge-generation]、[AIS 思考和生成配置处理][src-ais-executor]、[CLI 配置处理][src-cli]、[AG 最终请求处理][src-ag-request]。

| 编号 | 字段或场景 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-GEN-01 | temperature | 忽略 | 捕获参数是否进入实际有效请求；3.6 Flash 及更新模型另有原生忽略约定，先选支持该参数的对照模型；不能用单次输出推断生效 | 本地忽略已确认，原生待验 |
| GE-GEN-02 | topP | 忽略 | 合法值、缺省和非法类型分例；3.6 Flash 及更新模型的原生忽略需单列，记录目标实际支持范围及最终值 | 本地忽略已确认，原生待验 |
| GE-GEN-03 | topK | 忽略 | 先确认模型支持，再验证原生参数或明确不支持 | 本地忽略已确认，原生待验 |
| GE-GEN-04 | maxOutputTokens | 忽略 | 检查限制真正落实及停止原因；AIS 会封顶，AG 内建 Gemini 路径会先删除，均需按实际最终请求比较 | 本地忽略已确认，原生待验 |
| GE-GEN-05 | stopSequences | 忽略 | 使用正文会包含的合成停止词，核对停止词后不继续输出；提示模型自行停止不等于原生硬停止 | 本地忽略已确认，原生待验 |
| GE-GEN-06 | seed | 忽略 | 参数真正转发且目标支持；相同 seed 的输出也不能单独作为确定性保证 | 本地忽略已确认，原生待验 |
| GE-GEN-07 | candidateCount | 忽略，输出固定单 candidate | 按支持多候选的 API/模型验证数量、index、各自结束原因；不适用的流式或模型单独标记 | 本地忽略已确认，原生待验 |
| GE-GEN-08 | responseMimeType 为 text/plain 或 application/json | 文本模式或提示词加 JSON 结果校验 | 分别核对普通、流式、JSON 值类型和客户端解析；只能按实际转换方式记“适配支持” | 本地 MIME 分支已确认，实际约束待验 |
| GE-GEN-09 | responseMimeType 为 text/x.enum 或其他目标合法值 | 除 text/plain 和 application/json 外列为忽略 | 对照目标支持的 MIME，验证输出约束；不能回退自由文本后记原生支持 | 本地 MIME 分支已确认，实际约束待验 |
| GE-GEN-10 | responseSchema | 忽略 | 通过必需字段、类型、枚举及嵌套断言验证 Schema；仅输出合法 JSON 不足以通过 | 本地忽略已确认，原生待验 |
| GE-GEN-11 | responseJsonSchema | 忽略 | 验证目标支持的 Schema 子集；AIS/AG 字段改名后以语义等价为准，不能只比较字段名 | 本地忽略已确认，原生待验 |
| GE-GEN-12 | responseModalities | 忽略 | 输出模态实际符合要求；请求图片或音频却返回文字不可记通过 | 本地忽略已确认，原生待验 |
| GE-GEN-13 | speechConfig | 忽略 | 在目标音频模型支持前提下验证声音及输出类型，不用文本模拟语音 | 本地忽略已确认，原生待验 |
| GE-GEN-14 | imageConfig | 忽略 | 图片模型、尺寸、比例等逐个选项验证；不能用普通文本 Pro 的结果替代图片模型 | 本地忽略已确认，原生待验 |
| GE-GEN-15 | generationConfig.mediaResolution | 忽略 | 与 Part 级选项分开捕获和判断，按目标支持的值验证 | 本地忽略已确认，原生待验 |
| GE-GEN-16 | responseLogprobs | 忽略 | 仅在支持的目标上核对实际概率响应；不能补造数值 | 本地忽略已确认，原生待验 |
| GE-GEN-17 | logprobs | 忽略 | 与 responseLogprobs 前置条件及响应字段一起验证，记录合法范围 | 本地忽略已确认，原生待验 |
| GE-GEN-18 | presencePenalty | 忽略 | 先确认目标支持，再核对实际请求值和边界校验 | 本地忽略已确认，原生待验 |
| GE-GEN-19 | frequencyPenalty | 忽略 | 与 presencePenalty 独立成例，不能因为同属采样配置而合并判定通过 | 本地忽略已确认，原生待验 |
| GE-GEN-20 | responseFormat | 未消费，该字段本轮本地探针确认为忽略 | 核对目标版本的合法模态格式结构和 Schema；不能把旧 responseMimeType 的 JSON 适配当新格式已支持 | 本地忽略已确认，目标格式待验 |
| GE-GEN-21 | translationConfig | 未消费，按未知生成选项忽略 | 先确认模型与 API 支持，再验证目标语言及输出语义；不以提示词翻译冒充该原生选项 | 静态确认，专项待验证 |
| GE-GEN-22 | audioTranscriptionConfig | 未消费，按未知生成选项忽略 | 按目标转录模型验证真实音频输入、转录输出与配置；先满足媒体输入前置条件 | 静态确认，专项待验证 |
| GE-GEN-23 | enableAffectiveDialog | 未消费，按未知生成选项忽略 | 目标支持时验证有效请求中的开关；不能仅凭模型语气变化判断生效 | 静态确认，专项待验证 |
| GE-GEN-24 | enableEnhancedCivicAnswers | 未消费，按未知生成选项忽略 | 先核对目标模型是否提供该可选能力，再判断传递或明确不支持 | 静态确认，专项待验证 |

## 思考与签名

依据：[思考字段在 Enterprise 的处理][src-ge-generation]、[历史签名与隐藏思考][src-ge-history]、[AIS 统一思考处理][src-ais-executor]。字段语义应沿用项目的统一思考表示与 provider 翻译架构，不能另建一套绕开标准流程的含糊映射。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-THK-01 | thinkingConfig.thinkingBudget | generationConfig 内忽略 | 预算值或对应模型控制真正生效，且遵守中央思考归一化和 Payload 最终顺序 | 待验证 |
| GE-THK-02 | thinkingConfig.thinkingLevel | 忽略 | 按模型支持的档位验证；不能将不支持档位与适配器丢字段混为一谈 | 待验证 |
| GE-THK-03 | generationConfig.thinkingConfig.includeThoughts | 随 thinkingConfig 忽略；上游 thought 回复也被文本 parser 跳过 | 明确是否可提供隐藏思考，不能把可见答案当思考输出，也不能补造思考 Token | 待验证 |
| GE-THK-04 | 文本附带 thoughtSignature 或 thought_signature，以及 new-api 占位值 | 已接受合法字符串或 null，值忽略且不进 query.text；两种拼写同时出现仍拒绝 | 多轮普通和流式不再误拒绝；签名值不泄露。接受占位只证明兼容，不证明原生签名状态回放 | 历史通过，待新版本复核 |
| GE-THK-05 | model 历史 thought:true、仅元数据 Part，以及 user/system 标记为 thought:true | 隐藏 model 文本不进入可见上下文；user/system 隐藏标志拒绝；整段无可见文本且无非空系统指令会失败 | 不泄露隐藏文本，不错误删除可见答案；区分签名兼容、隐藏内容过滤及有效请求条件 | 历史离线通过，待完整复核 |

## 顶层选项与上下文引用

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-OPT-01 | safetySettings | 输入归一化忽略 | AIS 有默认及显式安全设置处理，AG 自身也删除该字段；记录各目标实际策略，不能用“他渠道都保留”作判断前提 | 待验证 |
| GE-OPT-02 | cachedContent | 忽略，不读取所引用缓存 | 使用含首中尾独有标记的有效缓存验证；只回答当前 user 文本不算缓存支持 | 待验证 |
| GE-OPT-03 | labels、目标合法的新顶层选项及忽略提示 | 未消费字段通常列入忽略清单；提示头最多 64 个字段和约 4096 字节，new-api 可能不转发它 | 逐字段确认；抓取客户端可见提示及最终请求。提示列表缺某字段不能证明它生效 | 待验证 |
| GE-OPT-04 | Enterprise sessionInfo、assistToken 或会话复用 | 当前不回传复用该内部状态，以客户端完整历史重新构造文字上下文 | GenerateContent 本来可由客户端携带历史；明确区别于 Enterprise 内部会话状态，不把内部 Token 当 thoughtSignature | 待验证 |

## 本地校验与最终 Payload

依据：[JSON 解析和最终白名单][src-ge-validation]、[Payload 应用顺序][src-ge-executor]。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-VAL-01 | 同一 JSON 对象出现重复键 | 本地拒绝；被忽略字段内部也先经过这项检查 | 使用原始 JSON 字节测试，不能先经 map 序列化消除重复键；保留确定性解析要求，不默认改成取首值或末值 | 待验证 |
| GE-VAL-02 | 非对象、多个 JSON 值、非法 JSON 或非法 UTF8 | 本地拒绝 | 错误位置和状态明确；这些是结构完整性保护，不能因其他路径更宽松就算兼容缺陷 | 待验证 |
| GE-VAL-03 | 超过 64 层 JSON 嵌套 | 递归解析时拒绝，未消费选项也不例外 | 边界分别测试，区分 body、工具 Schema 和被忽略选项；记录目标有效请求是否真的需要更深层级 | 待验证 |
| GE-VAL-04 | camelCase 与 snake_case 两种别名同时出现 | 多处 alias 检查拒绝，即使值相同或为 null | 逐字段验证单独拼写与冲突输入；明确合法归一化规则，不能不确定地选一个值 | 待验证 |
| GE-VAL-05 | 空或 null 的 contents、parts、tools、generationConfig、toolConfig | 并非统一处理：数组形状、角色及使用 alias 的位置影响是否拒绝或跳过 | 每个字段和值单独成例；空 model parts 数组和 null 不能混淆；不得只用一项 null 推广到所有选项 | 待验证 |
| GE-VAL-06 | 在最终 Payload 添加 generationConfig、原生 contents 或未知业务字段 | 受 StreamAssist 子集白名单限制，通常本地 400 | 原生入口字段和内部上游 Payload 分层验证；其他渠道可用的 Payload 路径不一定可复用 | 待验证 |
| GE-VAL-07 | 完整历史转换后使用 Payload 覆盖或过滤 | Enterprise 在内建转换后仅应用一次，再只读校验；不会修复被过滤字段 | 覆盖值原样到上游；过滤必需字段后明确失败、不恢复历史。CLI 在 Payload 后另有 Schema 清理，不能照搬为正确顺序 | 历史离线通过，待新版本复核 |
| GE-VAL-08 | StreamAssist 本身存在而本地白名单未开放的字段和模式 | session、fileIds、agentsSpec 等范围及 RESEARCH/AGENT 模式未纳入当前文本合同 | 按官方接口与实际账号分别确认；本地未开放不等于 Google 无此功能，也不能直接塞回最终 Payload 绕过语义设计 | 待验证 |

本地开放的最终 Payload 主要包括：必需的 `query.text`、`generationSpec.modelId`；基础 `toolsSpec.webGroundingSpec`、`vertexAiSearchSpec.disabled/filter`；部分用户元数据、动作开关和会话开关。`answerGenerationMode` 仅允许未指定或 NORMAL；`assistSkippingMode` 仅允许未指定或 REQUEST_ASSIST。完整字段及值以固定提交的 `finalRequestFields` 为准。

## 响应与流式行为

依据：[输出重建和 JSON 校验][src-ge-response]、[上游流解析][src-ge-stream]、[流式执行路径][src-ge-json-stream]、[AG Gemini 响应转换][src-ag-response]。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-OUT-01 | candidate 数量、index、role 和多候选 | 重建单个 index=0、role=model 的 candidate | 与 GE-GEN-07 联动；记录合成字段及实际候选数，不把单候选成功推广为多候选支持 | 待验证 |
| GE-OUT-02 | 正常停止、长度停止、安全阻断等结束原因 | StreamAssist 成功状态统一重建 STOP，不是原生 finishReason 的完整映射 | 保留可获取的真实原因；缺失来源就明确缺失，不能补造。AG 也可能在缺省时合成 STOP，但会保留已有原因 | 待验证 |
| GE-OUT-03 | modelVersion、responseId、promptFeedback、safetyRatings、概率等元数据 | 当前响应重建不能完整提供这些原生字段 | 逐字段区分上游未给、适配器丢失和可等价映射；不能伪造版本、安全评分或来源 | 待验证 |
| GE-OUT-04 | thought 输出和 thoughtSignature 回传 | thought:true 内容跳过，不提供原生签名回放状态 | 验证可见内容、隐藏内容和后续回传边界；过滤思考不应误删答案，不因缺失而填假数据 | 待验证 |
| GE-OUT-05 | grounding、来源、Unicode 引用区间和混合代码 Part | 有来源和区间转换，Gemini 用 UTF8 字节索引，OpenAI 用字符索引；终态附带 grounding | 核对分片、中文、emoji、代码前后的区间和来源；检验最终客户端看到的引用，正文说有引用不算通过 | 历史离线通过，待真实复核 |
| GE-OUT-06 | application/json 的流式首个正文与分片 | input.JSON 时缓存正文，结束并通过 JSON 校验后发送；JSON 清理会清除 grounding/supports/annotations | 记录首个可见正文时间和实际分片，核对引用损失；不能宣称原生逐片 JSON 流式等价 | 本地缓冲及引用清理已验证，真实差异待验 |
| GE-OUT-07 | JSON 模式返回坏 JSON、Markdown 围栏、非对象 JSON | 可去围栏后校验；坏 JSON 失败；OpenAI json_object 还要求顶层对象 | 验证 Gemini application/json 与 OpenAI json_object 的不同契约；失败不可包装成成功空响应 | 待验证 |
| GE-OUT-08 | 上游媒体或未知内容种类 | 实际或未知媒体不能作为当前文本输出表示；严格的 UI-only suggestions 例外可忽略；其他新字段可能未被解析 | 区分已知 UI-only 元数据和真实内容。仅有 text 伴随成功不能掩盖被跳过的新内容；不可笼统称所有 inlineData 都拒绝 | 待验证 |
| GE-OUT-09 | JSON 数组、连续 JSON、NDJSON、SSE、粘包、拆包、坏尾及缺少成功终态 | 支持多种 framing，必须有 SUCCEEDED 成功终态；单独 [DONE] 不够；坏尾或失败终态不伪成功 | 有效样本完整重组，无重复文本；错误样本明确失败。按实际事件确认增量与累计语义；这是完整性校验，不默认算缺陷 | 历史离线通过，待专项复核 |
| GE-OUT-10 | 已开始输出后取消、上游错误、断流或后续非法事件 | 输出开始状态参与错误处理；Started 也包含上游 IN_PROGRESS 或隐藏 thought，即使客户端尚无正文；尾部错误不能变成正常 STOP | 取消正确释放资源；客户端识别失败；不自动重复已开始的生成，不把失败记成功用量 | 历史离线覆盖，待专项复核 |

UI-only 例外以[固定源码分类器][src-ge-media-classifier]为准，例如严格形状的 `application/json+suggestions`；这不代表输入端或输出端可以通用接收任意 inlineData。

## 响应大小与事件边界

以下均为**输出或上游响应读取限制**，不是已经移除的历史输入 1 MiB 限制。普通、流式和 JSON 缓冲路径应分别覆盖。

| 编号 | 边界 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-LIM-01 | 内部 Text 累积 4 MiB 前后 | 4,194,304 字节可接受，再加一字节拒绝；代码和执行结果 Markdown 开销也计入 | 精确记录字节、内容种类和最先失败阶段；不静默截断，不把字节数当字符数 | 本轮离线边界已确认 |
| GE-LIM-02 | 32768 个 text/code/execution 逻辑事件前后 | 32768 个可接受，第 32769 个拒绝；不是网络分片数，grounding/done 不计入 | 控制逻辑事件与网络分片两个独立变量，覆盖阈值及元数据穿插 | 本轮离线边界已确认 |
| GE-LIM-03 | 上游响应累计读取 32 MiB 前后 | 33,554,431 字节可接受，达到 33,554,432 字节即拒绝；不是“仅超过 32 MiB 才拒绝” | 使用合法终态和有效 framing；区分读取上限与解析坏尾；保留最初错误，不靠换 framing 隐藏限制 | 本轮离线边界已确认 |
| GE-LIM-04 | SSE 单行 16 MiB 前后 | 含换行总长 16,777,216 字节可接受，再加一字节失败；LF/CRLF 对应内容长度分别为 16,777,215/16,777,214 字节 | 行长度包含 SSE 和 JSON 包装；其他三执行器常用 50 MiB 扫描器，不能只比较 text 长度 | 本轮离线边界已确认 |

这些限制具备内存和流完整性保护作用。是否调整，应结合合法上游响应、内存成本及可配置策略判断，不以删除校验作为适配完成标准。

## API 操作与上游入口

依据：[操作门禁][src-ge-operation]、[Enterprise CountTokens][src-ge-count]、[本地用量计数][src-ge-usage]、[AIS CountTokens][src-ais-count]、[CLI CountTokens][src-cli-count]、[AG CountTokens][src-ag-count]。

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-API-01 | 原生 countTokens 与 usageMetadata | countTokens 不支持；usageMetadata 由本地 Gemma 分词估算，只填已知输入、输出和总量，缺失思考/缓存不填 0 | 记录方法是否实际调用、计数来源及缺失项；不能把本地估算当原生或账单值 | 待验证 |
| GE-API-02 | 原生普通/流式操作与相邻 Responses、Claude 或跨协议入口 | 仅显式原生 OpenAI Chat 或 Gemini GenerateContent 操作且 source=response 可进入 | 分开核对路由选择和模型能力；旁路不支持不等于模型原生文本生成不支持 | 待验证 |
| GE-API-03 | Enterprise 官方 streamGenerateContent 作为可能的原生聊天入口 | 基线没有走该入口；官方外层公开为 model 加不透明 httpBody | 先验证外层封装、实际内部 body、模型及账号权限，再逐项判断是否保留原生语义；不能只改 URL 就认定全兼容 | 待验证 |

Google 的 [Enterprise streamGenerateContent][official-ge-generate] 说明其是聊天入口并公开 `model`、`httpBody` 外层；当前代码用的是另一个 [streamAssist 合同][official-ge-assist]。这些文档不能单独证明不透明内部 body 对全部 Gemini 原生字段的支持程度。原生字段以对应版本的 [Gemini GenerateContent 参考][official-gemini] 及目标模型能力为准。

## 错误与客户端可见结果

| 编号 | 项目与最小变体 | Enterprise 基线行为 | 验证判据 | 状态 |
| --- | --- | --- | --- | --- |
| GE-ERR-01 | 本地字段错误、上游错误和流内错误的公开信息 | 只有来源可信的安全诊断可公开，字段路径经过清理 | 错误正文不含 Gemini Enterprise 前缀、内部渠道标识、Token、签名值和实际正文；仍保留可定位的通用错误码与安全字段路径 | 历史覆盖，待随改动复核 |
| GE-ERR-02 | 本地 400、上游 400、模型权限、429、断流 | 按来源和明确证据分类；400 本身不能证明模型无权限 | 精确记录发生阶段、HTTP 状态、公开 code/param；额度成功不能掩盖请求格式失败，也不能统一写 unknown_error | 待验证 |
| GE-ERR-03 | Gemini SDK、OpenAI SDK 和 new-api 对普通及流式错误的解析 | 公共错误还经过共享 handler，部分 envelope 行为并非 Enterprise 独有 | 三种客户端分别检查错误识别、流终止和失败统计；不能误把项目共同封装列为仅此渠道的限制 | 待验证 |

## 最小样本与逐项执行方法

### 文本控制样本

原生普通入口使用 `/v1beta/models/<model>:generateContent`，流式使用 `/v1beta/models/<model>:streamGenerateContent?alt=sse`。模型名通过路径确定；Enterprise 归一化取执行器传入的 `req.Model`，不会把 body.model 的值当作有效模型选择依据。

```json
{
  "contents": [
    {
      "role": "user",
      "parts": [{"text": "Reply with exactly BASELINE_OK"}]
    }
  ]
}
```

测试提示中的句点、引号、空白也属于样本。机器精确断言必须与实际提示一致；保留原始结果，再记录可接受的规范化规则，不能无记录地改判。

### 函数声明样本

将以下对象的 tools 作为单项增量加到文本控制样本，不同时加入强制 mode。声明成功后再分别执行调用历史、结果历史和 mode 验证。

```json
{
  "tools": [
    {
      "functionDeclarations": [
        {
          "name": "lookup_test_value",
          "description": "Return a synthetic test value for a key",
          "parameters": {
            "type": "OBJECT",
            "properties": {"key": {"type": "STRING"}},
            "required": ["key"]
          }
        }
      ]
    }
  ]
}
```

这是形状有效的合成声明，不证明当前 Enterprise 支持函数。对照 API 若要求改名或 Schema 转换，应按其合法形式执行并记录转换。

### 执行顺序

1. **固定代码与控制请求**：完成前置条件，记录当前版本；现有用户配置、凭证绑定、用量历史及其他窗口工作保持原状。
2. **离线输入和最终请求**：加入单项变体，捕获归一化、Payload 规则之后的实际请求，检查字段、角色、正文、文件引用和忽略清单。区分未进入上游、本地丢弃和上游拒绝。
3. **离线响应和客户端**：用合法上游夹具验证增量、终态、引用、JSON、错误和边界。夹具通过只证明本地处理，不证明 Google 能生成该响应或赋予相应模型权限。
4. **真实上游**：在目标模型能力与账号权限满足时，执行受控样本，核对实际选中 provider 和账号；普通与流式分别记录。使用合成文本和已知测试媒体，报告不保存真实凭证或签名值。
5. **new-api 完整链路**：先记录 new-api 转换后的字段形状，再检查代理实际请求、返回分片及下游错误。直接 SDK 的兼容形状测试不能替代对实际 new-api 实例的验收。
6. **记录结论再适配**：原生支持、适配支持、忽略、明确不支持分别登记；有实现改动时先补语义回归，再做必要的真实复核，不只添加“不会报错”的断言。

温度、采样概率和 seed 不能靠单次生成推断是否生效；优先检查真实有效请求，再用适合该参数的受控统计或上游证据验证。工具、Schema、缓存和模态必须检查其功能结果，不能仅检查 HTTP 状态。

### 首批处理队列

- [ ] GE-HST-08 与 GE-VAL-05：新增元数据、空对象及 null 的兼容策略。
- [ ] GE-TLS-01：工具声明能否无损进入有效上游请求。
- [ ] GE-TLS-02 与 GE-TLS-03：调用和结果回传闭环。
- [ ] GE-GEN-04 与 GE-GEN-05：输出长度和停止词是否真正生效。
- [ ] GE-GEN-10 与 GE-GEN-11：结构化输出是否遵守 Schema。
- [ ] GE-THK-01 与 GE-THK-02：统一思考配置能否正确落实。
- [ ] GE-OPT-02：引用缓存是否真的被加载。
- [ ] GE-MED-01：普通 Gemini 入口的有效图片输入。
- [ ] GE-LIM-01 至 GE-LIM-04：响应边界与完整性处理。
- [ ] GE-API-03：评估原生聊天入口的实际可用性和迁移条件。

依赖项先完成。若某能力需要更换上游请求合同，先记录该前提，不能用静默删除功能字段来完成“兼容”。

## 模型与入口覆盖矩阵

这些模型在基线共用 normalizer 和输出构建器，但实际能力、权限和工具可用性可能不同。共用代码通过不能替代逐模型真实验收。

| 模型 | 普通文本 | 流式文本 | 签名历史与续写 | 工具和 Schema | 媒体 | 真实上游状态 |
| --- | --- | --- | --- | --- | --- | --- |
| gemini-3.8-flash | 离线基线覆盖 | 离线基线覆盖 | 离线覆盖 | 按编号待验 | 按模型能力待验 | 历史其他版本记录单列；本清单新增能力待验 |
| gemini-3.7-flash | 离线基线覆盖 | 离线基线覆盖 | 离线覆盖 | 按编号待验 | 按模型能力待验 | 同上 |
| gemini-3.6-flash | 离线基线覆盖 | 离线基线覆盖 | 离线覆盖 | 按编号待验 | 按模型能力待验 | 同上 |
| gemini-3.5-flash | 离线基线覆盖 | 离线基线覆盖 | 离线覆盖 | 按编号待验 | 按模型能力待验 | 同上 |
| gemini-3.1-pro-preview | 离线基线覆盖 | 离线基线覆盖 | 有历史真实长会话及续写证据 | 按编号待验 | 按模型能力待验 | 0d13b48e 历史 OpenAI SDK 三次真实请求成功 |
| gemini-2.5-pro | 离线基线覆盖 | 离线基线覆盖 | 有历史真实长会话及续写证据 | 按编号待验 | 按模型能力待验 | 0d13b48e 历史 Google SDK 三次真实请求成功 |
| gemini-4-argon | 离线基线覆盖 | 离线基线覆盖 | 离线覆盖 | 共用规则，真实按权限待验 | 按模型能力与权限待验 | 待模型权限；不发起无权限真实生成来冒充字段验收 |

每个适用项至少区分：直接原生普通、直接原生流式、真实 new-api 转换普通、真实 new-api 转换流式。只在 OpenAI 入口验证过的结果不能自动覆盖 Gemini 原生入口，反之亦然。

## 单项结果记录模板

先维护该编号的证据记录，再更新上表状态。保留原有失败、首次错误和后续复核，不能用新通过覆盖旧记录。

```yaml
case_id: GE-GEN-04
tested_at: "YYYY-MM-DDTHH:MM:SS+08:00"
production_commit: "full SHA"
worktree_state: "clean or production baseline plus diagnostic test sources"
client: "native HTTP, Google GenAI, OpenAI SDK, or actual new-api"
client_version: "version"
model: "model ID"
provider_verified: false
account_reference: "local non-secret reference"
stream: false
request_sha256: "hash of the synthetic source request"
visible_history_bytes: 0
history_entries: 0
effective_request_capture: "local redacted evidence reference"
field_observation: "preserved, normalized, ignored, or rejected"
first_failure_stage: "input, final payload, upstream, response, or client"
upstream_generation_requests: 0
http_status: 0
public_error_code: "if any"
public_error_param: "safe field path if any"
semantic_assertions: []
result: "待验证"
limitations: []
source_test_hash: "required for added diagnostic tests"
evidence: []
followup_commit: "if adapted later"
```

请求 SHA、实际历史内容摘要、归一化后的业务投影摘要分别记录，不能因为包装或 SDK 序列化不同而要求整个 JSON 字节完全相同。实际可见文字、角色、顺序和真实内容引用必须满足各项判据。

## 既有回归与历史结果

### 固定提交已有用例

下列测试是既有覆盖入口，不代表清单内全部参数和边界已验证。

| 覆盖范围 | 既有测试 |
| --- | --- |
| 续写与占位 | TestEnterpriseContinuationKeepsCompleteContext、TestEnterpriseEmptyPlaceholdersAndSystemOnly |
| 完整大历史 | TestEnterpriseMultiMiBHistoryIsNotTruncated、TestEnterpriseLongTextHistory |
| 签名、隐藏思考和代码历史 | TestGeminiHistoryMetadata、TestGeminiHistoryRejectsLossyContent、TestGeminiCodeHistoryRoundTrip |
| 公共入口大历史和占位 | TestEnterpriseMultiMiBContinuationPublicRoutes、TestEnterpriseContinuationPlaceholdersPublicRoutes |
| 最终 Payload 与 JSON 续写 | TestEnterpriseNewAPIHistoryPayloadRemainsFinal、TestEnterpriseLargeConfiguredTextRemainsFinal、TestEnterpriseJSONContinuationPublicRoutes |
| 原生普通、流式及安全错误 | TestEnterprisePublicRoutesBothProtocolsAndOperations、TestEnterprisePublicUpstreamErrorsAreChannelNeutralAllModels |
| 实际 SDK 离线协议 | test/gemini_enterprise_sdk_smoke.py，通过 opt-in 本地 Go 假上游运行 |

### 2026 年 10 月 24 日历史真实验收

固定提交 0d13b48e 的历史发布记录保留如下：

- OpenAI 3.36.1 和 Google GenAI 2.32.0 共六次真实请求均返回 200，并核对 Enterprise 账号使用记录。
- 其中四次为普通及流式长会话：514 轮、可见历史 1,141,908 字节，首中尾标记正确，续写未重复前缀。其余两次是空占位请求。
- 两个空占位响应保留了测试提示末尾的句点，首轮“无句点精确相等”断言未通过；保存响应复核后，确认符合原提示。原始断言、错误和复核记录均保留，没有为复核增加模型调用。
- 全部七个模型的十四项公开错误检查通过；Argon 未进行真实生成。
- 当轮全量 Go 回归 91 个测试包通过、18 个被 Windows Application Control 阻止启动，不能记为全量通过。相关历史记录与之前各轮拦截记录继续保留。

历史证据目录名为 `enterprise-long-history-continuation-20261024`，其中 `online-result.json` 是首轮严格结果，`online-reviewed-result.json` 是保留首轮信息后的复核结果，`FINAL_REPORT.json` 汇总发布、配置及历史保留情况。完整历史说明另见 [Gemini Enterprise 中文说明](../GEMINI_ENTERPRISE_CN.md)。

## 0d13b48e 基线并发核对记录

清单基线整理阶段固定同一 0d13b48e 生产源码提交，在独立干净工作区并行进行请求、响应和其他渠道审计。仅运行本机合成测试，不增加 Google 真实生成调用。其他窗口的未提交实现未混入该基线；后续 76d0fb8f 只读复核已在前文单列。

| 项目 | 本轮结果 | 能证明的范围 |
| --- | --- | --- |
| Enterprise translator 既有回归 | 通过；30 个顶层测试、246 个含子用例通过事件 | 现有请求、Parser、Output 和相关语义断言；不等于全部原生字段支持 |
| AIS 与 Antigravity translator 既有回归 | 两包通过；共 55 个既有 Test 函数 | 对照转换、签名、Schema、函数历史及响应处理的现有断言；不等于两个 executor 全部线上验收 |
| 公共 test 包 | 通过 | 现有跨模块合成入口及七模型矩阵，不含真实 Google 和实际 new-api 实例 |
| 双 SDK 离线验收 | 16 项通过，OpenAI 3.36.1、Google GenAI 2.32.0 | SDK 解析、流式、JSON、签名与长历史合成闭环 |
| 服务构建 | 通过 | 只证明编译，不证明系统策略允许启动每个测试二进制 |
| 请求专项诊断 | 70 个叶子用例通过 | 实测 19 个生成配置字段的本地忽略、三种 MIME 行为、工具配置形态、模型来源及元数据等；不证明参数在 Google 生效 |
| 响应边界专项诊断 | 5 组、17 个叶子检查通过，16 条观测 | 四项阈值、字节和事件口径，以及 JSON 清理引用的实际行为；不代替 Google 模型能力验证 |
| 对照渠道专项诊断 | 84 个叶子用例执行通过，84 条观测 | AIS/AG 各 42 项；只确认 converter 阶段变化，executor 的后续删改和线上能力分别判断 |

本轮证据目录名为 `enterprise-native-compatibility-doc-20261024`。诊断测试若附加到固定生产源码上运行，必须保留附加测试源及 SHA，并与干净提交的既有回归区分。后续填写每个编号时引用该编号的实际断言，不能将整包通过批量标记为全部适配成功。

本轮共执行 171 个叶子诊断用例，均完成预期的本地行为核验；它们与 89 个功能编号不是一一对应。对照探针在每个 converter 中观察到 38 项原值保留、2 项等价改名、1 项补齐和 1 项修正；其中包含兼容扩展样本及仅验证形状的媒体参数，不能推断相应模型真实支持。请求探针的 19 个配置字段包括 thinkingConfig 和 responseFormat，MIME 另有三种变体，也不能机械对应为 GEN 编号。GE-GEN-21 至 GE-GEN-24 仍是静态确认，未混入这 171 项执行统计。

本轮选定测试未发生 Windows 策略拦截，不覆盖或撤销历史全量的 18 个受阻包记录。新增诊断文件只放在专用测试工作区，源码副本及摘要已保存，并按路径和 SHA 核验后删除；生产源码和既有测试没有改动。

| 证据 | 内容 |
| --- | --- |
| request-audit.json、request-tests.json | 请求静态审计与既有包回归 |
| request-probes.json、request-probe-source.go.txt | 70 个新增本地探针、源 SHA、首次日志摘要 |
| provider-comparison.json、provider-tests-result.json | AIS、CLI、AG 对照与既有两包回归 |
| provider-probes.result.json、provider-probe-source.go.txt | 84 个 converter 探针及阶段边界 |
| response-audit.json、response-sdk-result.json、response-public-tests-result.json | 响应审计、离线双 SDK 和公共 test 包 |
| response-probes-result.json、response-probe-source.go.txt | 四项响应阈值及 JSON 引用清理探针 |
| build-result.json | 固定提交的服务编译结果 |

## 固定版本代码与官方参考

| 范围 | 参考 |
| --- | --- |
| Enterprise 请求与历史 | [请求归一化][src-ge-request]、[工具处理][src-ge-tools]、[生成配置][src-ge-generation]、[历史 Part][src-ge-history] |
| Enterprise 校验与执行 | [JSON 与最终白名单][src-ge-validation]、[Payload 与上游路径][src-ge-executor]、[操作门禁][src-ge-operation] |
| Enterprise 响应与用量 | [Output][src-ge-response]、[Parser][src-ge-stream]、[JSON 流式][src-ge-json-stream]、[本地 Token 统计][src-ge-usage]、[忽略参数提示][src-ge-ignored] |
| AIS | [Gemini 归一化][src-ais-normalize]、[Gemini 执行器][src-ais-executor]、[边界角色修复][src-boundary-user] |
| Gemini CLI | [执行器][src-cli]、[Schema 清理][src-cli-schemas]、[注册表回退][src-registry]、[内建注册导入][src-translator-init] |
| Antigravity | [Gemini 输入转换][src-ag-normalize]、[最终请求][src-ag-request]、[包装与字段删除][src-ag-envelope]、[Gemini 响应转换][src-ag-response] |
| 既有回归 | [历史用例][src-history-tests]、[续写用例][src-continuation-tests]、[公共 new-api 形状用例][src-public-tests]、[SDK 验收脚本][src-sdk-smoke] |
| 官方合同 | [Gemini GenerateContent][official-gemini]、[Enterprise StreamAssist][official-ge-assist]、[Enterprise StreamGenerateContent][official-ge-generate] |

[src-ge-request]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/request.go#L253
[src-ge-tools]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/request.go#L129
[src-ge-generation]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/request.go#L301
[src-ge-history]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/history.go#L30
[src-ge-validation]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/validation.go#L63
[src-ge-executor]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_enterprise_executor.go#L109
[src-ge-json-stream]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_enterprise_executor.go#L294
[src-ge-count]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_enterprise_executor.go#L344
[src-ge-operation]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/sdk/cliproxy/executor/operation.go#L7
[src-ge-response]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/response.go#L36
[src-ge-stream]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/stream.go#L42
[src-ge-media-classifier]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/media.go#L18
[src-ge-usage]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/helps/gemini_enterprise_tokens.go#L62
[src-ge-ignored]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/helps/gemini_enterprise.go#L25
[src-ais-normalize]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/gemini/gemini/gemini_gemini_request.go#L19
[src-ais-executor]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_executor.go#L174
[src-ais-count]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_executor.go#L767
[src-boundary-user]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/helps/gemini_content_turns.go#L14
[src-cli]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_cli_executor.go#L124
[src-cli-schemas]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_cli_executor.go#L873
[src-cli-count]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/gemini_cli_executor.go#L499
[src-registry]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/sdk/translator/registry.go#L105
[src-translator-init]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/init.go#L1
[src-ag-normalize]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/antigravity/gemini/antigravity_gemini_request.go#L39
[src-ag-request]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/antigravity_executor_request.go#L32
[src-ag-envelope]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/antigravity_executor_request.go#L461
[src-ag-response]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/antigravity/gemini/antigravity_gemini_response.go#L19
[src-ag-count]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/runtime/executor/antigravity_executor_tokens.go#L22
[src-history-tests]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/history_test.go#L12
[src-continuation-tests]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/internal/translator/geminienterprise/continuation_test.go#L28
[src-public-tests]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/test/gemini_enterprise_newapi_test.go#L22
[src-sdk-smoke]: https://github.com/lywx215/CLIProxyAPI/blob/0d13b48ece3ed69fa138747f7ae02b1a36d41302/test/gemini_enterprise_sdk_smoke.py#L1
[official-gemini]: https://ai.google.dev/api/generate-content
[official-ge-assist]: https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist
[official-ge-generate]: https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamGenerateContent?hl=zh-cn
[official-ge-generation-spec]: https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist#generationspec
[official-ge-old-generation]: https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/AssistGenerationConfig
[official-ge-tools]: https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist#toolsspec
[src76-functions]: https://github.com/lywx215/CLIProxyAPI/blob/76d0fb8f73f3fc28b4d7f0a2c729787a64362a6c/internal/translator/geminienterprise/ignored_functions.go#L1
[src76-request]: https://github.com/lywx215/CLIProxyAPI/blob/76d0fb8f73f3fc28b4d7f0a2c729787a64362a6c/internal/translator/geminienterprise/request.go#L1
[src76-functions-tests]: https://github.com/lywx215/CLIProxyAPI/blob/76d0fb8f73f3fc28b4d7f0a2c729787a64362a6c/internal/translator/geminienterprise/ignored_functions_test.go#L1
[src76-sdk]: https://github.com/lywx215/CLIProxyAPI/blob/76d0fb8f73f3fc28b4d7f0a2c729787a64362a6c/test/gemini_enterprise_sdk_smoke.py#L1

## 2026-10-24 思考配置增量复核

前述 THK、OUT 表格保留各历史阶段的观察边界；当前工作区新增的有限适配见 [Enterprise 思考配置增量验证](../GEMINI_ENTERPRISE_CN.md#enterprise-思考配置增量验证2026-10-24)。本轮直接访问 Google，针对 `gemini-3.1-pro-preview` 比较 HIGH/LOW 与 includeThoughts true/false，并以非法枚举、非法布尔类型作 Schema 负对照。

- HIGH/LOW 和 includeThoughts 的结构确被识别；档位的内部推理深度和固定延迟差没有可审计反馈，不能由 200、延迟或文本长度单独声称已证明。
- false 样本仍有短 thought 状态片段；true 样本可额外返回大段思考内容。旧阶段“双方均有 thought 节点”的观察不能推导模型思考返回开关必然无效，也不能反过来把所有状态片段当作公开思考摘要。
- 当前仅为验证模型接入 LOW/HIGH 和显式思考返回，统一思考管线处理后才执行最终 payload 规则。其他模型、预算、MEDIUM 和原生签名回放仍不泛化宣称支持。
- 默认与显式 false 继续过滤思考；true 可返回思考 parts 或 reasoning_content。可见答案计数、JSON、引用偏移及未知思考/缓存用量保持独立。

本轮原始思考正文和 OAuth/会话材料不进入文档或明文诊断。脱敏对照、答案核验、测试结果与 Windows 执行环境限制分别记录，不把模型答错归因于 API 不识别字段，也不把环境拦截当作测试断言失败。


## 2026-10-25 Flash 思考配置增量复核

当前范围扩展为 `gemini-3.1-pro-preview` 与 `gemini-3.8-flash` 的 LOW/HIGH 接收和显式思考返回，详见 [Flash 思考配置增量验证](../GEMINI_ENTERPRISE_CN.md#flash-思考配置增量验证2026-10-25)。本轮使用新项目直连 Google，四组合两轮共 8 次成功，两个 Schema 负对照均被拒绝。此前项目的 429 不计入本轮结果。

参数被接收与内部推理深度生效分别表述。Google 未反馈实际档位或思考用量，本轮 true/false 也未形成可承诺的固定返回量差异；公开 StreamAssist Schema 尚未列出这些字段。不能把其他 Google API 的模型文档当作本渠道合同。

Flash 复用统一思考管线及最终 payload 屏障，未添加独立参数捷径。默认/false 本地过滤 thought，true 仅透传上游实际返回的思考标记内容；双协议、流式/非流式、JSON、引用和可见 token 的分离规则沿用上一阶段。MEDIUM、预算、未验证模型及签名回放不在本次新增范围。完整观察表、适配边界和脱敏证据入口见上述章节。
