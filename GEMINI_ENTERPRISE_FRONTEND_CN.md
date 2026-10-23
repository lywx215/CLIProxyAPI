# Gemini Enterprise 管理前端适配与验收

## 评估结论

原实现已覆盖 CLI 登录、账号存储、模型发现与双 SDK 请求，但管理前端没有 Gemini Enterprise 入口，v8 管理接口也不能启动该 provider 的授权。只增加前端卡片无法完成登录，必须同时补齐管理接口与凭证展示契约。

本次改动位于 CLIProxyAPI 与 Cli-Proxy-API-Management-Center 两个仓库。Gemini Enterprise 使用 OAuth 凭证管理，不添加到 API Key 提供商编辑器。配置面板提供入口，实际配置与登录统一放在 OAuth 登录页。

## 用户流程

1. 打开 **OAuth 登录 → Gemini Enterprise**。
2. OAuth 客户端 ID 和 Secret 环境变量名使用固定应用配置，不在授权表单显示。Secret 值仍由后端进程环境提供，不经过前端。
3. 手动填写引擎 ID。助手默认 `default_assistant`，可修改；留空时也使用默认值。Google 授权后，后端根据该账号可见的项目及手填引擎 ID 查询完整资源名称，识别项目、区域和集合。
4. 自动查询使用 `default_collection`，且仅在找到唯一可读的匹配引擎后保存账号。项目不可见、查询失败或匹配不唯一时不保存凭证；可开启“手动指定项目、区域和集合”，补填后重新授权。在 Google 客户端登记完全一致的 `http://localhost:8788/oauth-callback` 后发起授权。
5. 后端可在本机 loopback 接收回调。端口被占用时不会停止占用端口的进程，仍可使用手动回调；远程后端也可以粘贴完整回调 URL。
6. 手动回调须使用当前授权尝试的 state。前端拒绝错误来源、错误路径、重复 state 或不完整结果，后端再次验证 pending session 与 provider。
7. 可取消待完成的会话；取消成功（`cancelled: true`）后不会保存新凭证。持久化已开始时返回 `cancelled: false`，应继续查询实际结果；保存完成前状态仍为等待。成功后刷新认证文件列表，查看账号自己的资源绑定、访问令牌到期时间及刷新令牌状态。

授权表单通过启动接口提交本次会话的公开参数，不再先写全局配置。自动模式不继承旧账号的项目、区域和集合。切换连接、管理密钥或退出登录会废弃旧请求。已有账号保留自己的客户端和资源绑定；CLI 与不传新模式参数的管理调用仍使用全局默认绑定。

## v8 接口契约

| 操作 | 接口 | 说明 |
| --- | --- | --- |
| 读取配置 | `GET /v8/management/config` | 复用前端全量配置缓存 |
| 保存默认绑定 | `PATCH /v8/management/config/oauth/providers/gemini-enterprise` | 保留默认配置契约；当前授权表单不调用此接口 |
| 启动授权 | `GET /v8/management/oauth/auth-url?provider=gemini-enterprise` | 新表单传 `auto_discover_resources` 和本次公开参数；响应含 `resource_inputs_supported: true` |
| 提交回调 | `POST /v8/management/oauth/callback` | 复用 state/provider 校验与原子回调文件 |
| 查询状态 | `GET /v8/management/oauth/status?state=...` | 成功在凭证持久化之后报告 |
| 取消 | `DELETE /v8/management/oauth/session?state=...` | 复用现有会话取消契约 |
| 凭证列表 | `GET /v8/management/credentials` | 新增公开 `gemini_enterprise` 摘要，不包含 token 或 Secret |
| 手动刷新 | `POST /v8/management/credentials/refresh` | 复用现有 executor 刷新；缺少刷新令牌时提示重新授权 |

未新增或修改 v0 路由；新增凭证摘要仅在 v8 返回，既有 v0 凭证列表响应保持不变。CLI 与管理登录共享账号 ID 和刷新令牌保留规则，不能从其他账号、应用或资源绑定继承刷新令牌。

自动模式通过 Resource Manager 的 `projects:search` 及逐个候选资源的 `Engine.Get` 查询，不调用 `Engine.List` 或 `Assistant.List`。需要账号具有相应项目的可见性（`resourcemanager.projects.get`）、引擎读取权限（`discoveryengine.engines.get`），以及资源项目的配额使用权限（`serviceusage.services.use`）。助手 ID 采用用户输入或默认值，不声称已自动发现或验证助手。非默认集合、不可见项目或不支持的区域可使用手动模式。查询不修改 IAM，也不调用模型。

只读查询契约参考 Google 官方 [projects.search](https://docs.cloud.google.com/resource-manager/reference/rest/v3/projects/search) 和 [engines.get](https://docs.cloud.google.com/generative-ai-app-builder/docs/reference/rest/v1/projects.locations.collections.engines/get)。

凭证支持品牌筛选、按项目/引擎/助手搜索、详情、启停、下载及现有模型浏览。模型由后端实际发现，不在前端预填 Argon 或虚构目录。凭证卡现已通过 v8 额度接口读取绑定资源的每日助手与搜索额度。文案覆盖简体中文、繁体中文、英语、俄语、越南语和韩语。

### 每日助手与搜索额度（2026-10-23）

- 后端根据当前凭证的资源绑定读取 `licenseConfigs.list`，按有效订阅版本查询 Cloud Monitoring 的 `text_answer_gen_tier_…_regional/limit` 和 `/usage`；包括仍可访问的宽限期订阅，不依赖服务生产方的 Service Consumer Management API。
- 实际上限使用最新 GAUGE，已上报用量累计当前太平洋自然日的全部 DELTA 分页及方法。剩余量为上限减已上报用量，标明为计算值，并分别显示用量样本时间和查询时间。
- 版本、项目及区域分别成池，不叠加同版本多个订阅的重复限额。Google 没有返回用量时显示未知，不能视为零，也不能视为个人实时余额。其他功能及滚动周额度暂不在本卡片展示。
- 服务需启用 Discovery Engine 和 Cloud Monitoring API，账号需能读取订阅及监控数据。无权限时卡片提示检查 `roles/viewer` 或等效读取权限；服务未启用、分页失败或 Monitoring 返回部分结果时显示“额度暂不可用”。均不弹出原始错误、不标记凭证异常、不改变模型路由。
- 页面自动首次读取，支持独立刷新、请求去重、资源绑定和连接代际隔离，以及太平洋午夜后的重新查询；不持久化额度或 OAuth 密钥。每日重置使用 `America/Los_Angeles` 时区，支持夏令时。
- 后端三包完整回归共 735 项测试及子测试、专项 23 项和 server 编译通过；前端专项 31 项及 `bun run verify` 的 1,791 项测试、Lint、TypeScript 与单文件构建通过。初次新增 Go 测试的配置夹具编译错误已修正，失败日志保留。

前端提交 `13ce4bfec84e7d4b7d4dcb6b53bf21e4ed8e3424` 与隔离的真实 Go 后端完成 17 项 HTTP 和 13 项浏览器检查。七条合成凭证覆盖正常、未知、明确零、无权限、API 未启用、Monitoring 部分失败及禁用状态；刷新时 Plus 用量从合成的 6 变为 8，Standard 独立池不变，筛选清除后恢复所有状态。页面显示用量上报、重置和查询时间；控制台无错误，退出后回到未记住密钥的空登录表单。午夜重查及连接切换后的旧响应丢弃由确定性单元测试覆盖。

本轮只使用本机合成上游，不将夹具值描述为真实 Google 额度，也没有重新授权或模型调用。静态产物及实际 HTTP 正文一致，固定构建环境、产物哈希和证据目录见 [额度查询联调记录](GEMINI_ENTERPRISE_CN.md#每日助手额度查询与前后端联调2026-10-23)。此前各阶段 Windows 拦截记录保持保留，本轮相关三包通过不代表重新完成后端全量回归。

## 前期前端适配与联调记录

- 前端 `bun run verify`：1755 项测试通过，Lint、TypeScript 检查与单文件生产构建通过。
- 后端 `go test ./internal/api/handlers/management ./sdk/auth` 通过，`go build ./cmd/server` 通过。
- 新增测试覆盖 PKCE/固定回调、手动回调、取消期间不落盘、端口占用降级、共享账号绑定、错误脱敏、配置 ETag/连接切换、资源搜索、配置保留与六语言渲染。
- 浏览器已验证：配置保存与重新读取、表单校验、并发修改冲突/重载、Enterprise 筛选和资源详情、无刷新令牌提示。使用独立本机配置和禁用且不含凭证的测试记录，未使用真实 Google 凭证或发起模型请求。
- 后端全量测试首次受到沙箱的程序执行、局域网发现和临时 Git 夹具访问限制。正常用户环境复核后，`internal/discovery`、`internal/store`、`sdk/api/handlers/gemini`、`sdk/pluginhost` 均通过；`internal/cache` 测试程序仍被 Windows 应用控制策略阻止执行，不能宣称全量全部通过。
- 用户明确授权本机模拟会话后，浏览器已验证链接生成、取消后重开、旧 state 拦截、手动拒绝回调、拒绝后重试和 loopback 自动回调接收。自动回调显示 `Authorization received`，随后页面正确进入拒绝状态并结束等待；未打开 Google 页面，未创建真实凭证。成功交换与保存路径由模拟 HTTP 测试覆盖，本轮未重复真实 Google 授权。

正式发布前应组合这两个仓库的对应版本，将最终前端单文件产物按项目发布流程装配。以下另记本次后端独立复核。

## 管理接口及账号记录复核（2026-10-23）

本次独立提交限于后端管理接口、共享账号记录、相关回归与本文档。配套前端仅核对接口契约并运行现有 Enterprise 测试，不将前端工作区改动纳入后端提交。

- 管理授权复用配置中的客户端和资源绑定，使用 S256 PKCE、随机 state 与固定回调；端口占用时保留手动提交，支持并行会话且不停止端口原有进程。
- CLI 与管理授权共享账号记录构造；测试覆盖账号、客户端、Secret 环境变量名及五类资源字段的绑定隔离，以及新刷新令牌优先且不修改输入凭据。
- 保存与取消通过原子会话状态竞争，只有一方成功。新增实际保存阻塞/失败回归：保存尚未结束时查询返回等待、取消返回 `false`、重复回调被拒绝；最终按保存结果报告成功或脱敏错误。
- 复现并修复共享列表逻辑影响 v0 的问题；内存和磁盘列表的分页/非分页路径均验证 v8 公开摘要、v0 响应隔离，以及不泄露 Access Token、Refresh Token、ID Token、Secret 或 Secret 环境变量名。
- `go test -json -count=1 -timeout 180s ./internal/api/handlers/management ./sdk/auth ./internal/api` 通过。Enterprise 管理、账号绑定及实际路由鉴权用例在这三个包中连续运行二十轮通过；server 构建通过。
- 使用项目要求的 Bun `1.3.14` 运行 `bun test tests/geminiEnterprise.test.ts`，13 项通过、0 项失败，覆盖前后端字段、取消流程、配置保留及六语言呈现；本轮未重新运行前端全量 verify 或浏览器交互。
- 完整命令 `go test -json -count=1 -timeout 240s ./...` 返回非零：108 个包通过、34 个无测试包跳过，`internal/cache` 测试程序被 Windows Application Control 阻止执行，没有失败断言。对该包的标准独立复测仍被拦截，未修改或绕过系统策略，不能描述为本轮全量全绿。

本轮仅使用临时目录、合成凭据和模拟 Google HTTP 响应，没有新增真实 Google 授权、模型调用、部署或推送。其他 Free Tier 文档与本机忽略规则保留在本次提交之外。

## 手填引擎与自动资源识别复核（2026-10-23）

本节保留本轮审查开始前已有的独立任务记录；本轮重新执行的检查另记如下，不将历史真实只读结果描述成本轮重新授权或调用的结果。

- 默认表单仅显示引擎 ID 和可编辑的助手 ID。固定应用字段隐藏，项目、区域及集合在手动模式下显示。助手留空仍使用 `default_assistant`。
- 本次会话参数不写入全局配置。自动模式不发送旧配置的项目、区域和集合；发现失败或取消时不保存新凭证。旧版后端缺少能力标记时提示升级，并尽力取消已创建的会话；切换连接后不向新连接发送旧会话清理请求。
- 前端 `bun run verify`：1758 项测试通过，Lint、TypeScript 检查及单文件构建通过。测试覆盖自动模式参数、手动字段呈现、助手默认值、旧版后端能力校验及六语言渲染。
- 后端 `go test ./internal/auth/geminienterprise ./internal/config ./internal/api/handlers/management ./sdk/auth`、`go test ./internal/api ./sdk/cliproxy` 及 server 构建通过。模拟 HTTP 集成覆盖授权交换、账号识别、资源识别、成功保存和失败/取消不落盘。
- 独立本机后端的真实 v8 HTTP 接口已使用虚构客户端验证：启动响应包含资源输入能力标记，取消成功，全局绑定未改变，认证目录仍无凭证文件。没有打开 Google 页面。
- 浏览器已验证默认精简表单、空/有效引擎校验、助手留空和自定义、手动模式字段及键盘操作。
- 经用户单独授权，真实只读验证按相同查询步骤读取了三个可见项目，四路并发完成 24 次 `Engine.Get`，找到唯一匹配的 `global/default_collection` 引擎；没有使用两项缺失的 list 权限，没有修改 IAM/配置/凭证文件或调用模型。此项验证采用独立只读脚本；新登录后的完整 Go 持久化流程由模拟集成覆盖，本轮未重新进行真实浏览器 Google 授权。

## 当前未提交改动审查与回归（2026-10-23）

本轮重新核对自动/手动会话参数、旧后端能力标记、配置与账号隔离、取消/失败不落盘，以及 Google 只读查询契约。修正六个语言文件的格式，更新仍描述旧五字段表单和全局保存行为的使用说明；没有修改或跳过测试来规避 Windows 安全策略。

- 前端专项 `bun test tests/geminiEnterprise.test.ts` 的 16 项通过；`bun run verify` 的 1758 项测试、Lint、TypeScript 和生产构建通过。
- 后端 `go test -json -count=1 -timeout 240s ./internal/auth/geminienterprise ./internal/config ./internal/api/handlers/management ./sdk/auth` 的四个包通过，包含 938 个测试及子测试；`./internal/api ./sdk/cliproxy` 的两个包通过，包含 469 个测试及子测试。执行期间源码未变化，server 编译通过。
- 新前端通过隔离后端实际托管。全局 Enterprise 默认绑定为空，前端真实 API 模块调用该后端的九项检查通过：自动/手动启动、PKCE、取消和旧回调拒绝、非法参数校验、手动拒绝回调，以及全局配置和旧账号绑定不变。
- 浏览器确认两字段默认表单、引擎校验、助手留空/自定义、五字段手动模式、Tab/空格键切换、合成会话生成与取消、旧 state 拦截、资源搜索和原账号详情。控制台无错误。没有打开 Google 授权页面，也没有取得真实 Token 或发送模型请求。

此前 Windows Application Control 的专项诊断和完整回归失败记录保持保留：固定提交 `4a23c88b` 的隔离全量曾有 91 包通过、17 包受策略拦截、33 包标记 skip，不能称为全量全绿。本轮只对上述六个相关 Go 包重新回归，没有把其成功结果替代后端全量结果；系统策略没有修改。前端产物随配套后端提交装配，两个仓库分别提交，暂不推送。

## 已提交资源发现流程的真实验收（2026-10-23 午后，北京时间）

资源发现改动提交后，固定后端 `74fe54b7` 完成新一轮真实 Google 授权、资源识别和 Argon 双 SDK 验收，最终脱敏报告完成于 `2026-10-23T13:07:56+08:00`。配套前端 `cd671427` 仅作为已提交版本参考；本节不覆盖随后并发产生的前端未提交改动，前文的页面交互检查仍属于其原有合成环境验证。

- 从空的隔离认证目录及空的全局 Enterprise 默认绑定启动。会话只提交公开应用参数、引擎和助手输入，不注入旧配置的项目、区域或集合；用户完成 Google 授权后，后端自动识别出与已有资源一致的唯一项目、区域和集合，保存一条带 refresh token 的新记录。助手沿用本次输入，没有声称自动发现助手。
- v8 凭证公开摘要与实际保存的资源绑定一致，全局配置及 ETag 不变，没有加载或覆盖旧账号。真实回调和凭证持久化已完成；辅助脚本的旧列表路径 404 修正为 `/v8/management/credentials` 后，使用已保存的新凭据继续验收。
- OpenAI Python SDK `3.36.1` 与 Google GenAI Python SDK `2.32.0` 各发现七个模型，均包含 `gemini-4-argon`。历史/JSON、Search SSE 引用、指定 URL 正负对照、代码与真实执行结果 SSE 四类用例在两个 SDK 中全部通过，共八次生成，无自动重试。引用范围及 Google 官方来源落地校验通过。
- 隔离验收服务已停止，临时二进制已删除；原 OAuth Secret、Windows 策略诊断记录及其他无关改动保留。没有追加全量 Go 测试或修改系统策略，既有全量测试受拦截的结论保持不变。

固定提交、二进制 SHA256、真实用例表、保留的中断记录和验收范围见 [Gemini Enterprise 使用与验收文档](GEMINI_ENTERPRISE_CN.md)。本次文档补录仅整理已有脱敏证据，没有再次授权或追加模型请求。

## 用量展示独立进程补验收与 Windows 4551 复核（2026-10-23）

固定后端 `95e53da5c6a80d907534a92a15dc3e64445d9d8f`，配套前端 `1b93eeb437ed7bebb55df40c98c202c44bea50b9`，在本机隔离的真实 Go 后端独立进程中补齐用量展示联调。最终报告完成于 `2026-10-23T17:00:54+08:00`。本节仅覆盖这两个固定提交，不将工作区其他未提交代码或产物计入验收。

- 12 项真实 HTTP 检查通过：前端提交产物、后端固定静态文件及实际响应正文一致，响应长度为 4,002,650 字节；v8 配置与用量接口鉴权有效，隔离实例启用用量统计，初始用量和凭证列表为空。
- 通过真实 `POST /v8/management/observability/usage/import` 导入五条合成用量记录，覆盖 Enterprise 缺少上报、其他 provider 明确为零、正数上报、失败和缓存计数。实际用量响应保留计数、结果、耗时、来源及认证索引；重复导入跳过全部五条，没有重复计数。后端快照导出与查询结果一致。
- 11 项浏览器及导出检查通过：页面区分“未提供”、数字零及已上报数值，显示失败结果与耗时；筛选其他 provider 时隐藏缺少用量提示，清除筛选恢复五条记录；JSON 导出将未提供值保留为 `null`，CSV 对应字段为空，明确的零及正数保持数值。导出元数据与真实后端数据一致，导出未改变后端快照，退出后清除连接，控制台无错误。导出文件另经独立核对，不重复计入这 11 项。
- 全局配置字节和 ETag 未变，没有写入凭证文件，模型列表为空。本轮未使用真实 OAuth 凭据、交换 Token、打开 Google 页面或发起模型请求。

原先触发 Windows Application Control 错误 4551 的程序，使用原构建方式在原输出路径重建后，SHA256 与原拦截程序完全一致，本次成功启动。程序仍为未签名状态，系统策略仍处于强制模式（模式值 `1`），策略文件 SHA256 前后相同；没有修改系统策略、证书或信任配置，也没有更名、迁移可执行文件或改变编译参数以规避拦截。本轮未发现该程序新增的匹配 Code Integrity 拦截事件。现有证据无法确定 Windows 放行决定变化的具体原因，不能据此保证未来构建均可运行。

| 对象 | SHA256 |
| --- | --- |
| 前端产物与实际 HTTP 正文 | `d8009ccad2dffc15d4ef698c8aed9e9a8389bafccf7c8bb51ec891ec210a5308` |
| 原路径的独立进程程序 | `88cca97c474972d04a13727ddabea20c66a1c98a9c55a41ea7ed60187c41f1ff` |
| 前后未变的策略文件 | `2668895a5b233a80432d00d67251d7b7f52686a3fb13780f4b242c5a1f937a01` |

首次策略复现启动即已成功。随后两次功能启动修正的只是本机验收夹具的静态目录映射和断言基线字段名；初始夹具失败记录仍保留，未作为业务代码或系统策略修复。既有 61 份证据文件的 SHA256 以及 2,145 个固定源码快照文件均保持不变。原 4551 拦截记录和固定提交 `4a23c88b` 的全量回归受阻记录继续保留，本轮没有补跑其余受阻包，不能宣称全量 Go 测试全绿。

本机脱敏证据目录标识为 `windows4551-95e53da5-20261023`，包含 `FINAL_REPORT.json`、`http-integration.json`、`browser-regression.json`、`export-regression.json`、策略前后快照、`cleanup.json` 及页面截图 `live-backend-usage-details.png`。验收后已停止本轮独立进程、关闭监听端口并删除重建的临时二进制；本节补录未修改业务代码、静态产物或系统安全配置，其他未提交改动保持保留。

## Token 分项统计与用量展示复核（2026-10-23 傍晚，北京时间）

本轮检查本地 Token 计数及分项可用性链路，相关业务语义见 [本地 Token 统计说明](GEMINI_ENTERPRISE_CN.md#本地-token-统计与缺失分项2026-10-23)。本地计数只用于可见业务内容统计，不代表 Google 上游计费；六种语言的缺失提示已说明这一区别。

- 后端按最终请求及合并输出计数，普通响应和协议终包写入已知的标准 Token 字段。管理统计、快照、队列及插件适配保留未知分项，未知思考/缓存不会补零或变成完整的 v2 细分。修正了快照可用性指针共享、显式未知/零总量重算，以及导入去重混淆未知与零的问题。
- 前端明细逐项保留 `null`，汇总和分类图只使用可用计数；JSON 明细导出保留 `null`，CSV 对应字段为空。非法布尔值、对象和空白字符串不再被转成数字零，明确的凭证类型优先于历史文件名回退。全量收集器不提前按文件名丢弃零值。
- Bun `1.3.14` 专项 16 项通过；`bun run verify` 的 1,777 项测试、Lint、TypeScript 及生产构建通过。验收脚本在成功后打印 Unicode 时遇到 GBK 编码错误；原始 verify 进程退出码为零，日志和脚本错误均保留，未将它误记为产品测试失败。
- 后端相关九包的中间一轮有八包通过、6,619 项测试及子测试通过，一项新增的规范细分测试发现未知零总量被标成 `complete`，已修正为 `unclassified`。最后一轮九包回归有五包通过、4,329 项测试及子测试通过，没有失败断言；`sdk/cliproxy/usage`、`internal/runtime/executor/helps`、`test` 及 `internal/api/handlers/management` 的测试程序被 Windows Application Control 阻止启动，因此不能声称最终九包全绿或该受阻断言已在最终版本执行通过。server 编译及 `go mod verify` 通过。
- 离线 Python 双 SDK 命令尝试启动时，其 Go `test` 包助手同样被策略拦截，未执行 SDK 请求；此前的真实 Google/Argon 与双 SDK 成功记录保持原阶段结论。本轮没有重新进行真实授权或模型调用。
- 另对 `internal/translator/geminienterprise`、`sdk/pluginapi` 和 `internal/managementasset` 完成完整包回归，三包、47 项测试及子测试通过。它们与上述最后一轮成功包合计八包、4,376 项；四个受阻包仍不计为通过。
- 正常构建的 Go server 独立进程成功启动，以真实 v8 API 分两阶段导入三条 Enterprise 记录及五条混合记录；两阶段分别通过 10 项和 6 项 HTTP 检查。八条记录覆盖部分计数、明确为零、全部未知、失败、缓存、未知总量及仅输出可用；汇总为八次请求、六成功、两失败、已知总量 19。重复导入不重复计数，导出/查询一致，全局配置和 ETag 未变，凭证目录及模型列表始终为空。
- 浏览器完成 16 项检查，确认总量与 TPM、缺失思考/缓存的汇总及图例、逐项空值、明确零值、失败结果与耗时、筛选提示恢复及退出登录。实际下载的 JSON/CSV 另经三项独立检查，与后端八条记录的计数及身份元数据一致，导出没有改变后端快照；控制台无错误。

前端提交为 `4e74515d98c9e549bf20e8ee50eaaa4a6e579906`，使用固定构建环境 `VERSION=dev`、`BUILD_TIME=2026-10-23T09:32:20Z`。单文件产物与后端 `static/management.html` 及实际 HTTP 正文相同，长度 4,003,799 字节，SHA256 为 `6d54b814a556cbdc28b34dc21dc919c040c28cf50f82184ba387b206bd59cac7`。从该提交使用同一构建环境重新构建，产物与已提交文件及后端副本逐字节一致。

系统策略仍为强制模式 `1`，既有策略文件 SHA256 仍为 `2668895a5b233a80432d00d67251d7b7f52686a3fb13780f4b242c5a1f937a01`；独立 server 仍未签名。本轮没有修改或绕过策略、证书和信任配置，也没有用其他执行方式替代受阻的测试。历史 4551、固定提交全量受阻及本轮拦截日志均保留，不因独立实例成功启动而宣称系统问题已经消除。

本机脱敏证据目录标识为 `enterprise-token-statistics-20261023`，含回归日志、HTTP 两阶段结果、浏览器及导出核对、固定构建环境、产物哈希和清理记录。原诊断记录与其他无关改动保留，独立实例及浏览器验收会话已停止；两个仓库分别提交，暂不推送。

## 后端 9ab28a2f 的补测结果（2026-10-23）

后续固定后端 `9ab28a2f333dbc8dbdb240f84a3885105ad9cfaa` 补跑上一节四个受阻包，`sdk/cliproxy/usage`、`internal/runtime/executor/helps`、`test` 及 `internal/api/handlers/management` 全部通过，共 2,291 项测试及子测试；`TestEnsureTokenBreakdownPreservesPartialAvailability` 已在该固定版本执行通过。OpenAI Python SDK `3.36.1` 与 Google GenAI Python SDK `2.32.0` 对本机合成 Go 上游的九项离线检查也全部通过，包含普通/流式响应、JSON、引用或 grounding 及用量字段。

系统策略保持强制模式 `1`，策略文件哈希未变，没有修改或绕过安全策略；本轮未重现 4551，但测试程序与历史受阻程序的哈希不同，放行原因尚未确定。前文受阻结果和全量回归记录继续按原阶段保留。本次只补齐上述后端与离线 SDK 检查，没有重新执行前端构建、浏览器联调或真实 Google/Argon 请求。

固定源码、命令、各包计数、证据目录及验收边界见 [后端补测记录](GEMINI_ENTERPRISE_CN.md#后端-9ab28a2f-补测记录2026-10-23)。其他无关改动及历史诊断均保留，暂不推送。
