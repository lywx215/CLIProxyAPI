# Gemini Enterprise 内置 provider

`gemini-enterprise` 在普通 CLIProxyAPI 模式下，把 Gemini Generate Content 和 OpenAI Chat Completions 转换为 Google Discovery Engine `streamAssist` 请求。它使用宿主的 API Key、账号存储、OAuth 刷新、代理、轮询、优先级、会话亲和、冷却、限流、日志和请求统计。

公开模型名为上游发现的裸 ID，例如 `gemini-4-argon`。同名模型可以与其他 provider 共同调度；本功能不增加 Enterprise 优先级、隐式前缀、调用方 Key 绑定或全局默认模型。客户端预设和下面的样例显式使用 Argon，缺少 `model` 和宿主 `auto` 的既有行为保持不变。

## 配置与登录

在 v8 配置的现有 `oauth.providers` 下增加配置，避免创建重复的 `oauth` 节点：

```yaml
config-version: 8
oauth:
  auth-dir: "./auths"
  providers:
    gemini-enterprise:
      client-id: "YOUR_OAUTH_CLIENT_ID.apps.googleusercontent.com"
      client-secret-env: "GEMINI_ENTERPRISE_CLIENT_SECRET"
      project-id: "YOUR_PROJECT_ID_OR_NUMBER"
      location: "global"
      collection-id: "default_collection"
      engine-id: "YOUR_ENGINE_ID"
      assistant-id: "default_assistant"
```

`client-id`、`project-id`、`engine-id` 必填。其余资源字段的默认值见上例。环境变量名可省略，用于不要求 secret 的 OAuth client；Google Web OAuth client 通常需要设置它。secret 的**值**只通过进程环境或宿主自动加载的 `.env` 提供，不能放到 YAML、命令行参数或认证记录中。保护并忽略包含 secret 的 `.env`，不要提交它。

在 Google OAuth client 中登记完全一致的重定向地址 `http://localhost:8788/oauth-callback`。登录使用 Authorization Code、随机 state、S256 PKCE、offline access，以及 `cloud-platform`、`userinfo.email`、`userinfo.profile` scopes。授权同意页由用户自行完成。

```powershell
go build -o cli-proxy-api.exe ./cmd/server
./cli-proxy-api.exe --config config.yaml --gemini-enterprise-login --no-browser
```

可以省略 `--no-browser` 自动打开浏览器。可用 `--oauth-callback-port 8789` 覆盖端口，此时也必须先在 OAuth client 中登记 `http://localhost:8789/oauth-callback`。回调监听在本机 loopback，等待授权的期限为五分钟。远程登录需要把该本地端口安全转发给浏览器所在机器。

登录输出会明确说明自动续期是否就绪。Google 没有返回 refresh token 时，仅可保留同一账号、client 和资源绑定的已有 refresh token；没有匹配 token 则只能保存短期访问凭据，不会宣称可自动续期。`invalid_grant` 表示需要重新授权。

完整资源绑定的登录先按宿主选定的存储契约保存认证记录，再发现模型。管理页面的自动资源模式会先识别唯一的资源绑定，成功后才保存凭据；资源识别失败或取消时不保存新记录。之后的模型目录发现失败或被中止，不会丢失已经保存的 OAuth 授权。

认证记录的 provider/type 都是 `gemini-enterprise`，文件名包含账号和应用/资源绑定哈希。同一账号的不同应用不会互相覆盖。记录保存应用 ID、secret 环境变量名、资源绑定和 token；已有记录不会因 YAML 的应用配置变化而被重新绑定。修改应用或资源后重新登录，会建立相应绑定的记录。环境变量名对应的 secret 必须一直可用于该账号刷新。

凭据按宿主选定的 File、Postgres、Git 或 ObjectStore 契约持久化，**不提供额外加密**，也不自动迁移 Manager 的 DPAPI 凭据。客户端使用原有宿主 API Key，不增加第二套 Key。

## 管理页面与 v8 接口

管理页面提供 Gemini Enterprise 授权入口，默认填写引擎 ID 和可编辑的助手 ID；助手留空时使用 `default_assistant`。Google 授权后，后端搜索当前账号可见的项目，再通过 `Engine.Get` 查找手填引擎对应的唯一资源。自动模式使用 `default_collection` 和已知候选区域；项目不可见、匹配不唯一或使用其他集合/区域时，可打开手动模式补填项目、区域和集合，再重新授权。

当前页面内置已确认的公开 OAuth 应用 ID，Secret 由后端的 `GEMINI_ENTERPRISE_CLIENT_SECRET` 环境变量提供，不在页面输入或保存。表单只向启动接口提交本次会话参数，不修改全局配置，也不复用其他账号的资源绑定。CLI 和不带新模式参数的管理调用仍使用配置中的默认绑定；该默认配置的 PATCH/ETag 契约保持不变。需要使用其他 OAuth 应用时，可使用配置与 CLI，或按 v8 接口提交相应会话参数。

登录使用 `GET /v8/management/oauth/auth-url?provider=gemini-enterprise&is_webui=true`，新表单同时传入 `auto_discover_resources` 和公开会话参数，并检查响应的 `resource_inputs_supported: true`。旧后端缺少该标记时提示升级，并尽力取消刚创建的会话。返回的 `callback_url` 固定为 `http://localhost:8788/oauth-callback`；端口可用时由后端自动接收回调，否则支持把完整回调地址提交到 `/v8/management/oauth/callback`。收到回调后仍需等待 `/v8/management/oauth/status` 返回 `ok`，才表示资源绑定及凭据已保存；取消通过 `DELETE /v8/management/oauth/session` 完成。此入口仅在普通模式的 v8 管理 API 提供。

认证文件列表显示账号实际保存的应用和资源标识，并提示是否具有 refresh token。没有 refresh token 的账号不会显示单账号手动续期按钮。账号列表不公开 Secret 或 Token，不宣称支持尚未实现的 Enterprise 配额查询。接口字段见 [v8 Management API](docs/management-api-v8.md#gemini-enterprise)。

后端跟踪的 `static/management.html` 应与已提交前端的 `dist/index.html` 一致；完成生产构建后复制单文件产物，并核对 SHA256，避免后端托管旧版页面。

## 模型发现与调度

以下是模型目录发现，与授权期间的资源识别分开。登录会尝试只读发现；服务启动、账号新增/重绑时异步发现，运行期间约每五分钟刷新每个账号的目录：

```text
GET https://discoveryengine.googleapis.com/v1alpha/projects/{project}/locations/{location}/collections/{collection}/engines/{engine}/widgetConfigs/default_search_widget_config?modelInfoView=END_USER_WEB&languageCode=en
```

读取 `uiSettings.modelConfigInfo.resolvedModels` 中非空的真实 `modelId`，包括未来上游新增的虚拟 ID。网页 Auto 的空 ID 不会注册为宿主 `auto`。目录表示上游声明可选，不能据此声称每个模型均已生成验收。发现不发送生成请求，不枚举 Cloud 项目，也不依赖 `engines.list`。

目录只保存在本次服务运行的内存中，重启后重新发现；首次失败不发布猜测模型。成功目录替换该账号旧目录，成功空目录注销该账号模型。网络/解析失败保留该账号最近一次成功目录，日志报告失败，失败退避最长五分钟。token 轮换保留目录，并独立于旧 token 的失败退避。后台发现最多五个并发，不限制推理并发。发现结果按认证注册 epoch、资源绑定、探测 revision 和 registry epoch 校验，旧请求不能恢复已删、禁用或重绑账号的模型。

发现结果继续经过宿主的 `oauth.excluded-models`、`oauth.model-alias`、`oauth.settings`、账号显式 prefix 和调度规则。没有猜测 token 上限或 countTokens 能力。

## 公开接口与 SDK

支持：

- `GET /v1/models`、`GET /v1beta/models`，以及宿主已有的模型详情。
- `POST /v1/chat/completions`，普通响应和 SSE。
- `POST /v1beta/models/{model}:generateContent`。
- `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`。

OpenAI Python SDK 样例，环境变量中的 Key 是**宿主 API Key**：

```python
import os
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8317/v1", api_key=os.environ["CLIPROXY_API_KEY"])
args = dict(model="gemini-4-argon", messages=[{"role": "user", "content": "简短介绍你自己。"}])
print(client.chat.completions.create(**args).choices[0].message.content)
for chunk in client.chat.completions.create(**args, stream=True):
    print(chunk.choices[0].delta.content or "", end="")
```

Google GenAI Python SDK 样例：

```python
import os
from google import genai
from google.genai import types

client = genai.Client(api_key=os.environ["CLIPROXY_API_KEY"],
    http_options=types.HttpOptions(base_url="http://localhost:8317", api_version="v1beta"))
print(client.models.generate_content(model="gemini-4-argon", contents="简短介绍你自己。").text)
for chunk in client.models.generate_content_stream(model="gemini-4-argon", contents="简短介绍你自己。"):
    print(chunk.text or "", end="")
```

内部 Go SDK 必须显式声明 operation。使用 core auth manager 时，在 `executor.Options.Metadata[executor.OperationMetadataKey]` 中设置 `chat_completions`、`generate_content` 或 `stream_generate_content`，并使 source/response 同为 `openai` 或同为 `gemini`。`handlers.ModelExecutionRequest` 和 `handlers.ProtocolExecutionRequest` 提供 `Operation` 字段。缺少 operation 时 Enterprise 不参与调度。不要用转换后的旧接口冒充原生操作。

## 工具、历史和输出边界

每次请求使用完整文本历史和新的上游会话，宿主会话亲和不会作为 Google session。系统指令和角色历史转换为有角色标记的文本上下文。

| 输入 | 行为 |
| --- | --- |
| Gemini `googleSearch` / `google_search` | 设置 `toolsSpec.webGroundingSpec` |
| Gemini `urlContext` / `url_context` | 开启 web grounding，并增加实际读取指定 URL 的指令 |
| Gemini `codeExecution` / `code_execution` | 增加 Python 执行指令；只报告上游真实执行事件，不保证执行 |
| OpenAI `tools:[{"google_search":{}},{"url_context":{}}]` | 相同工具归并；兼容 camelCase、`code_execution` |
| OpenAI `gemini_tools`、`web_search_options` | 在本 provider 内兼容旧扩展；不会修改其他 provider |
| Gemini `application/json` | 提示约束，加最终合法 JSON 校验 |
| OpenAI `response_format:{"type":"json_object"}` | 提示约束，加顶层 JSON 对象校验 |

同一对象不能同时提供某个 camelCase/snake_case 别名。工具类型错误、函数工具、强制函数调用、tool 消息、图片、音频、文件、函数结果等不能保真转换的内容返回 400，不能静默删除后发送。普通可选参数如 sampling、token 限额、thinking 或 JSON schema 可被忽略，不宣称 schema 强制约束。忽略的字段名记入诊断日志和 `X-CLIProxyAPI-Ignored-Parameters` 响应头；经公开 HTTP 接口获取此头需要启用宿主 `requests.passthrough-headers`。

Gemini 输出保留文本、真实 executableCode、codeExecutionResult、grounding parts/metadata；OpenAI 输出正文、URL annotations，并将代码及结果呈现为文本块。上游 grounding 的 UTF-8 字节 offset 在 Gemini 中保留字节单位，在 OpenAI 中转换为 Unicode 字符索引，包含中文和 emoji 的回归测试。不生成上游没有提供的 function call、searchQueries、URL 抓取状态或隐藏思考/缓存计数，不泄露 thought 正文、assistToken 或 sessionInfo。输入与可见输出的本地 Token 统计另见下节；计数不可用时仍发布宿主请求数。

JSON 请求的流式输出先完整缓冲并校验，再发出可见内容。最终 JSON 输出仅包含通过校验的 JSON 文本，不夹带中间代码块或引用；需要观察代码、执行结果和引用时使用普通文本输出。

只有 `SUCCEEDED` 是成功终态。HTTP 200 内的 error、FAILED、SKIPPED、错误尾部、缺终态的 EOF 都作为错误处理。开始生成或工具活动后的失败（即使只有被过滤的 thought）不会跨账号重放。第一个可见块之前不会发送纯 role 块，已输出内容后的错误按宿主流式错误机制终止。客户端取消会释放上游。

模型推理和模型发现不设置连接后的总、读或响应头超时，受调用 context 取消控制。OAuth 凭据获取可使用短期超时。推理使用宿主临时/账号/全局代理顺序；刷新只使用账号/全局代理，不继承执行级临时代理。认证仅发送到固定的官方 HTTPS origin，且不跟随认证请求的重定向。

## Payload 规则

匹配上游协议使用 `protocol: gemini-enterprise`。在所有内建 model、history、tools、JSON 指令转换完成后，用户规则恰好作用于最终 streamAssist 业务 JSON 一次：

```yaml
requests:
  payload:
    filter:
      - models:
          - name: "gemini-4-argon"
            protocol: "gemini-enterprise"
        params: ["toolsSpec.webGroundingSpec"]
```

规则之后只做只读校验、序列化和传输封装。删除的字段不会被恢复，override 不会被内建逻辑覆盖。删除必需的 `generationSpec.modelId` 或产生无效 JSON/字段会明确报错，不自动修复。禁止通过规则注入可复用的上游 session/assist token。

## 本地 Token 统计与缺失分项（2026-10-23）

Enterprise 上游没有提供本次执行的完整 usage。当前实现使用随程序内嵌的 Gemma 3 SentencePiece 模型，对最终发送的 `query.text` 和合并后的可见输出进行本地分词。模型来源及 SHA256 与 [Google Go Gen AI SDK 的本地 tokenizer 配置](https://github.com/googleapis/go-genai/blob/main/tokenizer/tokenizer.go) 一致，固定在 `google/gemma_pytorch` 的提交 `014acb7ac4563a5f77c76d7ff98f31b568c16508`，解压后 SHA256 为 `1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c`。来源和 Apache 2.0 许可随模型保留，运行时不下载模型或调用远程计数接口。

这些数值是本机对可见业务文本的计数，不能视为 Argon 的官方 tokenizer、上游实际消耗或 Google 账单。Google SDK 的模型映射没有为 Argon 提供此保证；上游隐藏提示、思考和缓存也不在本机可观测范围内。

- 输入在所有内建转换和用户 payload 规则完成后读取，不修改最终请求。输出从合并后的文本、代码和执行结果 parts 读取，在 JSON 清理及 OpenAI Markdown 呈现之前计数，因此文本分片不改变结果，也不会把呈现包装重复计入。
- 普通 OpenAI 响应填充已知的 `usage.prompt_tokens`、`completion_tokens` 和 `total_tokens`；SSE 仅在请求 `stream_options.include_usage: true` 时追加 `choices: []` 的 usage 尾包。Gemini 响应及正常 SSE 终包填充相应的 `usageMetadata` 字段。失败流不发送成功终态或成功 usage 尾包。
- 隐藏思考、缓存读取和缓存创建保持未知，不用零代替。分词初始化或校验失败时，不能确定的输入、输出及总量不出现在协议 usage 中；宿主仍记录请求结果和耗时。连接前失败没有可用计数；生成中途失败只能记录已观察到的业务内容。
- 宿主 `TokenPresence` 按字段保留可用性，缺省为 `nil` 的旧生产者沿用原有行为。管理用量及快照导入/导出把未知字段序列化为 `null`，保留明确的零；有显式可用性标记时，不从分项重算未知或明确为零的总量。快照不会共享可变的可用性指针，去重区分未知值与零值。
- 队列及插件适配保留分项可用性。缺少思考或缓存信息时，规范化的 v2 细分标为 `unclassified`，不宣称完整分解。前端明细、汇总和分类图按已有计数展示；费用按配置单价和可用分项计算，也不能据此确定真实账单。JSON 明细导出保留 `null`，CSV 对应字段为空。

本地统计不开放 `countTokens` API，不更改目录声明或模型限制。前文及下文历史真实 Google/Argon 验收发生于该统计改动之前；这些历史记录保持原结论，不能证明新增计数等同真实上游用量。本轮回归及 Windows 策略限制另见 [前端与用量联调记录](GEMINI_ENTERPRISE_FRONTEND_CN.md)。

## 未支持与验收

仅限制本 provider：Responses、旧 Completions、Claude Messages、Interactions、countTokens、媒体及函数调用不支持；同名模型可继续由支持这些操作的其他 provider 执行。Chat 入口自动转换 Responses body 的兼容分支也不开放给 Enterprise。

CLIProxyAPIHome 模式目前未支持 Enterprise OAuth、刷新和模型发现。启用 Home 时本地发现、刷新和 Enterprise 执行均禁用，不会绕过控制平面使用本地账号池，也不发布本地目录覆盖 Home 信息。

离线测试：

```powershell
go test ./internal/auth/geminienterprise ./internal/translator/geminienterprise ./internal/config ./internal/runtime/executor ./sdk/auth ./sdk/cliproxy ./sdk/cliproxy/auth ./sdk/api/handlers ./test -run Enterprise -count=1
go test ./...
go build -o cli-proxy-api.exe ./cmd/server
```

实际 Python SDK 对本机合成上游的验收（无需 Google 凭据，不产生模型调用）：

```powershell
# In an isolated Python environment:
python -m pip install openai google-genai
python test/gemini_enterprise_sdk_smoke.py
```

该脚本测试两个 SDK 的模型列表、普通响应、SSE、JSON、代码/执行 parts、引用以及本地 Token 字段，并检查未填充隐藏思考/缓存分项，不能替代真实 Google 资源验收。真实验收必须先设置 client secret 环境变量并完成上面的新 OAuth 授权，确认目录发布 Argon，然后仅用必要的 Argon 请求检查 Search 引用、指定 URL 字段、不可访问 URL 对照、文本历史、JSON 和实际代码结果。不要自动遍历全部模型生成测试。历史旧网关的成功记录不算本融合版本通过。

## 本次验证记录（2026-10-22）

实际安装的 OpenAI Python SDK `3.36.1` 和 Google GenAI Python SDK `2.32.0` 对本机合成上游完成八项验收：双方普通响应、SSE、JSON 普通响应、JSON SSE 缓冲均通过。模型列表、OpenAI URL annotations、Gemini 代码/执行结果 parts 和 grounding、无虚构 usage 也在这些请求中校验。全部使用合成账号和 token，没有连接 Google 或发送真实模型请求。

Enterprise 针对性 Go 回归覆盖 OAuth state/PKCE 参数、缺 refresh token 的匹配保留、刷新并发/轮换/存储恢复、模型发现与旧结果竞争、双协议、公开根路径、操作排除、分片解析、多字节引用、错误防重放、取消和最终 payload 规则。当时最后一轮全量测试中，`sdk/cliproxy`、`sdk/auth`、`sdk/cliproxy/auth`、公开接口集成等相关包通过；该轮全量 `go test ./...` **未全绿**，剩余情况如下：

- 现有 `TestDIAG05RealSemanticBoundaries` 失败：Antigravity 流式输出通道关闭早于最终诊断事件提交，测试可能先封存 server span，导致 `upstream.attempt_finished` 缺失。独立三轮复测在不同流式 case 再现；该阶段尚未修改此执行路径或诊断实现。
- 现有 `TestInPlaceByteWritesAreReviewed` 扫描到工作区已有 `sync-worktrees-20261010/...` 和 `internal/runtime/budget/wal.go` 等未登记字节写入，该阶段未修改这些文件。
- 最后一轮 Windows Application Control 拦截 `cmd/diag-analyze` 和 `sdk/cliproxy/session` 的测试可执行文件，这两包的该轮测试未运行。

server 编译成功，并通过 `--help` 确认 `--gemini-enterprise-login`、`--no-browser`、`--oauth-callback-port`。该离线阶段没有执行部署、push、合并、Google IAM 修改或真实 OAuth 授权。

并行修改整合后再次编译成功，双 SDK 八项再次全部通过。同名模型混合调度、旧 Completions 回退到其他 provider，以及宿主 bootstrap 回归通过；`sdk/auth` 完整回归通过。该阶段最后一次针对性复测的 auth/discovery、translator、config、core auth、公开接口等通过；`internal/runtime/executor` 当时的最新测试二进制被 Windows Application Control 阻止启动（较早轮次的 Enterprise 测试通过，此次阻碍不是测试断言失败）。未修改或绕过系统执行策略。

以上是 2026-10-22 的离线阶段记录。八项离线结果不能单独证明真实资源权限、配额或工具可用性；后续新 OAuth 和真实 Google/Argon 验收另记如下。

## 真实 Google/Argon 验收（2026-10-23，北京时间）

本阶段使用用户提供的本地 OAuth 客户端 JSON 配置新 client，并由用户重新完成 Google 授权，取得 refresh token。真实目录发现返回七个模型，包含 `gemini-4-argon`，两个 SDK 的模型列表也各返回七项。client secret 的本机验收副本以 Windows 当前用户 DPAPI 保存在任务目录；这是本次验收辅助保管方式，不代表 provider 的账号存储自动提供 DPAPI 或其他加密。

最终真实报告标记 `status=passed`，`passed_unique_cases=8`，完成于 `2026-10-23T00:04:31+08:00`。验收使用 OpenAI Python SDK `3.36.1` 和 Google GenAI Python SDK `2.32.0`，结果分阶段记录：

| 用例 | OpenAI SDK | Google GenAI SDK | 执行阶段 |
| --- | --- | --- | --- |
| 文本历史与 JSON 普通响应 | 通过 | 通过 | 首次真实执行；早于下述解析修复 |
| Search SSE 与真实引用 | 修复后补跑通过 | 修复后补跑通过 | 保留首次双方失败历史 |
| 指定 URL 四字段 JSON 与不可访问 URL 负对照 | 通过 | 通过 | 修复后真实执行 |
| Python 代码及真实执行结果 SSE | 通过 | 通过 | 修复后真实执行 |

首次双方 Search 请求失败，未作为成功结果覆盖：上游 `inlineData.mimeType` 精确为 `application/json+suggestions` 的界面建议字段被错误归类为不支持的媒体。两次必要的定向 Argon 诊断还确认，grounding 可以独立于文本回复到达，其引用索引指向累计可见文本的 UTF-8 字节范围。实现已据此修复已确认的 `application/json+suggestions` UI 建议字段处理，以及独立 grounding 的累计文本定位，并补充回归；普通 `application/json` blob 仍不支持，不能据此放宽任意媒体或 JSON 数据输入。

独立引用校验确认，两套协议各有四个有效引用段；两条真实 Google grounding redirect 均以 HTTP 200 落到 [Google 官方 Gemini Enterprise 配额文档](https://docs.cloud.google.com/gemini/enterprise/docs/quotas-and-overages)。这项检查验证了引用范围与来源落地，不增加模型生成请求。

修复后，translator/公开接口针对性回归通过，server 重新编译成功，实际双 SDK 对合成上游的八项离线验收再次全部通过。真实补跑限于 Search、URL 负对照和代码 SSE；没有为了补齐表述而对已通过的历史/JSON 真实用例再次消耗额度。因此不能将本表描述为“全部八项均在最终二进制下重新执行”。

本次安排 SDK 真实生成请求十次，包括首次失败的两次 Search 和八个独立成功用例；另外执行两次定向诊断，生成请求总计十二次。未启用自动重试，未遍历全部发现模型。目录查询与模型列表均为只读请求。此次成功仅证明本次账号/资源下的必要 Argon 样例，不代表其他模型、所有输入或未来权限/配额均可用。

验收结束后隔离测试 server 已停止，报告未包含 Secret、Token、授权码或上游私有会话字段。截至本阶段 `00:04` 完成时，2026-10-22 记录的全量测试未全绿、现有诊断竞态、目录扫描断言和 Windows Application Control 限制尚未处理完毕；本阶段没有重跑全量，也未修改或绕过系统执行策略。没有部署、push、合并或 Google IAM 修改。后续上午的修复与完整回归另记如下，保留以上失败和拦截历史。

## 最终全量回归与提交准备（2026-10-23 上午，北京时间）

用户授权继续处理剩余全量测试问题后，`internal/runtime/executor/antigravity_executor_stream.go` 将最终诊断结算移到消费者可见的流式 EOF 或错误之前，避免消费者先封存 span 而遗漏 `upstream.attempt_finished`。独立的 usage 交付结算仍按其确认流程处理。新增 `testing/synctest` 确定性回归覆盖结束和错误路径的时序，复现并校验此前 `TestDIAG05RealSemanticBoundaries` 暴露的问题。

字节原地写入审查改为按 Git 边界枚举本仓库已跟踪和未忽略的 Go 源码，避免将嵌套 worktree 的源码纳入本仓库审查；已跟踪文件仍会受审查。无 Git 元数据的源码包使用排除嵌套仓库等目录的文件系统回退，并新增边界回归。`internal/runtime/budget/wal.go` 的一处复制经审查后精准登记：将 `json.Marshal` 结果复制到新分配且尚未对外可见的二进制 WAL frame，不修改调用方的 JSON 字节；没有修改 WAL 业务实现。

正常 Windows 用户在原有系统安全策略下，于 `09:34:58` 开始执行标准完整回归：

```powershell
go test -json -count=1 -timeout 240s ./...
```

结果为退出码 `0`，耗时 `69.172` 秒，**109 个包通过、34 个包标记 skip**，没有失败测试或执行拦截；这些是包级统计。`internal/runtime/executor`、`internal/util`、`cmd/diag-analyze` 和 `sdk/cliproxy/session` 等此前涉及失败或拦截的包均通过。早先单包复测确实受到 `VerifiedAndReputableDesktop` 策略拦截；最终全量在相同的正常安全策略下运行未被拦截，未修改或绕过系统策略。

该阶段准备提交的分支为 `codex/gemini-enterprise-20261023`，当时尚未 commit、push、合并或部署。本轮没有追加模型生成请求；真实验收仍采用前述八个独立成功用例、总计十二次生成请求的历史证据，并保留各用例的执行阶段限制。

提交前补充验证：原 `TestDIAG05RealSemanticBoundaries` 与新增 `TestAntigravityStreamDiagnosticsSettleBeforeDeliveryCompletion` 连续运行二十轮全部通过；最终 server 编译通过，临时验证二进制已删除。该轮没有新增真实模型调用，先前真实 Argon 双 SDK 八项证据继续保留。

## 前后端最终联调（2026-10-23，北京时间）

后端 provider、协议与相关文档已分别提交，管理 OAuth 和账号记录包含在 `a524f539`；管理前端提交为 `6b85096`。本轮补齐后端 `static/management.html` 的 Enterprise 页面装配及 v8 接口说明。固定原产物的 `VERSION` 和 `BUILD_TIME` 后，从已提交前端重新构建得到相同字节；前后端页面 SHA256 均为 `93c3e4916a7406d0750e4b5d4936c233566c8a2117b0827eadb78e4cca81df58`。

隔离后端直接托管上述管理页面，使用禁用的合成账号和占位 Secret。前端真实 API 模块对该后端的九项检查通过，覆盖读取、保存、ETag 冲突、账号绑定、PKCE 登录响应、手动拒绝回调和取消后旧回调拦截。浏览器另行确认五字段资源表单、保存后重读、未编辑表单的并发冲突、合成会话生成与取消、筛选和资源搜索、账号原绑定与续期提示；控制台无错误。

`bun run verify` 的 1756 项测试、Lint、TypeScript 和生产构建通过；后端 server 编译通过。该轮标准 `go test -json ./...` 有 108 个包通过、34 个包标记 skip，`internal/cache` 的测试程序被 Windows Application Control 阻止执行，退出码为 1；没有失败测试用例，但不能将这轮称为全量全绿。未修改或绕过系统策略。

本轮没有打开 Google 授权页、交换或保存真实 Token、追加模型请求、push 或部署。合成本机服务和临时浏览器会话已关闭；前述真实 Argon 验收仍以其原始阶段记录为准。

## 资源自动发现流程的真实验收（2026-10-23 午后，北京时间）

本轮重新编译并固定使用后端提交 `74fe54b7ad53adbd4863166b53937231974b7704`；配套前端已提交版本 `cd671427334346076b96c37e36d5a2e5ed22b142` 作为版本参考，随后并发产生的前端未提交改动不在本轮真实验收范围内。下述八项生成用例均运行在同一个已校验的 server 二进制下，其 SHA256 为 `101aa594b65e35fd2e97498df7bb9e842216edbb23c258b8b989fecf6f6342f8`。最终脱敏报告完成于 `2026-10-23T13:07:56+08:00`。

复用此前确认的 OAuth 客户端及 Windows 当前用户 DPAPI 保存的 Secret，由用户完成新的 Google 授权。验收从空的隔离认证目录启动，全局 `oauth.providers.gemini-enterprise` 绑定为空；通过真实 v8 管理授权入口传入 `auto_discover_resources=true`、客户端 ID、Secret 环境变量名、引擎 ID 和助手 ID，没有随会话传入项目、区域或集合。

后端完成授权码交换、Google 账号识别、可见项目搜索和 `Engine.Get` 唯一匹配，保存一条同时具有 access token 和 refresh token 的新记录。自动识别出的项目、`global` 区域和 `default_collection` 集合与已有资源一致，引擎及输入的 `default_assistant` 绑定匹配。助手 ID 来自本次输入，不能将它描述为自动发现助手。`GET /v8/management/credentials` 的公开摘要与保存的绑定一致，全局配置及其 ETag 保持不变，没有加载或覆盖旧账号记录。取得 refresh token 不等于已验证自然到期或真实轮换，本轮未增加这类验证。

OpenAI Python SDK `3.36.1` 和 Google GenAI Python SDK `2.32.0` 的模型列表各返回七项，均包含 `gemini-4-argon`，且没有为该模型宣称 `countTokens` 能力。

| 真实 Argon 用例 | OpenAI SDK | Google GenAI SDK |
| --- | --- | --- |
| 文本历史、随机口令回忆及 JSON 普通响应 | 通过 | 通过 |
| Google Search SSE 与实际引用 | 通过 | 通过 |
| 指定 URL 四字段 JSON 与不可访问 URL 负对照 | 通过 | 通过 |
| Python 代码及带随机标记的真实执行结果 SSE | 通过 | 通过 |

本轮累计发送八次真实 Argon 生成请求，八个独立用例全部通过，未启用自动重试，也未遍历其他模型生成。两套协议均校验正常终态及没有虚构 usage。独立引用校验确认 OpenAI 的四段引用范围有效，Gemini 的五段 grounding 支持范围与 UTF-8 文本一致；两条实际引用跳转均以 HTTP 200 落到 [Google 官方 Gemini Enterprise 配额文档](https://docs.cloud.google.com/gemini/enterprise/docs/quotas-and-overages)。目录、模型列表和引用落地检查不计入生成请求。

首次会话因浏览器无法访问本机 loopback 而超时的记录继续保留。后续新会话已完成回调、交换和资源绑定保存；验收辅助脚本在读取账号摘要时曾误用旧列表路径而得到 404，修正为 `/v8/management/credentials` 后复用刚保存的凭据继续，没有为该辅助脚本问题再次授权或增加生成请求。这些中断记录没有作为通过结果覆盖。

脱敏本机证据保留在任务目录 `enterprise-live-resources-20261023` 的 `FINAL_REPORT.json`、`resource-discovery-result.json` 和 `argon-live-results.json` 中；Secret、Token、授权码及凭据目录不随文档提交。验收服务已停止，本轮临时二进制已删除，原 DPAPI Secret 未改变，没有部署、推送或修改 Google IAM。

本轮没有重跑完整 Go 测试，也没有修改或绕过 Windows Application Control。此前固定提交 `4a23c88b` 的隔离全量结果（91 包通过、17 包受策略拦截、33 包标记 skip）及十六份专项诊断文件继续保留；本轮真实授权与八项 SDK 成功不能替代该全量回归结果。此前各阶段的失败、拦截和成功记录均按原阶段保留。

## 后端 9ab28a2f 补测记录（2026-10-23）

本次固定后端提交 `9ab28a2f333dbc8dbdb240f84a3885105ad9cfaa`，在隔离源码快照中补跑此前被 Windows Application Control 阻止启动的四个 Go 包及离线双 SDK。执行前后均校验 2,564 个源码文件与提交内容一致，不包含并行工作区改动；快照没有 `.git` 元数据。最终脱敏报告完成于 `2026-10-23T18:06:02+08:00`。

标准 Go 命令于北京时间 `17:59:34` 开始，`17:59:57` 完成，退出码为 `0`，耗时 `23.11` 秒：

```powershell
go test -work -json -count=1 -timeout 240s ./sdk/cliproxy/usage ./internal/runtime/executor/helps ./test ./internal/api/handlers/management
```

| Go 测试包 | 通过的测试及子测试数 | 结果 |
| --- | ---: | --- |
| `sdk/cliproxy/usage` | 34 | 通过 |
| `internal/runtime/executor/helps` | 1,358 | 通过 |
| `test` | 396 | 通过 |
| `internal/api/handlers/management` | 503 | 通过 |
| 合计 | 2,291 | 四包全部通过 |

没有失败断言或受阻包。此前修正未知零总量后的 `TestEnsureTokenBreakdownPreservesPartialAvailability` 本次已实际执行并通过，补齐了上一阶段最终版本未能运行的断言。`-work` 仅保留 Go 默认临时目录中的文件供只读哈希及签名核对，没有改变编译与原生启动方式，也没有迁移、更名或重新执行测试程序来规避策略。

同一固定快照在已安装 SDK 依赖的 Python 环境中执行：

```powershell
python test/gemini_enterprise_sdk_smoke.py
```

离线回归于 `18:02:43` 开始，`18:02:57` 完成，退出码为 `0`，耗时 `13.25` 秒。OpenAI Python SDK `3.36.1` 的五项检查和 Google GenAI Python SDK `2.32.0` 的四项检查全部通过，共九项：

- OpenAI：普通响应、引用及 Token 用量；SSE 首个可见分片与终态；`include_usage` 末尾用量块；JSON 普通响应校验；JSON SSE 缓冲。
- Google GenAI：普通响应、代码、执行结果、grounding 及 Token 用量；SSE 终态与用量；JSON 普通响应校验；JSON SSE 缓冲。

两套真实 SDK 均只请求本机 Go 代理及其合成上游；该 Go 助手通过标准 `go test -count=1 -timeout 180s -run ^TestEnterpriseSDKHarness$ ./test` 启动。用量检查确认已知输入/输出为正数、合计一致，未提供的思考/缓存细分保持缺失。本次没有重新授权、调用 Google 或发送真实模型生成请求，不能将九项离线检查描述为新的真实 Argon 验收。结束后确认没有遗留 SDK 助手进程。

历史 Code Integrity 事件及只读错误码映射确认，先前的 `VerifiedAndReputableDesktop` 拦截状态 `0xc0e90002` 对应 Windows 错误 `4551`。本次前后系统仍处于强制模式 `1`，策略文件 SHA256 均为 `2668895a5b233a80432d00d67251d7b7f52686a3fb13780f4b242c5a1f937a01`，四个测试程序仍未签名；回归时间窗口没有查到新的匹配拦截事件。没有修改系统策略、证书、信任或签名配置，也没有使用替代执行方式。

本次测试程序的 SHA256 与已保存的历史受阻程序均不相同；源码路径和 Git 元数据条件也与此前工作区运行不同。现有证据尚不能确定放行决定变化的具体原因，不能据此认定原受阻程序已经获准，或保证未来构建不再被拦截。上述成功仅补齐固定提交的四包与离线双 SDK，不替代此前各阶段的 `go test ./...` 结果，不宣称重新完成全量回归；历史失败和拦截记录按原阶段保留。

本机脱敏证据目录标识为 `windows4551-9ab28a2f-20261023`，包含 `FINAL_REPORT.json`、`go-four.log`、`go-four-summary.json`、`go-four-work-audit.json`、`sdk-offline.log`、`sdk-offline-summary.json`、策略前后快照、Code Integrity 事件及 `preservation-supplement.json`。既有 169 份受保护证据、另行核对的 61 份早期诊断以及 2,145 个旧源码快照文件均保持原哈希；这些计数分组记录，不合并为不重复文件总数。其他无关改动保留，补测期间并行产生的 `7b42e084` 未纳入固定快照。本节文档补录仅核对已有证据，没有追加测试、模型请求或修改静态产物，暂不推送。

## OAuth 切换与本机回调故障排查

- **切换新旧 client：** 修改 `oauth.providers.gemini-enterprise` 只影响之后的登录，不会重写已保存账号的 client/resource 绑定。新 client 必须重新授权；原 client 的 refresh token 不能替换为新 client 的凭据。同一账号不同 client 的记录按绑定哈希共存。不同 client 建议使用不同的 `client-secret-env` 名称，避免覆盖旧环境变量后让旧记录使用新 client 的 secret。
- **Testing 测试用户：** 对处于 Testing 状态的外部 OAuth 应用，若授权页提示应用正在测试或 `access_denied`，检查新 client 所属 Google Cloud 项目的 OAuth consent/Audience 设置，将需要授权的账号加入 Test users 后重试。旧项目的测试用户名单不会随下载的客户端 JSON 自动迁移。文档和日志不应记录实际个人账号名单。
- **Testing 续期边界：** 本次取得了 refresh token，没有等待其自然过期或触发真实轮换。Google 对 External + Testing 且请求超出基本身份 scopes 的授权，默认使 refresh token 在七天后过期；本 provider 请求的 `cloud-platform` 不属于基本身份例外，见 [Google token expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration)。Google Workspace 的 Trusted 管理状态可能覆盖这一默认限制，见 [OAuth app state overview](https://developers.google.com/identity/protocols/oauth2/production-readiness/overview)；本次未核验该管理状态，不能把收到 refresh token 描述为无限期自动续期保证。
- **代理浏览器访问 localhost 被拒绝：** 先确认登录进程仍在等待、端口未冲突，且浏览器与 CLI 位于同一台机器。可以在同机直连浏览器中，将当前回调地址的 `localhost` 临时替换成 `127.0.0.1`，保留端口、`/oauth-callback` 和完整原查询参数。该操作只改变本机浏览器到 loopback 监听器的访问方式，不改变 OAuth 流使用的已登记 redirect URI。不要把包含授权码的完整回调 URL 粘贴到聊天、文档或日志中。
- **跨机器访问：** `localhost` 和 `127.0.0.1` 都指向浏览器所在机器，不能用于访问另一台机器上的 CLI。远程场景应按前述方式建立安全的本地端口转发，并保持回调端口和登记地址一致。
