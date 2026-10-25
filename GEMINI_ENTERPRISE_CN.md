# Gemini Enterprise 内置 provider

`gemini-enterprise` 在普通 CLIProxyAPI 模式下，把 Gemini Generate Content 和 OpenAI Chat Completions 转换为 Google Discovery Engine `streamAssist` 请求，并通过应用媒体工具提供图片和视频生成。它使用宿主的 API Key、账号存储、OAuth 刷新、代理、轮询、优先级、会话亲和、冷却、限流、日志和请求统计。

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

认证文件列表显示账号实际保存的应用和资源标识，并提示是否具有 refresh token。没有 refresh token 的账号不会显示单账号手动续期按钮。账号列表不公开 Secret 或 Token。凭证卡可读取绑定项目、区域及版本共享的每日助手与搜索额度，显示 Google 已上报用量、实际上限、计算剩余量、重置及查询时间；无数据不补零，无权限时提示检查 `roles/viewer` 或等效读取权限，其他读取失败显示额度暂不可用，不改变凭证健康状态。接口字段见 [v8 Management API](docs/management-api-v8.md#gemini-enterprise)。

后端跟踪的 `static/management.html` 应与已提交前端的 `dist/index.html` 一致；完成生产构建后复制单文件产物，并核对 SHA256，避免后端托管旧版页面。

## 模型发现与调度

以下是模型目录发现，与授权期间的资源识别分开。登录会尝试只读发现；服务启动、账号新增/重绑时异步发现，运行期间约每五分钟刷新每个账号的目录：

```text
GET https://discoveryengine.googleapis.com/v1alpha/projects/{project}/locations/{location}/collections/{collection}/engines/{engine}/widgetConfigs/default_search_widget_config?modelInfoView=END_USER_WEB&languageCode=en
```

读取 `uiSettings.modelConfigInfo.resolvedModels` 中非空的真实 `modelId`，包括未来上游新增的 ID。网页 Auto 的空 ID 不会注册为宿主 `auto`。图片使用已确认对应 Nano Banana Pro 的正式模型 ID `gemini-3-pro-image`，在应用图片功能明确启用时发布，通过 `toolsSpec.imageGenerationSpec` 路由。视频使用 `veo-3.1` 产品家族入口，在应用视频功能明确启用时发布，通过 `toolsSpec.videoGenerationSpec` 调用 Enterprise 默认视频工具，具体底层变体由 Enterprise 决定。两个媒体入口均不作为上游 `generationSpec.modelId` 发送。目录表示上游声明可选，不能据此声称每个模型均已生成验收。发现不发送生成请求，不枚举 Cloud 项目，也不依赖 `engines.list`。

目录只保存在本次服务运行的内存中，重启后重新发现；首次失败不发布猜测模型。成功目录替换该账号旧目录，成功空目录注销该账号模型。网络/解析失败保留该账号最近一次成功目录，日志报告失败，失败退避最长五分钟。token 轮换保留目录，并独立于旧 token 的失败退避。后台发现最多五个并发，不限制推理并发。发现结果按认证注册 epoch、资源绑定、探测 revision 和 registry epoch 校验，旧请求不能恢复已删、禁用或重绑账号的模型。

发现结果继续经过宿主的 `oauth.excluded-models`、`oauth.model-alias`、`oauth.settings`、账号显式 prefix 和调度规则。没有猜测 token 上限或 countTokens 能力。

## 公开接口与 SDK

支持：

- `GET /v1/models`、`GET /v1beta/models`，以及宿主已有的模型详情。
- `POST /v1/chat/completions`，普通响应和 SSE。
- `POST /v1beta/models/{model}:generateContent`。
- `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`。
- 图片和视频媒体模型同时支持以上两套内容接口，并提供图片专用接口、OpenAI 视频任务和 Gemini 原生视频任务，见下一节。

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

内部 Go SDK 必须显式声明 operation。使用 core auth manager 时，在 `executor.Options.Metadata[executor.OperationMetadataKey]` 中设置 `chat_completions`、`generate_content` 或 `stream_generate_content`，并使 source/response 同为 `openai` 或同为 `gemini`。专用媒体执行分别使用 `openai-image` / `image_generation` 和 `openai-video` / `video_generation`，source 与 response 也须一致；原生视频 HTTP 入口通过共享任务服务调用后者。`handlers.ModelExecutionRequest` 和 `handlers.ProtocolExecutionRequest` 提供 `Operation` 字段。缺少 operation 时 Enterprise 不参与调度。不要用转换后的旧接口冒充原生操作。

## 图片与视频生成

图片入口采用已确认的正式模型 ID，目录发现必须同时确认 `disable-image-generation: FEATURE_STATE_OFF` 和 `modelConfigs.gemini-3-pro-image: MODEL_ENABLED` 才发布。视频在 `disable-video-generation: FEATURE_STATE_OFF` 时发布 `veo-3.1` 产品家族入口。它们沿用 `oauth.excluded-models`、`oauth.model-alias`、`oauth.settings` 和账号 prefix；使用别名或前缀时，把示例中的模型名替换为模型目录实际发布的名称。

| 模型名 | 应用工具 | 结果 |
| --- | --- | --- |
| `gemini-3-pro-image` | 已确认的 Nano Banana Pro 正式模型 ID；发送 `toolsSpec.imageGenerationSpec: {}`，不设置上游 `generationSpec.modelId` | 单张图片 |
| `veo-3.1` | Veo 3.1 产品家族入口；发送 `toolsSpec.videoGenerationSpec: {}`，不设置上游 `generationSpec.modelId` | 单个 MP4；具体底层变体由 Enterprise 决定 |

视频工具的产品版本已确认为 **Veo 3.1**：[Enterprise 官方发布说明](https://docs.cloud.google.com/gemini/enterprise/docs/release-notes#March_03_2026) 和应用公开界面的工具标题一致。按用户确认，公开入口使用 `veo-3.1`，调用此前已成功的 Enterprise 默认视频工具。它表示产品家族，不固定为 `veo-3.1-generate-001`、`veo-3.1-fast-generate-001` 或其他具体变体。[官方 streamAssist 定义](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist#VideoGenerationSpec) 的 `VideoGenerationSpec` 没有模型选择字段，`generationSpec.modelId` 选择 Assistant 模型；媒体请求不发送该字段，由 Enterprise 决定实际视频变体。此入口不宣称取得任何指定变体的独立 API 调用权限。

2026-10-24 使用本地 OAuth 凭证和已绑定项目，对 `veo-3.1-generate-001`、`veo-3.1-fast-generate-001` 分别提交了直连请求：官方专用 Veo API（`us-central1` 的 `predictLongRunning`）均返回 HTTP 403 / `IAM_PERMISSION_DENIED`，缺少 `aiplatform.endpoints.predict`；在现有 Enterprise `streamAssist` 的 `generationSpec.modelId` 中分别指定两者，均返回 `INVALID_ARGUMENT`，消息为 `model id: <requested-id> is invalid`。这些结果表明当前凭证/绑定项目不能通过所测入口调用这两个明确 ID，不代表模型本身不存在，也不能据此确认默认视频工具的底层变体。该指定 ID 测试阶段未接受任何生成任务、未自动重试、未修改 IAM、应用设置或 API 开关；未注册这两个未经成功验证的具体视频 ID，没有这两款指定变体的原生接口成功、下载及播放证据。后续接入的 `veo-3.1` 产品家族入口沿用 Enterprise 默认工具，不改变这些历史测试结论。

下列视频调用均使用 `veo-3.1`。客户端只选择 Enterprise 视频工具入口，不能通过模型名或 Payload 规则把它固定到 `generate-001` / `fast-generate-001` 等底层变体；加入上游 `generationSpec` 会被媒体最终字段校验拒绝。

2026-10-24 的 `veo-3.1` 接入复测：隔离代理已发现并发布该入口，原生模型详情包含 `predictLongRunning` 和由 Enterprise 选择变体的说明。Google Gen AI SDK 2.32.0 的 `generate_videos` 创建代理 operation、`operations.get` 查询及跨身份隔离均通过；实际生成返回上游 429，任务进入失败终态。随后单次直连默认 `videoGenerationSpec: {}` 复核，传输 HTTP 200，但流内错误为 `429 / RESOURCE_EXHAUSTED`，`ErrorInfo.reason` 明确为 `RATE_LIMIT_EXCEEDED_VIDEO_GENERATION`，未返回视频文件引用。两次请求均未自动重试。当时 `metricDescriptors.list` 返回 403，尚未直接读取视频时间序列，因此当时没有据此判定剩余额度、恢复时间或日额度已经用尽；后续直接读取成功的证据见下方日额度排查记录。本次没有新生成的 MP4，因此该阶段 `veo-3.1` 的真实生成、下载和播放验收仍未完成；历史成功视频及离线夹具不计作本次通过。模型发现、工具路由、Gemini 原生 operation/下载契约和别名/前缀测试通过，服务编译成功。额度重置后的新一轮核心媒体成功及用量断言失败，分别记录在下方恢复实测小节，不覆盖本阶段失败历史。

2026-10-24 使用本地凭证和隔离代理，分别以 Google Gen AI SDK 调用正式图片 ID 的 `generateContent` 和 `streamGenerateContent`，两次均成功返回单张 `inlineData` PNG，已下载、解压校验并预览，尺寸均为 1408×768；每次仅记录一次生成用量。模型详情返回 `models/gemini-3-pro-image`，当前目录不再发布原先的自定义媒体名称。该复测只验证正式图片入口，不将历史视频成功或离线夹具计作某个精确视频模型 ID 的实测证据。

媒体请求只接受文本提示词。Chat/Gemini 内容接口可包含文本历史和系统指令，输出模式由所选媒体模型决定；普通文本模型仍拒绝未请求的媒体输出。图片编辑、输入图片或视频、参考文件、延长视频、多结果，以及尺寸、分辨率、时长、比例等无法保证生效的控制参数返回 400，不会静默忽略。`gemini-2.5-flash-image`、`gemini-3.1-flash-image` 在当前应用中被禁用，本轮不启用它们，也不修改 Google 应用或 IAM 设置。

| 入口 | 创建与返回行为 |
| --- | --- |
| `POST /v1/chat/completions` | 图片或视频；普通 JSON 和 SSE；等待生成并下载后返回 Base64 data URI |
| Gemini `:generateContent`、`:streamGenerateContent?alt=sse` | 图片或视频；原生 `contents` 请求；返回 `parts[].inlineData` |
| `POST /v1/images/generations` | 图片；`data[].b64_json`；仅普通 JSON |
| `POST /openai/v1/videos` | 视频异步任务；创建后查询 `GET /openai/v1/videos/{id}`，完成后 `GET /openai/v1/videos/{id}/content` |
| `POST /v1/videos`、`POST /v1/videos/generations` | Enterprise 视频模型使用同一异步任务；查询 `/v1/videos/{id}`，下载 `/v1/videos/{id}/content`；其他模型沿用原行为 |
| `POST /v1beta/models/{model}:predictLongRunning` | Gemini 原生视频 operation；支持 SDK 查询和文件下载 |

### Chat 的媒体字段与 SSE

环境变量仍是宿主 API Key。下面分别演示普通图片和流式视频；客户端关闭自动重试，避免调用方在连接不确定时重新提交生成。

```python
import base64
import os
from pathlib import Path
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8317/v1",
                api_key=os.environ["CLIPROXY_API_KEY"], max_retries=0)
image = client.chat.completions.create(
    model="gemini-3-pro-image",
    messages=[{"role": "user", "content": "生成一张日落海边灯塔的图片。"}],
)
uri = image.choices[0].message.model_dump()["images"][0]["image_url"]["url"]
mime = uri.split(";", 1)[0].removeprefix("data:")
extension = {"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[mime]
Path("chat-image." + extension).write_bytes(base64.b64decode(uri.split(",", 1)[1]))

for chunk in client.chat.completions.create(
    model="veo-3.1",
    messages=[{"role": "user", "content": "生成一个海边灯塔旁海浪缓慢涌动的视频。"}],
    stream=True,
):
    for choice in chunk.choices:
        for item in choice.delta.model_dump().get("videos") or []:
            uri = item["video_url"]["url"]
            Path("chat-video.mp4").write_bytes(base64.b64decode(uri.split(",", 1)[1]))
```

Chat 图片沿用 `message.images[] = {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}`；视频对应 `message.videos[] = {"type":"video_url","video_url":{"url":"data:video/mp4;base64,..."}}`。两者均可能附带真实文本，视频没有文本也可成功。SSE 使用相同的 `delta.images` / `delta.videos` 字段，在完整下载后发送媒体块、结束块和 `[DONE]`，不提供逐帧视频或生成进度流。

### Gemini 内容接口的 inlineData

```python
import os
from pathlib import Path
from google import genai
from google.genai import types

client = genai.Client(
    api_key=os.environ["CLIPROXY_API_KEY"],
    http_options=types.HttpOptions(
        base_url="http://localhost:8317", api_version="v1beta",
        retry_options=types.HttpRetryOptions(attempts=1),
    ),
)
response = client.models.generate_content(
    model="gemini-3-pro-image", contents="生成一张海边灯塔的图片。",
)
for part in response.candidates[0].content.parts:
    if part.inline_data:
        mime = part.inline_data.mime_type
        extension = {"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[mime]
        Path("gemini-image." + extension).write_bytes(part.inline_data.data)

for chunk in client.models.generate_content_stream(
    model="veo-3.1", contents="生成一个海边灯塔旁海浪涌动的视频。",
):
    for candidate in chunk.candidates or []:
        for part in candidate.content.parts or []:
            if part.inline_data:
                Path("gemini-video.mp4").write_bytes(part.inline_data.data)
```

HTTP 请求保留 Gemini `contents` 结构，例如 `{"contents":[{"role":"user","parts":[{"text":"生成一张海边灯塔的图片。"}]}]}`。响应使用 `candidates[].content.parts[].inlineData = {"mimeType":"image/png","data":"<base64>"}`，视频 MIME 为 `video/mp4`；SDK 将 Base64 解码为 `part.inline_data.data`。媒体应从 parts 读取，不能只读取 `.text`。可选的 `generationConfig.responseModalities` 仅接受 `TEXT` 与对应 `IMAGE` / `VIDEO`，`candidateCount` 只能为 1；这不启用其他生成控制参数。

### 图片专用接口

```python
import base64
import os
from pathlib import Path
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8317/v1",
                api_key=os.environ["CLIPROXY_API_KEY"], max_retries=0)
result = client.images.generate(
    model="gemini-3-pro-image", prompt="生成一张水彩风格的山间湖泊图片。",
    n=1, response_format="b64_json",
)
Path("image.bin").write_bytes(base64.b64decode(result.data[0].b64_json))
```

对应 HTTP 请求体为 `{"model":"gemini-3-pro-image","prompt":"生成一张水彩风格的山间湖泊图片。"}`。响应为 `{"created":<unix秒>,"data":[{"b64_json":"<base64>"}]}`，下载到本机后按图片实际格式保存或解码。此入口只接受单张图片和 `b64_json`，不返回外部 URL；不支持 `stream: true`。宿主禁用全部图片生成时此入口同样不可用。

### OpenAI 异步视频任务

OpenAI SDK 视频客户端使用 `/openai/v1` 作为 base URL。提交只传 `model` 和 `prompt`，不要发送 SDK 为其他模型提供的 `seconds`、`size` 或 `input_reference`。下面样例使用 OpenAI Python SDK 默认的宽松响应解析。

```python
import os
import time
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8317/openai/v1",
                api_key=os.environ["CLIPROXY_API_KEY"], max_retries=0)
job = client.videos.create(model="veo-3.1",
                           prompt="生成一个清晨薄雾中小舟缓慢漂过湖面的视频。")
while job.status in ("queued", "in_progress"):
    time.sleep(2)
    job = client.videos.retrieve(job.id)
if job.status != "completed":
    raise RuntimeError(job.error)
client.videos.download_content(job.id).write_to_file("openai-video.mp4")
```

HTTP 创建接受 JSON，也兼容 SDK 的文本 multipart 表单。立即返回 `object: "video"`、代理生成的 `id`、`model`、`status`、`progress` 和 `created_at`。状态为 `queued`、`in_progress`、`completed` 或 `failed`，终态带 `completed_at`；`progress` 只区分未完成的 0 和完成的 100，不表示 Google 实时进度。任务失败返回安全的 `error`，不会自动重新生成。查询和内容下载共用同一份缓存 MP4，内容未就绪时下载返回 409。

当前兼容创建、轮询和内容下载的异步工作流，返回可确认的任务字段。当前任务服务未提取并发布时长、尺寸元数据，任务对象不提供 `seconds`、`size`，也不填入 Sora 的默认值；调用方不要依赖 `job.seconds` / `job.size`。OpenAI Python SDK 3.36.1 的 `Video` 类型将这两个字段列为必需项，启用 `_strict_response_validation=True` 会因缺少字段拒绝响应，按其完整响应 schema 校验的客户端同样不兼容。这是目前的响应兼容边界，不能据默认 SDK 解析成功宣称完整的 OpenAI 视频 schema 兼容。

### Gemini 原生视频任务

Google Gen AI SDK 使用与内容接口相同的本机 `Client` 配置。下面覆盖原生 `generate_videos`、`operations.get` 和 `files.download`：

```python
import os
import time
from google import genai
from google.genai import types

client = genai.Client(
    api_key=os.environ["CLIPROXY_API_KEY"],
    http_options=types.HttpOptions(
        base_url="http://localhost:8317", api_version="v1beta",
        retry_options=types.HttpRetryOptions(attempts=1),
    ),
)
operation = client.models.generate_videos(
    model="veo-3.1", prompt="生成一个晨雾中小舟漂过湖面的视频。",
)
while not operation.done:
    time.sleep(2)
    operation = client.operations.get(operation)
if operation.error:
    raise RuntimeError(operation.error)
video = operation.response.generated_videos[0].video
client.files.download(file=video)
video.save("gemini-native-video.mp4")
```

原生 HTTP 流程如下，所有请求都使用同一宿主 API Key 鉴权：

```http
POST /v1beta/models/veo-3.1:predictLongRunning
x-goog-api-key: <CLIPROXY_API_KEY>
Content-Type: application/json

{"instances":[{"prompt":"生成一个晨雾中小舟漂过湖面的视频。"}]}
```

创建返回 `{"name":"models/veo-3.1/operations/<opaque-id>","done":false}`。原样查询该 name 对应的 `GET /v1beta/models/veo-3.1/operations/<opaque-id>`；也支持 `GET /v1beta/operations/<opaque-id>`。完成后的原生结构为：

```json
{
  "name": "models/veo-3.1/operations/<opaque-id>",
  "done": true,
  "response": {
    "generateVideoResponse": {
      "generatedSamples": [
        {"video": {"uri": "files/<opaque-id>", "encoding": "video/mp4"}}
      ]
    }
  }
}
```

`files/<opaque-id>` 是本机代理的相对资源名，SDK 使用配置的 base URL 和宿主 Key 下载；它不是 Google 上游文件 ID，也不是可匿名访问的外部链接。手动下载可用 `GET /download/v1beta/files/<opaque-id>:download?alt=media` 或 `GET /v1beta/files/<opaque-id>:download?alt=media`，返回鉴权后的 MP4 字节。`parameters` 仅允许可选的 `sampleCount: 1`，不支持其他控制参数。

### 下载、任务缓存与限制

生成结果固定由生成该文件的账号及资源绑定下载。代理去重重复的文件引用，只接受同源且会话路径、文件 ID 一致的下载跳转，并在本机校验 Base64、MIME 和文件大小。无效结果或下载失败返回错误；生成一旦开始，不因下载失败、流尾错误或凭证切换重新提交生成。公开结果和错误不包含 OAuth Token、`assistToken`、上游 session 或文件 ID。

OpenAI 和 Gemini 视频任务共用单服务实例的内存缓存。创建请求返回后，任务继续在后台运行；服务关闭统一取消。客户端轮询或下载不会再次调用生成，也不重复记录生成用量；媒体二进制和 Base64 不计入文本 Token。

| 限制 | 行为 |
| --- | --- |
| 并行和排队 | 最多 2 个后台视频任务并行，32 个排队；队列满返回 429 |
| 单文件 | 解码后最多 32 MiB；内容接口的图片和视频也适用 |
| 已结束任务 | 最多 128 项，完成结果总字节最多 128 MiB；超限淘汰最旧的已结束任务 |
| 保留时间 | 沿用 `multimedia.video-result-auth-cache-ttl`，默认 3 小时；从任务结束起计时，较短配置可缩短保留，上限为 3 小时 |
| 访问身份 | 绑定创建请求的鉴权身份；使用其他身份访问、结果过期或被淘汰均返回 404 |
| 服务重启和多实例 | 重启丢失任务及文件；缓存不跨实例共享，不提供外部文件托管 |

缓存 TTL 不是推理或下载超时。直接 Chat、Gemini 内容接口和图片专用接口随请求 context 取消；专用视频任务的生成及下载由服务生命周期管理。业务生成完成后的缓存查询、下载可以重试，创建请求不要自动重试。

### 本轮媒体真实验收状态（2026-10-23 至 24 日）

本轮使用本地凭证和隔离代理执行真实生成，分项结果如下。这是调整为图片正式 ID 和 `veo-3.1` 视频产品家族入口之前的开发验收记录，不代表更名后已经重测，也不证明已确认视频底层变体。上面的 SDK 示例说明接口用法；新入口的恢复实测见后续小节，不能把下表当作 `veo-3.1` 入口的完整真实验收。

| 用例 | 真实上游结果 |
| --- | --- |
| Chat 图片普通响应 | 生成、下载及返回成功 |
| Chat 视频 SSE | 生成、下载及返回成功 |
| Gemini 图片 SSE | 生成、下载及返回成功 |
| Gemini 视频普通响应 | 生成、下载及返回成功 |
| 图片专用接口 | 生成、下载及返回成功 |
| OpenAI 专用异步视频 | 任务及人工复测均失败，未完成真实成功生成与下载验收；共享执行路径的 Gemini 复测确认上游 429 |
| Gemini 原生异步视频 | 上游返回 429，未完成真实成功生成与下载验收 |

脱敏分项报告保存在本轮证据目录 `enterprise-media-proxy-e2e-20261023`。离线 SDK 回放验证协议转换、任务查询和缓存文件下载，不能代替该阶段两项真实上游成功验收；不将上游 429 或模拟成功记为真实通过。历史直接调用的视频成功也不替代新增异步入口的完整真实验收。

### 视频 429 的日额度排查（2026-10-24）

后续排查已直接读取 Cloud Monitoring `timeSeries.list` 并取得 HTTP 200，范围是当前本地凭证实际绑定的项目及 `global` 区域。先前 `metricDescriptors.list` 的 403 只说明该次描述符枚举失败，不代表 `timeSeries.list` 同样不可读；两种请求结果分别保留。

| 监控项 | 本次直接读取结果 |
| --- | --- |
| limit 指标 | `discoveryengine.googleapis.com/quota/video_gen_numbers_tier_enterprise_regional/limit` |
| `limit_name` | `VideoGenNumbersTierEntPerDayPerProjectRegional`，明确为项目/区域的视频每日次数额度 |
| 最新 limit | GAUGE 值为 2 |
| 同一 Pacific 日的 usage | 观测到 3 个值均为 1 的计数点，合计 3 |
| 同日 exceeded | 合计 6；最近的 `06:29`、`06:30 UTC` 数据点与刚才两次失败吻合 |

结合每日次数限制的明确名称、实际 limit/usage 和对应时间的 exceeded 计数，本次 429 已确认为绑定项目/区域的视频日次数额度耗尽，不仅依据 `RATE_LIMIT_EXCEEDED_VIDEO_GENERATION` 的错误名称判断。观测到 usage 为 3、上限为 2；两者出现这一关系的原因尚未确证，按原始观测分别记录，不猜测原因。用户 Business 界面截图的 `150 videos per day` 不能代替该绑定项目/区域的实际监控上限，也不据此把当前可用次数改算为 150。

按 [Google 每日额度规则](https://docs.cloud.google.com/gemini/enterprise/docs/quotas-and-overages)，每日额度在太平洋时间午夜重置；本次对应 `2026-10-24 07:00 UTC`，即北京时间 `2026-10-24 15:00`。原计划在 `07:03 UTC`（北京时间 `15:03`）仅执行一次关闭自动重试的 Gemini 原生 SDK 受控验证；该请求已于 `07:03:37 UTC` 提交，生成、下载、播放及用量检查的实际结果分项记录如下。

### Veo 3.1 日额度重置后的原生恢复实测（2026-10-24）

本轮通过 Google Gen AI SDK 2.32.0，以 `veo-3.1` 调用原生 `predictLongRunning`，仅提交一次生成，没有自动重试或额外生成。新的 `native_async.mp4` 已生成、下载并完整播放，核心媒体流程通过；脚本最后的用量次数断言失败，统计问题及修复验证范围在本节单独保留。

| 验证项 | 实际结果 |
| --- | --- |
| 提交时间 | `2026-10-24 07:03:37 UTC`，北京时间 `15:03:37` |
| operation 查询 | 模型内路径与全局 operation 路径返回一致结果 |
| 文件下载 | SDK 两种下载入口及 4 个 HTTP 下载路径均返回 200，包括 `/download/v1beta/files/{id}:download` 和 `/v1beta/files/{id}:download`；字节及 SHA256 全部一致 |
| 身份隔离 | 其他请求身份访问返回 404 |
| 结果文件 | `native_async.mp4`，2,440,685 字节 |
| SHA256 | `3090fd018302e315301edbfca4e577902c2539eb307b2f6ff24083f7eb65d22d` |
| 完整播放 | 首次 `07:09:23` 至 `07:09:31 UTC`；1280×720、8 秒、192 帧、丢帧 0；同文件 `07:15:34` 至 `07:15:42 UTC` 复核播放通过，呈现丢帧 2；两次均 `ended=true`、`played=[0,8]`，无解码错误 |

`07:12:54 UTC` 的新 Pacific 日监控读取仍显示 limit 为 2、usage 为 1；新日 exceeded 没有返回时间序列，此项按未返回记录，不能填成已确认的 0。本次请求成功及新日 usage 为 1 证明此前耗尽的日额度已恢复可用；这不等同于代理内部成功用量已记录一次。

原始 `native_async-report.json` 在媒体下载完成后的最后一个用量次数断言失败，保持原文件及失败结论。定位到后台 Gin 快照没有 `Writer`，使 `usage.resolveSuccess` 发生 nil pointer，导致该次成功用量丢失。产品代码已增加空 `Writer` 检查，新增 6 个回归用例、usage 包测试和服务编译通过；全量 Go 回归中仅 pluginhost 的注册等待出现 5 秒超时，随后该整包隔离补跑通过，首次失败记录保留。统计修复只做了离线回归，没有再次发起真实生成，因此不能宣称这次真实用量已成功记录一次，也不把原始脚本写为全项通过。

本机证据目录为 `enterprise-video-429-recovery-20261024`：[MEDIA_ACCEPTANCE.json](C:/Users/lywx2/.codex/visualizations/2026/10/23/01a1692d-dcd4-7e81-ba37-0a8773960b30/enterprise-video-429-recovery-20261024/MEDIA_ACCEPTANCE.json) 记录核心媒体验收与统计瑕疵，[PLAYBACK_VERIFICATION-first.json](C:/Users/lywx2/.codex/visualizations/2026/10/23/01a1692d-dcd4-7e81-ba37-0a8773960b30/enterprise-video-429-recovery-20261024/PLAYBACK_VERIFICATION-first.json) 和 [PLAYBACK_VERIFICATION.json](C:/Users/lywx2/.codex/visualizations/2026/10/23/01a1692d-dcd4-7e81-ba37-0a8773960b30/enterprise-video-429-recovery-20261024/PLAYBACK_VERIFICATION.json) 分别保留两次完整播放证据；[最终验收截图](C:/Users/lywx2/.codex/visualizations/2026/10/23/01a1692d-dcd4-7e81-ba37-0a8773960b30/enterprise-video-429-recovery-20261024/playback-verification-final.jpg) 显示复核播放及统计修复说明。[原始失败报告 native_async-report.json](C:/Users/lywx2/.codex/visualizations/2026/10/23/01a1692d-dcd4-7e81-ba37-0a8773960b30/enterprise-video-429-recovery-20261024/native_async-report.json) 未改写。

## 工具、历史和输出边界

每次请求使用完整文本历史和新的上游会话，宿主会话亲和不会作为 Google session。系统指令和角色历史转换为有角色标记的文本上下文。

| 输入 | 行为 |
| --- | --- |
| Gemini `googleSearch` / `google_search` | 设置 `toolsSpec.webGroundingSpec` |
| Gemini `urlContext` / `url_context` | 开启 web grounding，并增加实际读取指定 URL 的指令 |
| Gemini `codeExecution` / `code_execution` | 增加 Python 执行指令；只报告上游真实执行事件，不保证执行 |
| OpenAI `tools:[{"google_search":{}},{"url_context":{}}]` | 相同工具归并；兼容 camelCase、`code_execution` |
| OpenAI `gemini_tools`、`web_search_options` | 在本 provider 内兼容旧扩展；不会修改其他 provider |
| OpenAI `tools[].type:function`、`functions`、函数选择与并行调用控制 | 当前接口不支持客户端函数；按用户确认的策略忽略并提示，继续文本请求 |
| Gemini `functionDeclarations` / `function_declarations`、`functionCallingConfig` | 忽略函数声明及控制，保留同一对象中的受支持内置工具 |
| 历史 `tool_calls`、`function_call`、tool/function 消息、`functionCall` / `functionResponse` | 保留为明确标注的客户端历史文本；不作为本代理新执行的动作，不返回新函数调用 |
| Gemini `application/json` | 提示约束，加最终合法 JSON 校验 |
| OpenAI `response_format:{"type":"json_object"}` | 提示约束，加顶层 JSON 对象校验 |

同一对象不能同时提供某个 camelCase/snake_case 别名。工具字段类型错误、无效历史结构，以及输入图片、音频、文件等不能保真转换的内容继续返回 400。客户端自定义函数按 2026-10-24 用户明确选择的忽略策略处理，具体边界见下文。对文本模型，普通可选参数如 sampling、token 限额、thinking 或 JSON schema 可被忽略，不宣称 schema 强制约束；媒体模式使用上节的严格参数限制。忽略的字段名记入诊断日志和 `X-CLIProxyAPI-Ignored-Parameters` 响应头；经公开 HTTP 接口获取此头需要启用宿主 `requests.passthrough-headers`。忽略客户端函数时另记不含函数定义或参数值的服务端警告；部署不会为显示提示而修改现有透传配置。

Gemini 输出保留文本、真实 executableCode、codeExecutionResult、grounding parts/metadata；OpenAI 输出正文、URL annotations，并将代码及结果呈现为文本块。上游 grounding 的 UTF-8 字节 offset 在 Gemini 中保留字节单位，在 OpenAI 中转换为 Unicode 字符索引，包含中文和 emoji 的回归测试。不生成上游没有提供的 function call、searchQueries、URL 抓取状态或隐藏思考/缓存计数，默认过滤 thought 正文，始终过滤 assistToken 和 sessionInfo。显式思考返回的已验证范围见下文“Enterprise 思考配置增量验证”。输入与可见输出的本地 Token 统计另见下节；计数不可用时仍发布宿主请求数。

JSON 请求的流式输出先完整缓冲并校验，再发出可见内容。最终 JSON 输出仅包含通过校验的 JSON 文本，不夹带中间代码块或引用；需要观察代码、执行结果和引用时使用普通文本输出。

只有 `SUCCEEDED` 是成功终态。HTTP 200 内的 error、FAILED、SKIPPED、错误尾部、缺终态的 EOF 都作为错误处理。开始生成或工具活动后的失败（即使只有被过滤的 thought）不会跨账号重放。第一个可见块之前不会发送纯 role 块，已输出内容后的错误按宿主流式错误机制终止。同步内容请求的客户端取消会释放上游；已创建的专用视频任务按前述后台生命周期继续执行。

模型推理和模型发现不设置连接后的总、读或响应头超时，受调用 context 取消控制。OAuth 凭据获取可使用短期超时。推理使用宿主临时/账号/全局代理顺序；刷新只使用账号/全局代理，不继承执行级临时代理。认证仅发送到固定的官方 HTTPS origin，不自动跟随认证请求的重定向；媒体下载仅在验证前述同源路径和文件绑定后显式请求跳转目标。

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

规则之后只做只读校验、序列化和传输封装。删除的字段不会被恢复，override 不会被内建逻辑覆盖。文本模型删除必需的 `generationSpec.modelId` 或产生无效 JSON/字段会明确报错，不自动修复。媒体模型不发送 `generationSpec`，只允许相应的空 `toolsSpec.imageGenerationSpec` 或 `videoGenerationSpec`；删除必需媒体工具、改为另一媒体工具或注入 `generationSpec` 同样报错。禁止通过规则注入可复用的上游 session/assist token。

### 最终请求的严格支持子集

字段校验按文本、图片、视频模式分别生效；文本模型中 Argon 沿用 Pro 的同一规则。它检查最终的 `streamAssist` 请求；客户端的 Gemini `generateContent` 参数需先经过上述协议转换。以下是本代理支持的文本与 grounding 子集，媒体模式的必需工具与参数限制见上节，均不代表完整 Google API 能力。文本字段依据 [Google streamAssist 定义](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist) 约束。

| 最终字段 | 允许的类型或取值 |
| --- | --- |
| `query.text`、`generationSpec.modelId` | 必需的非空字符串；不再额外施加本地 1 MiB 文本上限，实际容量受上游与模型上下文约束 |
| `toolsSpec.webGroundingSpec` | 空对象 |
| `toolsSpec.vertexAiSearchSpec.disabled`、`actionSpec.actionDisabled` | 布尔值 |
| `toolsSpec.vertexAiSearchSpec.filter` | 字符串 |
| `userMetadata.timeZone`、`userMetadata.preferredLanguageCode` | 字符串；语义见 [Google 用户元数据定义](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/AssistUserMetadata) |
| `isSessionLess`、`googleSearchGroundingEnabled`、`webGroundingEnabled` | 布尔值；后两项为 Google 已弃用字段，新规则优先使用 `toolsSpec.webGroundingSpec` |
| `languageCode` | 字符串；Google 已弃用，新规则优先使用 `userMetadata.preferredLanguageCode` |
| `answerGenerationMode` | `ANSWER_GENERATION_MODE_UNSPECIFIED` 或 `NORMAL` |
| `assistSkippingMode` | `ASSIST_SKIPPING_MODE_UNSPECIFIED` 或 `REQUEST_ASSIST` |

这些字段的父对象仅接受表内列出的子字段；`toolsSpec` 及其他可选父对象可以省略。未知字段、错误类型、无效枚举、重复 JSON 键、无效 JSON/UTF-8 和超过 64 层的嵌套在发送 Google 请求前返回 400。`null` 不作为删除字段的替代方式。校验不改写请求，也不恢复被规则删除的字段。IANA 时区、搜索过滤表达式等服务语义，以及模型和资源权限仍由 Google 判断；本地校验不能保证所有合法类型的值都会被 Google 接受。

本代理不接受调用方注入 `session`、`sessionInfo`、`assistToken`、`fileIds`、浏览器上下文、agents 及 `RESEARCH` / `AGENT` 等超出该子集的最终字段或模式。文本模式还拒绝媒体工具和媒体输出，媒体工具仅由已确认并发布的相应媒体模型选择。Google API 支持某能力不等于本 provider 已支持对应输入和输出。

原生 Gemini 的 `generationConfig`、`systemInstruction`、`contents` 不属于最终 `streamAssist` 请求。为原生 Gemini 模型设置的规则必须声明 `protocol: gemini`；Enterprise 的规则声明 `protocol: gemini-enterprise`。只匹配同名模型且省略 `protocol` 的规则会同时匹配其他协议，可能再次把原生字段注入 Enterprise；现在这类不兼容最终字段会被本地拦截，不能靠删除校验来规避。

### 明确且不暴露渠道或原始正文的错误

公开模型接口的普通和流式错误使用通用提示与错误码，不在 `error.message` 或 `error.code` 中标识 Gemini Enterprise、Google、`streamAssist`、渠道协议或上游地址。渠道名称只用于服务端诊断；本节文档中的名称用于说明实现，不会作为响应前缀发送给调用方。本地字段错误使用 `error.type=invalid_request_error`、`error.code=invalid_field`，`error.param` 指向安全的字段路径，`error.message` 说明原因；由最终 Payload 产生的错误仅提示检查 `server request payload configuration`，不向客户端披露规则协议。示例：

```json
{"error":{"type":"invalid_request_error","code":"invalid_field","param":"generationConfig","message":"generationConfig is not supported by the request schema. Check the server request payload configuration."}}
```

Google 非成功 HTTP 响应只返回本地构造的通用提示，不回显上游原始 message、字段值、资源路径、Token 或会话信息。读取的错误正文有大小上限；可信的 `google.rpc.BadRequest` 字段路径经过限制后可用于 `param`。仅凭其他 provider 的错误文本或伪造同名 JSON 标记，不能启用本渠道的详细提示；历史带 `gemini_enterprise_` 前缀的错误标记也不应穿透公共错误处理。

| Google HTTP 状态 | 对外 `error.code` |
| --- | --- |
| 400 / 422 | `upstream_invalid_request`；有明确模型权限证据时使用 `model_access_denied` |
| 401 | `upstream_authentication_required` |
| 403 | `permission_denied` |
| 404 | `resource_not_found` |
| 429 | `rate_limit_exceeded` |
| 其他非成功状态 | `upstream_error` |

HTTP 400 本身不能证明没有模型权限；没有明确证据时保留参数或可用性提示。Argon 应用相同的字段校验和错误分类；当前账号的 Argon 权限限制仍是独立验收条件，不能把 Pro 的成功、目录包含 Argon 或离线夹具通过记为该账号的真实 Argon 验收成功。历史授权、Windows 策略拦截及各阶段测试结果继续按原记录保留。

### 字段兼容回归记录（2026-10-23，历史发布阶段）

以下保留此前字段校验发布阶段的测试结论；后续 new-api 历史兼容与公开错误去渠道标识调整不改变该阶段的成功、失败或 Windows 策略拦截记录。

本轮公开接口夹具覆盖 7 个模型的 OpenAI/Gemini 普通与流式请求：168 组非法最终 Payload、84 组媒体或函数输入均返回明确字段错误且不调用上游；28 组正常可选参数请求通过。执行器另覆盖未来模型 ID、Payload 单次应用、覆盖值保留、过滤不恢复，以及有界错误读取、关闭和取消；错误处理覆盖安全提示来源、恶意包装和敏感值隐藏。

`go build -o <artifact>/cli-proxy-api.exe ./cmd/server` 通过。离线双 SDK 验收使用 OpenAI 3.36.1 与 Google GenAI 2.32.0，9 项功能通过；首次与全量编译并行时遇到测试进程收尾及日志文件占用，保留首次记录，单独复核后正常退出并通过，没有修改系统策略或测试脚本。

`go test ./... -count=1` 本轮 105 个测试包通过；`internal/api`、`internal/auth/kimi`、`internal/misc`、`internal/pluginhost`、`internal/translator` 的测试二进制被 Windows Application Control 拦截，不能记为全量通过。本轮涉及的 Enterprise translator、executor、公共 handlers、管理 handlers、auth manager 及跨模块 `test` 包均已执行通过，历史 Windows 拦截记录保持原状。

### new-api 多轮历史兼容（2026-10-23）

new-api 启用相应的 Gemini 思考签名选项时，会给助手历史的首个文本 part 添加 `thoughtSignature`，包括 `context_engineering_is_the_way_to_go` 占位值。此前本代理将文本附带的元数据一并判为非文本，能复现 `contents[1].parts[0] supports text parts only`。这里的复现依据是 [new-api 固定版本的签名转换逻辑](https://github.com/QuantumNous/new-api/blob/33c61a24234aa20cb10e8a644304322c476361de/relaykit/relayconvert/internal/shared/gemini/request.go) 和本机合成请求；未采集用户的原始请求正文，不能据此断言其具体开关状态。

- Gemini 文本 part 接受 `thoughtSignature` / `thought_signature` 字符串或 `null`，以及布尔或 `null` 的 `thought`。签名属于原生 Gemini 的不透明状态，字段名列入忽略参数提示，值不写入 Enterprise 请求、上下文或诊断日志。模型历史中 `thought: true` 的隐藏文本不会混入可见对话；用户及系统输入不能标记为隐藏思考。
- 本代理返回的 `executableCode` / `codeExecutionResult` 可作为可见的模型历史回传，转换为标注为先前代码及执行结果的文本上下文；不触发本地执行，也不将自定义 `functionCall` / `functionResponse` 当作内置执行结果。非空媒体和自定义函数内容继续返回明确的具体字段路径；已知可选字段为 `null` 时按缺省处理。
- 该阶段取消原先固定的 256 条数组限制，仍保留规范化历史的 1 MiB 总大小限制和 JSON 深度校验。不会截断或静默丢弃超过 256 条的历史；数组类型错误和空数组分别返回 `must be an array`、`must contain at least one entry`。后续 2026-10-24 调整另见下文，原阶段记录保留。
- 执行器在上游调用前的输入或最终 Payload 校验失败时记录 `provider`、`stage`、`code`、`field`，便于定位哪种字段不兼容；不记录正文、签名、Token 或字段值。Payload 规则仍在内建历史转换完成后仅应用一次，后续校验不会恢复被过滤的字段或覆盖配置值。
- 字段错误不再带 `Gemini Enterprise:` 前缀，错误码不再包含 `gemini_enterprise_`；例如 `contents` 类型错误会提示 `contents must be an array.`。该阶段的文本大小错误随后由 2026-10-24 调整移除。服务端日志仍可按 `provider` 和校验阶段排查。

公开错误去渠道标识调整前，本地历史兼容回归覆盖全部 7 个渠道模型的文本签名历史、513 条历史的双协议普通及流式入口、原生代码响应回传，以及错误内容拒绝和最终 Payload 顺序。离线 OpenAI 3.36.1 / Google GenAI 2.32.0 的 12 项 SDK 验收通过，其中新增真实 SDK 的签名、代码结果回传和长历史测试；这些使用本机合成上游，不构成 Google 或 Argon 权限验收。该阶段全量回归中 `internal/githubauth`、`internal/translator/gemini/interactions`、`sdk/cliproxy/executionregistry` 被 Windows Application Control 拦截，保留记录且未更改系统策略；此前各轮拦截记录继续保留。

历史热修复提交 `8d168b45cfa3acff99a79c50d4ddfdd32fb6cc14` 已于北京时间 `2026-10-24 00:06` 部署。线上复核完成 6 个可用模型的 8 次真实生成调用及全部 7 个模型的 14 项公开错误检查，均通过；其中 `gemini-3.1-pro-preview` 的 513 条历史，以及 `gemini-3.1-pro-preview`、`gemini-2.5-pro` 的 SSE 请求通过。真实调用已核对本渠道凭证的用量记录，201 条既有用量历史恢复并核对保留。未对 Argon 发起真实生成调用；公开错误检查在本地校验阶段拒绝，不代表 Argon 权限验收。该次热修复只涉及后端协议转换、错误处理和测试，未改变管理前端接口契约或页面逻辑，发布时保留原静态页面。脱敏发布与线上核对证据见任务目录 `enterprise-newapi-history-20261023/FINAL_REPORT.json`。

将该热修复与已有共享额度功能合并后，已另外通过相关 7 个 Go 测试包、离线双 SDK 的 12 项验收及服务构建。共享额度夹具的旧错误码断言已同步为通用错误码，并补充公开错误不暴露渠道的检查。首轮全量日志仍保留 5 个测试包的 Windows Application Control 拦截及旧断言造成的 4 例失败；其中本渠道 translator 和 Gemini handlers 已在相关回归中补跑通过，`internal/auth/claude`、`internal/cmd`、`internal/translator/gemini/interactions` 本轮尚不能记为通过。各阶段失败和前文历史拦截记录继续保留，未修改系统策略。本轮证据单独保存在任务目录 `enterprise-merge-main-20261024`。合并沿用前端 `110c204bd2055ccb165595e06e18d27ca82cd16d` 对应的共享额度页面产物，一致性证据见 [共享额度验收记录](docs/enterprise-shared-quota-validation.md)，无需为此后端合并重新构建前端。

### 不受支持的客户端函数工具（2026-10-24）

本次使用已保存的同一账号、资源和 Token 直接核对 Google：有效文本 `streamAssist` 请求返回 200；顶层添加 `tools` 返回 `400 INVALID_ARGUMENT`，明确提示找不到 `tools` 字段；添加 `toolsSpec.functionDeclarations` 也返回 400，明确提示找不到该字段。[官方 `ToolsSpec` 定义](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist#toolsspec) 提供工具注册表和内置能力，并非按请求注入客户端函数声明。另一个官方文档中的不透明 `streamGenerateContent` 方法在当前资源探测返回 `404 Method not found`，不能据此声称所有 Google 产品均不支持函数调用，也不能将其直接作为当前接入的替代路径。

用户确认采用“忽略不支持的函数工具字段、继续普通文本请求、保留忽略提示”，而非完整函数协议桥接。OpenAI 现代函数工具及旧式 `functions`、`function_call`、`tool_choice` 的 required/指定函数、`parallel_tool_calls`，以及 Gemini 的函数声明和调用控制均不进入上游业务字段或可执行提示。忽略的是原函数协议语义；即使客户端指定 required/ANY，也返回普通回答而不生成 `tool_calls`。声明正文、说明和默认参数值不写入提示或忽略字段响应头。混合对象中的 Google Search、URL context、code execution 提示仍按原规则处理。

已存在的函数调用、参数、关联 ID 和客户端提供的执行结果转换成标注为历史的文本，保留在完整会话上下文中；不执行函数、不伪造调用、不把历史结果说成本代理执行结果。没有函数能力却需要新外部动作时，提示上游明确说明无法执行。JSON 输出约束、普通与流式响应、所有已发现模型及 Argon 的转换规则保持一致；这不是 Argon 账号权限验收。无效类型、错误角色或损坏的历史结构仍返回具体字段错误。

与其他内建转换一样，这些处理均在最终 Payload 规则之前完成。规则只应用一次，之后不会恢复已过滤字段，也不会重写配置覆盖值。此前函数字段拒绝、各轮测试失败与 Windows Application Control 拦截属于历史阶段记录，保留原结论，不以本次忽略策略改写历史。

本轮相关 7 个 Go 测试包、服务构建及 OpenAI 3.36.1 / Google GenAI 2.32.0 的 22 项离线 SDK 验收通过。新增公开入口覆盖全部 7 个模型、双协议普通与 SSE 的 84 组声明忽略、历史保留和内置工具混合请求，以及 12 组最终 Payload 和 JSON 契约检查；历史长会话回归继续通过。全量 104 个测试包通过，5 个包被 Windows Application Control 阻止启动，没有已执行断言失败；初次 translator 二进制拦截和随后补跑通过的记录分别保留。证据位于本轮任务目录 `enterprise-function-ignore-release-20261024`，未修改或绕过系统策略。

### new-api 长会话与末尾续写（2026-10-24）

`contents must end with a nonempty user text turn` 和 `body text history exceeds 1 MiB` 均来自代理本地兼容层。现移除“必须以非空用户轮次结尾”的假设及规范化历史、最终 `query.text` 的两处 1 MiB 校验，不新增替代性的固定条数或字节上限，也不截断、压缩或摘要历史。系统指令、全部可见文本和空占位轮次按原有转换规则完整保留；签名和隐藏思考仍按前述规则处理。

最后一个非空可见轮次为 assistant/model 时，提示上游继续该轮次，只返回新增内容；为空的尾部占位不改变判定，也不会通过追加虚构 user 消息修复请求。OpenAI assistant 的 `null` 或缺省 `content`、Gemini model 的空 `parts` 可作为占位；字段类型错误、整段请求没有非空文本或系统指令、真实媒体及自定义函数仍返回通用字段错误。OpenAI 仅包含非空系统指令的请求可以生成响应。

该渠道通过 [streamAssist 的 `query` 字段](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist) 携带请求中的完整可见文本。当前实现不再添加代理编写的提示说明或对话 JSON 包装：单条文本原样写入 `query.text`，多条文本按请求顺序用换行连接；Gemini 的 `systemInstruction` 文本放在正文之前。历史代码和函数结果仍保留其业务内容，但不附加代理说明。角色边界不再编码进提示文本，不能据此认定原生角色优先级或解码器预填充语义。JSON 模式仍校验完整、有效的 JSON；需要 JSON 输出时，调用方应在自己的提示中说明。取消本地限制不代表上游或模型上下文无限，实际超限错误仍按安全错误规则返回。Argon 与其他模型使用相同规则，账号权限验收仍单独判断。思考参数、媒体路径和最终文本的 Token 统计维持现有管线。

Payload 规则仍是最后一次语义变更：完整历史转换之后仅应用一次，随后只读校验，不恢复过滤字段、不重写覆盖值。回归覆盖超过 1 MiB 的 514 轮历史、直到 4 MiB 的文本、末尾 assistant/model、空占位、双协议普通与流式入口及最终配置保留；逐项比较转发的角色、顺序和文本。所有历史 Windows 策略拦截与失败记录继续保留。

本轮已通过上述新用例、全部 7 个模型的 JSON 续写契约检查、服务构建，以及离线 OpenAI 3.36.1 / Google GenAI 2.32.0 的 16 项 SDK 验收。相关的 translator、executor/helps、公共 handlers、Gemini handlers、OpenAI handlers 与跨模块 `test` 包已执行通过；executor 包的独立测试二进制仍被 Windows Application Control 阻止启动。全量回归 91 个测试包通过、18 个包被同一策略拦截，没有已执行测试的断言失败；首次日志和补跑记录单独保存在任务目录 `enterprise-long-history-continuation-20261024`，不记为全量通过，也未修改或绕过系统安全策略。

## 本地 Token 统计与缺失分项（2026-10-23）

Enterprise 上游没有提供本次执行的完整 usage。当前实现使用随程序内嵌的 Gemma 3 SentencePiece 模型，对最终发送的 `query.text` 和合并后的可见输出进行本地分词。模型来源及 SHA256 与 [Google Go Gen AI SDK 的本地 tokenizer 配置](https://github.com/googleapis/go-genai/blob/main/tokenizer/tokenizer.go) 一致，固定在 `google/gemma_pytorch` 的提交 `014acb7ac4563a5f77c76d7ff98f31b568c16508`，解压后 SHA256 为 `1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c`。来源和 Apache 2.0 许可随模型保留，运行时不下载模型或调用远程计数接口。

这些数值是本机对可见业务文本的计数，不能视为 Argon 的官方 tokenizer、上游实际消耗或 Google 账单。Google SDK 的模型映射没有为 Argon 提供此保证；上游隐藏提示、思考和缓存也不在本机可观测范围内。

- 输入在所有内建转换和用户 payload 规则完成后读取，不修改最终请求。输出从合并后的文本、代码和执行结果 parts 读取，在 JSON 清理及 OpenAI Markdown 呈现之前计数，因此文本分片不改变结果，也不会把呈现包装重复计入。
- 媒体结果只统计可见业务文本；文件二进制、Base64、下载内容及任务轮询不作为文本 Token 或额外生成用量。
- 普通 OpenAI 响应填充已知的 `usage.prompt_tokens`、`completion_tokens` 和 `total_tokens`；SSE 仅在请求 `stream_options.include_usage: true` 时追加 `choices: []` 的 usage 尾包。Gemini 响应及正常 SSE 终包填充相应的 `usageMetadata` 字段。失败流不发送成功终态或成功 usage 尾包。
- 隐藏思考、缓存读取和缓存创建保持未知，不用零代替。分词初始化或校验失败时，不能确定的输入、输出及总量不出现在协议 usage 中；宿主仍记录请求结果和耗时。连接前失败没有可用计数；生成中途失败只能记录已观察到的业务内容。
- 宿主 `TokenPresence` 按字段保留可用性，缺省为 `nil` 的旧生产者沿用原有行为。管理用量及快照导入/导出把未知字段序列化为 `null`，保留明确的零；有显式可用性标记时，不从分项重算未知或明确为零的总量。快照不会共享可变的可用性指针，去重区分未知值与零值。
- 队列及插件适配保留分项可用性。缺少思考或缓存信息时，规范化的 v2 细分标为 `unclassified`，不宣称完整分解。前端明细、汇总和分类图按已有计数展示；费用按配置单价和可用分项计算，也不能据此确定真实账单。JSON 明细导出保留 `null`，CSV 对应字段为空。

本地统计不开放 `countTokens` API，不更改目录声明或模型限制。前文及下文历史真实 Google/Argon 验收发生于该统计改动之前；这些历史记录保持原结论，不能证明新增计数等同真实上游用量。本轮回归及 Windows 策略限制另见 [前端与用量联调记录](GEMINI_ENTERPRISE_FRONTEND_CN.md)。

## 未支持与验收

仅限制本 provider：Responses、旧 Completions、Claude Messages、Interactions、countTokens、函数调用、Deep Research，以及媒体编辑、参考输入、视频延长、固定视频底层变体和未支持的生成控制参数不支持；同名模型可继续由支持这些操作的其他 provider 执行。图片生成使用 `gemini-3-pro-image`，视频使用 `veo-3.1` 产品家族入口并由 Enterprise 决定实际变体。Chat 入口自动转换 Responses body 的兼容分支也不开放给 Enterprise。

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

## 每日助手额度查询与前后端联调（2026-10-23）

新增 `GET /v8/management/quota/gemini-enterprise?auth_index=<credential-index>`，仅使用目标凭证保存的项目、区域、OAuth 绑定及代理读取额度，不继承其他账号或全局资源绑定。读取 `licenseConfigs.list` 后，按有效版本查询 Cloud Monitoring 的每日助手与搜索指标。除 `ACTIVE` 外，仍保留访问权限的 `DEACTIVATING` 宽限期订阅也参与版本识别，见 Google 的 [LicenseConfig 状态说明](https://docs.cloud.google.com/generative-ai-app-builder/docs/reference/rest/v1/projects.locations.licenseConfigs#State)。同版本的多个订阅不重复叠加限额，不支持的版本不推算额度。

上限使用最新 GAUGE，已上报用量累计当前 `America/Los_Angeles` 自然日内的全部 DELTA 分页和方法，并去除重复点。项目、区域及版本分别成池；剩余量为 `max(0, limit-used)` 的计算结果，不代表个人实时余额。缺少用量保持 `null`，明确上报的零保持数字零。分页失败、循环分页、数值溢出或 Monitoring 的 `executionErrors` / `unreachable` 部分结果不会产生剩余额度，参见 [Monitoring 返回契约](https://docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/ListTimeSeriesResponse)。每日重置按太平洋当地午夜计算，覆盖夏令时切换。

额度接口需要管理鉴权，返回 `Cache-Control: no-store`；可选读取失败使用 HTTP 200 的 `no_permission` 或 `unavailable` 状态，管理鉴权失败仍返回原有错误码。禁用、缺失或不属于该 provider 的凭证不访问上游。刷新仅在内存中取得本次查询 Token，不写回凭据，不改变路由健康状态。上游错误正文和 Token 不进入响应，授权头不跟随重定向，取消沿请求上下文传播。未新增 v0 额度路由，也未修改系统策略或 IAM。

本轮先修正了新增测试中配置夹具的编译写法，再完成三包完整回归：`internal/auth/geminienterprise`、`internal/api/handlers/management` 和 `internal/api` 共 735 项测试及子测试通过，专项 23 项通过，server 编译通过；没有受策略拦截的包。本次没有重跑 `go test ./...`。前端专项 31 项及 `bun run verify` 的 1,791 项测试、Lint、TypeScript 和生产构建通过。

隔离的真实 Go API 服务使用七条合成凭证及模拟 Google 上游，完成 17 项 HTTP 检查和 13 项浏览器检查。覆盖管理鉴权、Plus/Standard 独立额度池、宽限期订阅、未知与零、权限失败、服务未启用、分页部分失败、禁用凭证、筛选、手动刷新及退出会话。浏览器中 Plus 的合成用量由 6 刷新至 8、计算剩余由 9,994 更新至 9,992，Standard 保持上限 2,000、用量 1、剩余 1,999；这些是夹具值，不是生产账号观测。控制台无错误，全局配置及 ETag、凭证绑定/健康状态和七份合成文件内容不变。本轮没有使用真实 OAuth、发送 Google 请求或调用模型；午夜缓存和连接代际隔离由确定性单元测试覆盖。

前端提交为 `13ce4bfec84e7d4b7d4dcb6b53bf21e4ed8e3424`。使用固定环境 `VERSION=dev`、`BUILD_TIME=2026-10-23T10:34:00Z` 构建，单文件产物、后端 `static/management.html` 及实际 HTTP 正文均为 4,014,911 字节，SHA256 为 `6f1b1cb51320dd1436f8fcdc32ea944bd0934b5ed28bb499f622f1c0df6ca080`。从该提交按相同环境重新构建后，产物逐字节一致。本次变更不包含其他功能或滚动周额度展示。

脱敏证据目录标识为 `enterprise-quota-final-20261023`，包含各轮回归日志、`http-integration.json`、`browser-regression.json`、产物核对和 `quota-live-browser.jpg`。首轮测试夹具编译失败，以及独立联调夹具因缺少文件路径而未列出内存凭证的记录均保留；修正仅涉及本次测试夹具，没有为通过测试放宽产品校验。验收进程已退出，监听端口已关闭，两份临时二进制已删除。历史 Windows 4551 和各阶段全量受阻记录保持原文及原文件，不能用本轮成功取代历史结果。前后端分别提交，暂不推送。

## OAuth 切换与本机回调故障排查

- **切换新旧 client：** 修改 `oauth.providers.gemini-enterprise` 只影响之后的登录，不会重写已保存账号的 client/resource 绑定。新 client 必须重新授权；原 client 的 refresh token 不能替换为新 client 的凭据。同一账号不同 client 的记录按绑定哈希共存。不同 client 建议使用不同的 `client-secret-env` 名称，避免覆盖旧环境变量后让旧记录使用新 client 的 secret。
- **Testing 测试用户：** 对处于 Testing 状态的外部 OAuth 应用，若授权页提示应用正在测试或 `access_denied`，检查新 client 所属 Google Cloud 项目的 OAuth consent/Audience 设置，将需要授权的账号加入 Test users 后重试。旧项目的测试用户名单不会随下载的客户端 JSON 自动迁移。文档和日志不应记录实际个人账号名单。
- **Testing 续期边界：** 本次取得了 refresh token，没有等待其自然过期或触发真实轮换。Google 对 External + Testing 且请求超出基本身份 scopes 的授权，默认使 refresh token 在七天后过期；本 provider 请求的 `cloud-platform` 不属于基本身份例外，见 [Google token expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration)。Google Workspace 的 Trusted 管理状态可能覆盖这一默认限制，见 [OAuth app state overview](https://developers.google.com/identity/protocols/oauth2/production-readiness/overview)；本次未核验该管理状态，不能把收到 refresh token 描述为无限期自动续期保证。
- **代理浏览器访问 localhost 被拒绝：** 先确认登录进程仍在等待、端口未冲突，且浏览器与 CLI 位于同一台机器。可以在同机直连浏览器中，将当前回调地址的 `localhost` 临时替换成 `127.0.0.1`，保留端口、`/oauth-callback` 和完整原查询参数。该操作只改变本机浏览器到 loopback 监听器的访问方式，不改变 OAuth 流使用的已登记 redirect URI。不要把包含授权码的完整回调 URL 粘贴到聊天、文档或日志中。
- **跨机器访问：** `localhost` 和 `127.0.0.1` 都指向浏览器所在机器，不能用于访问另一台机器上的 CLI。远程场景应按前述方式建立安全的本地端口转发，并保持回调端口和登记地址一致。


## Assistant and Search 共享额度冷却（2026-10-23，本地开发）

此功能适用于普通 CLIProxyAPI **单服务实例**。额度展示与路由健康分开：Monitoring 显示零余额、数据延迟、`no_permission` 或查询失败均不会禁止生成；只有生成接口的结构化额度拒绝会建立共享限制。Overages 允许生成成功时不会冷却。CLIProxyAPIHome 不启用本地权威池；PostgreSQL 持久化不代表跨进程传播或互斥。

已确认的共享池按 provider、Google 项目编号、location、subscription edition 和 `assistant_search` 区分，Flash/Pro/Argon 及后续文字模型共用 gate；engine、assistant、模型、OAuth client 和 LicenseConfig ID 不分池。用户额度按同一项目/区域下已确认的 OAuth 用户隔离，技术项目分钟限流另设作用域。身份未确认时只影响当前凭证的全部文字模型，其他账户不受传播。

归属查询使用 OAuth userinfo、Cloud Resource Manager 项目规范名，以及 `projects/{number}/locations/{location}/userStores/default_user_store/userLicenses` 中该用户的已分配 `licenseConfig`，再读取对应订阅版本。推理参数或导入文件的任意 email 不能建立归属。缓存以资源绑定和凭证生命周期隔离，正常请求准备时至多每小时刷新；临时只读故障保留已验证缓存，明确撤销许可证会使其失效。可选权限包括 `discoveryengine.userStores.listUserLicenses`、对应 LicenseConfig 和项目的读取能力；缺少这些权限仍可正常推理，不修改 IAM。

错误解析只接受资源匹配的 `google.rpc.Status` / `ErrorInfo` / `QuotaFailure`，支持 `RetryInfo`、`Retry-After` 及明确 reset 时间。已支持的 Assistant/Search 指标为 `text_answer_gen_tier_{edition}_regional`，日/分钟周期须由结构化 quota unit 或 quota ID 说明；泛化的 daily/quota 描述不能建立日池。未知 429 保留原有短时退避，授权、网络和取消沿用各自处理。HTTP 非 2xx、200 内错误、JSON/NDJSON/SSE、分片与流尾共享分类；思考已经开始也不更换凭证重放，但确定的拒绝仍更新池。原始错误、assistToken、sessionInfo、HelpToken 和密钥不会写入池快照。

池状态为正常、冷却、待验证、验证中。优先使用 Google 明确时间；仅确定日额度且没有时间提示时估算 America/Los_Angeles 次日午夜，使用时区日历处理 DST。到期只允许一个正常用户请求验证，不额外探测 Google；完整终态成功才解除。网络或取消释放验证资格，旧成功、重置前旧拒绝和重绑前旧响应受 epoch/version fencing 限制。共享投影不替换独立模型/授权错误，也不会给兄弟凭证制造失败统计。

`disable-cooling` 沿用现有全局、provider、credential 优先级。凭证 opt-out 只绕过自身 gate；真实拒绝仍更新池，绕过成功不解除共享限制。原单凭证重置不清共享池。新增 v8 显式共享重试，要求当前 opaque pool ID 和版本；它转为待验证，不恢复 Google 额度、不清无关限制、不主动调用 Google。

开启 `routing.cooldown.save-cooldown-status` 时，文件后端使用独立的版本化 `shared-quota-state.sqs` 原子快照，原 `.cds` 格式不变；PostgreSQL 使用 `{cooldown_table}_shared` 独立表与事务版本检查。保存失败保留内存限制并记录安全日志，损坏或未来版本不会静默覆盖。关闭保存时使用内存状态。重启时过期/验证中状态回到待验证，新同池凭证继承限制，删除单个成员不解除其他成员的限制。

管理界面区分本凭证重置与共享重试，显示关联数量、全部文字模型、估算重试时间和待验证/验证中状态；支持六语言、暗色、窄屏与原生键盘 disclosure。新前端可降级到旧后端冷却字段；旧前端在冷却阶段仍看到 credential timer，无法理解零秒验证阶段时会按其原有逻辑显示未知状态，不会据此声称官方额度恢复。

本轮验收为脱敏离线 fixtures 和本机合成联调，**未取得真实 Google 共享日额度自然耗尽样本，未故意耗尽额度，未新增真实 Google/模型调用、IAM 写入、推送或部署**。测试结果及环境限制见 [本轮验收记录](docs/enterprise-shared-quota-validation.md)。历史 Windows 拦截证据保持原阶段记录。

依据：[Google quota/overages](https://docs.cloud.google.com/gemini/enterprise/docs/quotas-and-overages)、[UserLicense list](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1/projects.locations.userStores.userLicenses/list)、[UserLicense](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1/UserLicense)、[AIP-193](https://google.aip.dev/193)、[google.rpc error details](https://github.com/googleapis/googleapis/blob/master/google/rpc/error_details.proto)。

## Enterprise 思考配置增量验证（2026-10-24）

本节是独立于前述共享日额度验收的新一轮直连验证。使用用户指定项目的 OAuth 凭证，直接请求 `discoveryengine.googleapis.com` 的 `streamAssist`，未经过 new-api 或已部署代理。完整脱敏记录保存在本地 `temp/_enterprise-thinking-live-20261024/live-results.json`；不保存原始思考正文、OAuth 令牌、assistToken 或 sessionInfo。

Google 明确以 400 拒绝非法 `thinkingLevel` 枚举和非布尔 `includeThoughts`，说明 `generationSpec.thinkingConfig` 是实际识别的结构，而非未知字段被静默忽略。有效 HIGH/LOW 与 true/false 的组合请求可完成生成。首轮同题四组合的观察如下：

| thinkingLevel | includeThoughts | 首个答案正文（秒） | thought 标记片段的累计字符数 | 数独答案 |
| --- | --- | ---: | ---: | --- |
| LOW | false | 303.99 | 1,537 | 正确 |
| HIGH | false | 355.13 | 1,813 | 正确 |
| LOW | true | 432.49 | 31,940 | 正确 |
| HIGH | true | 326.85 | 37,672 | 正确 |

`includeThoughts=false` 仍有短状态片段，不能把这些片段的存在等同于模型思考内容的开关失效。true 样本可包含大段思考返回，也可能没有；客户端须允许缺省。第二轮出现了错误答案，因此不能把参数被接收、思考文本更多或 HTTP 200 当作正确性保证。Google 未返回实际生效档位、思考 token 或预算反馈；**HIGH/LOW 的字段接收已验证，内部推理深度和固定性能差异仍不能量化确认**。

两轮共 8 次生成均以 SUCCEEDED 结束，独立答案核验为 5 次正确、3 次错误。第二轮 LOW/false、HIGH/false、LOW/true、HIGH/true 的首正文分别为 447.42、434.83、410.26、479.87 秒，thought 累计字符数分别为 3,530、2,124、2,196、29,253；只有 HIGH/false 的数独答案正确。两轮 HIGH 的 true/false 对照都观察到明显的思考返回量差异，但 HIGH 相对 LOW 的延迟方向不固定。

当前适配范围限定为本轮验证的 `gemini-3.1-pro-preview`，支持 LOW/HIGH。其他 Enterprise 模型保持既有忽略行为，不依据目录可见或其他 Google API 的同名模型泛化能力。未验证的 MEDIUM、显式 `thinkingBudget` 等控制不作为本轮新增能力。

```json
{
  "contents": [{"role": "user", "parts": [{"text": "请解答这个问题。"}]}],
  "generationConfig": {
    "thinkingConfig": {"thinkingLevel": "HIGH", "includeThoughts": true}
  }
}
```

参数经统一 `ThinkingConfig` 和 `SummaryConfig` 处理，再由 Enterprise provider applier 写入 `generationSpec.thinkingConfig`。模型名后缀优先于请求正文，例如 `(low)` 覆盖正文 HIGH；最终 payload 规则最后执行，可覆盖或移除配置，之后不补回字段。camelCase、snake_case 及其已定义别名继续遵循冲突校验。

Gemini 响应以 `thought: true` 的 parts 表示显式请求的思考内容；OpenAI Chat 的对应字段为 `reasoning_content`。OpenAI 使用 `reasoning_effort`，并沿用项目“非 none effort 表示请求思考返回”的惯例；需要仅设 HIGH 而隐藏返回时，可传 `extra_body.google.thinking_config.include_thoughts: false`。缺省不增加思考返回，最终 payload 设置的 false 或过滤同样会阻止本地回显。

思考文本不混入可见答案、JSON 校验、来源字符索引或输出 token 计数。思考插入可见文本分片之间时，计数先合并可见文本；思考和缓存的真实用量仍未知，不补零、不用思考文本的本地分词数冒充上游消耗。失败、断流与取消继续沿用现有单次 usage 上报和错误终态。

## Flash 思考配置增量验证（2026-10-25）

本节扩展上一节 2026-10-24 阶段的模型范围。使用用户新提供的项目 `190565269829` 凭证，直接访问 Google Discovery Engine `streamAssist`，只测试 `gemini-3.8-flash`；未经过 new-api 或已部署代理。凭证只在测试进程内使用，没有写入本地认证目录、代码或报告。此前项目的四次 429 单独保留在旧证据目录，不计入本轮成功请求。

两轮同题四组合共 8 次生成均返回 HTTP 200，并以 `SUCCEEDED` 结束；另两次非法枚举、非布尔类型负对照均为 400，明确指向 `generation_spec.thinking_config` 下的对应字段。完整脱敏记录、统计和 Schema 摘录分别见本地 `temp/_enterprise-thinking-flash-20261025-resumed/` 下的 `live-results.json`、`summary.json`、`metrics.csv` 和 `public-discovery-thinking-schema.json`。

| 轮次 | thinkingLevel | includeThoughts | 首个答案正文（秒） | thought 标记片段累计字符数 | 数独答案 |
| --- | --- | --- | ---: | ---: | --- |
| 1 | LOW | false | 241.90 | 2,513 | 正确 |
| 1 | LOW | true | 247.44 | 3,449 | 正确 |
| 1 | HIGH | false | 150.83 | 2,216 | 正确 |
| 1 | HIGH | true | 181.43 | 2,224 | 正确 |
| 2 | LOW | false | 317.84 | 3,851 | 正确 |
| 2 | LOW | true | 178.51 | 2,103 | 正确 |
| 2 | HIGH | false | 207.86 | 1,979 | 正确 |
| 2 | HIGH | true | 233.47 | 1,945 | 正确 |

独立答案核验为 8 次正确、0 次错误。表中字符数为流中已观察片段的累计值，不代表思考 token、独立推理步骤或模型实际消耗。首字时间只作本机对照观察，不作为固定性能承诺。

已确认的是有效 HIGH/LOW 和布尔参数的接口接收及生成兼容性。Google 没有返回实际生效档位、思考预算或思考 token。本轮 true/false 返回量的差异不能单独证明服务端开关的语义效果，更不能从 HTTP 200、延迟或文本长度声称已验证内部推理深度。false 仍可能返回短 thought 状态内容；true 也不保证每次都有额外的大段思考正文。

Google 当前公开的 [StreamAssist REST 说明](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist) 及本轮保存的 Discovery Schema 在 `GenerationSpec` 中只公开 `modelId`，未公开这些思考字段。负对照和有效请求构成本项目的有限运行证据，不等同于 Google 对该内部字段的稳定兼容承诺，也不将 Agent Platform GenerateContent 等其他 API 的同名模型能力泛化为本渠道合同。

当前 Enterprise 适配范围为 `gemini-3.1-pro-preview` 和 `gemini-3.8-flash` 的 LOW/HIGH 参数传递与显式思考返回。Flash 复用现有统一 `ThinkingConfig` / `SummaryConfig` 和 Enterprise applier；Gemini camelCase/snake_case、OpenAI `reasoning_effort`、模型后缀优先级及最终 payload 规则顺序保持一致。上节原生请求示例选择 Flash 即可使用，不新增路由。MEDIUM、显式预算和未验证模型继续保持原有边界。

响应行为由最终 payload 中的可见性设置决定：默认及 false 在本地过滤 thought 标记内容，true 才转为 Gemini `thought: true` parts 或 OpenAI `reasoning_content`。这项本地显示控制由模拟上游回归验证，不表述为 Google 原始返回在 false 时完全不含 thought。思考不混入答案、JSON、来源索引或可见输出 token；未知思考和缓存用量仍不补零。

本轮回归扩展到两款模型，覆盖 HIGH/LOW、显式 true/false、双协议流式与非流式、JSON、后缀与别名、最终 payload 覆盖和过滤、非法输入的本地阻止、显式注册能力禁用，以及模型别名/前缀的能力元数据保留。执行结果和系统策略拦截记录分别保存在该证据目录，不把未启动的测试计作通过。保留其他未提交修改，暂不提交、推送或部署。

服务端编译通过。统一思考管线、执行器、辅助用量代码、注册表及 SDK 模型目录回归通过。`go test ./...` 最后一轮有 109 个测试包、19,236 项测试及子测试通过，没有断言失败；`internal/api/handlers/management` 和 `internal/translator/geminienterprise` 的测试程序仍被 Windows Application Control 阻止启动，因此全量结果不是通过。首轮曾出现 GPT-4o 插件流取消的失败状态断言，未修改其代码的后两轮均通过，首轮失败记录保留。本轮没有修改系统策略、签名、测试执行目录或缓存设置。
