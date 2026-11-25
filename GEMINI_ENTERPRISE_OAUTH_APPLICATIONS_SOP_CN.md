# Gemini Enterprise 新 OAuth 应用导入与验收

每次新增应用都按本说明核对 Google Cloud、CLIProxyAPI 管理中心、Gemini Manager 桌面端以及 `init.example.com` 远程授权。先在指定试点验收，再切换新账号的默认应用。已有账号继续用凭证绑定的原客户端刷新和续授权。

## 项目发现返回 SERVICE_DISABLED

如果 OAuth 授权和令牌刷新已经成功，但项目发现接口 `cloudresourcemanager.googleapis.com/v3/projects:search` 返回 `403 SERVICE_DISABLED`，项目集合可能为空。由项目管理员在 Google 返回的 `consumer` 指向的 OAuth 项目启用 Cloud Resource Manager API。生效后，在管理中心对现有凭证点击 **刷新项目集合**，再验收模型目录。

## 1. 核对 OAuth 项目并启用必需 API

1. 创建或选中新 OAuth 客户端要登记的 Google Cloud 项目，记下应用名称、项目 ID、项目编号和目标 CPA 实例。启用 API、配置受众、添加范围和创建客户端都在同一个项目完成。
2. 在同一项目打开 **API 和服务 → 库**，查找 **Cloud Resource Manager API**（`cloudresourcemanager.googleapis.com`），由有权限的操作员点 **启用**。打开 **已启用的 API 和服务** 回查该服务，记录状态和时间。**这是首次授权前的必检项。** [Google Cloud 启用 API 说明](https://docs.cloud.google.com/service-usage/docs/enable-disable#enable)
3. 也可以使用已登录且具备该项目权限的 Cloud Shell 或 `gcloud`。替换 `OAUTH_PROJECT_ID` 为实际项目 ID，启用后回读，确认输出包含精确服务名。[Google Cloud CLI 服务列表说明](https://docs.cloud.google.com/sdk/gcloud/reference/services/list)

   ```sh
   gcloud services enable cloudresourcemanager.googleapis.com --project=OAUTH_PROJECT_ID
   gcloud services list --enabled --project=OAUTH_PROJECT_ID --filter="config.name=cloudresourcemanager.googleapis.com" --format="value(config.name)"
   ```

自动发现使用 OAuth 项目执行项目搜索。搜索返回的业务项目还需具备各自的 Discovery Engine API、可访问的 Enterprise 应用和适用的授权。需要为业务项目初始化资源时，沿用现有初始化流程。[Enterprise 集合发现](docs/enterprise-account-collections.md) · [项目初始化工具](scripts/gemini-enterprise-init/README.md) · [Google 项目搜索接口](https://docs.cloud.google.com/resource-manager/reference/rest/v3/projects/search)

## 2. 配置 Google OAuth 受众、权限、来源和回调

1. 在 **Google Auth Platform → 品牌塑造** 设置应用名称、支持邮箱和开发者联系邮箱。为远程回调在 **已获授权的网域** 核对 `example.com`，按控制台提示登记及验证域名所有权。[Google OAuth 品牌设置](https://support.google.com/cloud/answer/15549049?hl=zh-Hans)

   新建外部应用先以 **测试** 状态联调。在 **目标对象 → 测试用户 → 添加用户** 填入实际联调使用的完整 Google 邮箱，保存后确认名单能查到。此流程含 `cloud-platform`，外部测试模式的刷新令牌自同意起 7 天到期。正式对外使用时按 Google 要求发布并完成敏感范围验证。[Google OAuth 受众与发布状态](https://support.google.com/cloud/answer/15549945?hl=zh-Hans)
2. 在 **Google Auth Platform → 数据访问 → 添加或移除范围** 搜索并勾选所需项。如果列表里找不到 `openid`，在 **手动添加范围** 输入 `openid`，点 **添加到表格**。选好后点 **更新**，回到 **数据访问** 页面点 **保存**，再回读四项。远程授权使用完整列表，其中 `openid` 用于远程身份校验。[Google 数据访问配置说明](https://support.google.com/cloud/answer/15549135?hl=zh-Hans)

   ```text
   openid
   https://www.googleapis.com/auth/userinfo.email
   https://www.googleapis.com/auth/userinfo.profile
   https://www.googleapis.com/auth/cloud-platform
   ```

3. 在 **客户端** 创建 **Web 应用**类型的 OAuth 客户端。按现有客户端的模板，在同一客户端的两个字段分别登记下列地址，每个地址添加为一项。JavaScript 来源填写协议、主机名和相应端口。重定向 URI 填写完整回调地址，保存后按实际请求核对协议、主机名、端口、路径和尾部斜杠。[Google Web OAuth 客户端设置](https://developers.google.com/identity/oauth2/web/guides/get-google-api-clientid) · [回调匹配规则](https://developers.google.com/identity/protocols/oauth2/web-server#uri-validation)

   **已获授权的 JavaScript 来源**

   ```text
   http://localhost
   http://localhost:5173
   ```

   **已获授权的重定向 URI**

   ```text
   http://localhost:8787/auth/google/callback
   http://localhost:8788/oauth-callback
   https://init.example.com/oauth-callback
   ```

   `8787` 是现有客户端模板里的本地 Google 授权回调。CLIProxyAPI 管理中心和 Gemini Manager 的本地固定回调使用 `8788`，Gemini Manager 远程授权使用 `init.example.com` 的 HTTPS 回调。

4. 保存后回读控制台的两条来源和三条回调，再下载 Web 客户端 JSON 并存到受保护的本机目录。核对 JSON 的 `web.project_id`、`web.client_id` 和 `web.redirect_uris`。导出包含 `web.javascript_origins` 时一并比对。交付接入时提供应用名称、项目 ID 与编号、JSON 的本机路径、联调邮箱和试点实例。Client Secret 从本机文件读入现有加密保管和配置工具。回执只记录应用身份、配置引用和验证结果。

## 3. 追加应用到试点配置

### CLIProxyAPI 与管理中心

1. 选用本次指定的单台实例作为试点。先记录原有应用、当前新账号默认项、旧账号客户端绑定、渠道状态和相关配置。在现有 `oauth.gemini-enterprise-applications.entries` 追加条目，将下列示例合并到原有节点。继续保留 `oauth.providers.gemini-enterprise` 的原应用和已有条目。

   ```yaml
   oauth:
     gemini-enterprise-applications:
       entries:
         - name: "应用名称"
           client-id: "NEW_CLIENT_ID.apps.googleusercontent.com"
           client-secret-env: "GEMINI_ENTERPRISE_CLIENT_SECRET_NEW"
   ```

2. 每个客户端使用独立的环境变量。`client-secret-env` 保存该变量的名称，将匹配的 Secret 放入目标服务现有的受保护配置。VPS 通过 Compose 的 `env_file` 载入，核对运行进程确实取得新值。若需要重建 CPA 容器，按既有单服务器维护流程先记录并停掉所有指向该实例的 New API 渠道，确认传播、排空并验收后，只恢复原先启用且实测通过的渠道。[逐台更新和恢复流程](scripts/ops/README_CN.md#每台服务器的执行步骤)
3. 通过已认证的 `GET /v8/management/credentials/gemini-enterprise/oauth/capabilities` 回读 `applications` 中新条目的名称、`client_id` 和 `client_secret_ready=true`。通过 Gemini Manager 上传时，另核对该条目的 `import_supported=true`。管理中心从后端读取可选应用，在新建授权的 **OAuth 应用** 下拉框选择刚登记的名称。[v8 OAuth 导入契约](docs/enterprise-oauth-import.md)

### Gemini Manager 桌面端与远程服务

1. 在 `config/system.yaml` 的 `automation.gemini_enterprise_init.oauth_applications.entries` 追加同一个 Client ID 和名称。使用唯一的 `secret_ref`。通过现有加密配置工具将匹配的 `client_id`、`client_secret` 和固定 `redirect_uri` 保存到 `config/secrets.enc` 对应引用。桌面端本地 `redirect_uri` 明确填写 `http://localhost:8788/oauth-callback`，与导出的回调排列顺序无关。

   ```yaml
   automation:
     gemini_enterprise_init:
       oauth_applications:
         entries:
           - name: "应用名称"
             client_id: "NEW_CLIENT_ID.apps.googleusercontent.com"
             secret_ref: "oauth.gemini_enterprise.applications.new_app"
   ```

2. 对 `init.example.com`，同步核对桌面远程调用端、远程控制端和 Worker 的应用条目及加密密钥。确认各端用于本次远程授权的 `automation.gemini_enterprise_init.remote.oauth_client_id` 指向同一 Client ID，远程实际回调是已注册的 HTTPS 地址。保留 `fallback_oauth_client_id`、原有密钥、正在执行的任务和账号配置。
3. 在桌面使用 **准备检查**；在远程部署目录按现有服务账号执行 `python -B scripts/gemini_enterprise_remote.py preflight --node remote`。确认远程健康检查、客户端、回调和目标 CPA 能力。`authorize-window` 的成功回调会排队初始化任务，只有本次明确要验收该远程任务时才提交。仅验收凭证时使用本地凭证入口。相关入口和部署细节见 Gemini Manager 的 `docs/GEMINI_ENTERPRISE_INIT_SOP.md` 和 `docs/GEMINI_ENTERPRISE_INIT_REMOTE_DEPLOYMENT.md`。

## 4. 用单个测试账号完成验收

1. 选择已加入测试用户且尚未绑定旧 Enterprise 应用的账号，提前确认该邮箱能访问至少一个用于验收的业务项目和 Enterprise 应用。同一个账号选择一个入口导入。

   - **管理中心** 在 **OAuth → Gemini Enterprise** 选择新应用。页面提示手动回调时，把本次授权的完整浏览器回跳地址交给同一授权弹窗。
   - **Gemini Manager** 选择新应用后点 **获取 Gemini Enterprise 凭证**。自动化上号的 **Cloud SDK** 路沿用自身客户端。**Enterprise** 路读取 Gemini Manager 的新账号默认值，在切换后核对实际 Client ID。

   开始授权时核对实际请求的 `client_id`、`redirect_uri` 和 Google 邮箱。用户完成同意、回调和凭证保存后，回读邮箱、授予范围、应用 ID，并确认能刷新。已有新应用凭证的读回记为复用，首次授权另测。
2. 管理中心直接授权后回读该账号凭证。走 Gemini Manager 路径时，选中同一台试点，点 **上传 CPA 凭证**，核对回执 `status=saved`、`runtime_synced=true` 和所属 Client ID，再执行 **同步 CPA**。保留回执编号。结果不明确时按原回执查询，已保存但尚未发布到运行进程时按原回执补做运行同步。[v8 OAuth 导入回执](docs/enterprise-oauth-import.md#reconcile-and-concurrency)
3. 在 CPA 管理中心对该账号 **刷新项目集合**。核对项目发现能找到预先确认可见的业务项目，需要使用的项目、区域、集合和应用均能匹配。检查该账号下至少有一个目标集合、目标模型目录非空且所需模型可派发。出现部分失败时逐项查看服务、项目编号和错误码。
4. 在授权范围内，从该账号的目标集合做一次小文本真实调用，确认有非空输出，流式调用正常结束，且请求确实命中该测试凭证。记录结果。没有实际调用或只有列表结果时，验收记录填 **模型调用未验证**。
5. 远程授权纳入本次验收时，另记 HTTPS 回调、远程授予范围、凭证保存和任务回执。回调可达和准备检查通过属于预检，完整验收以用户同意后的实际回调和保存为准。

## 5. 切换新账号默认并复核原应用

1. 在验收范围内，将 CPA 的 `oauth.gemini-enterprise-applications.new-account-default-client-id` 和 Gemini Manager 的 `automation.gemini_enterprise_init.oauth_applications.new_account_default_client_id` 指向新 Client ID。回读管理中心和 Gemini Manager 新建授权页面的默认选项。自动化凭证入口启动 Enterprise 路前也核对实际 Client ID。
2. 选原应用的旧账号，检查续授权入口发出的 Client ID 与该账号原绑定一致，核对已有授权仍能刷新。续授权检查可在跳转 Google 前核对 URL 并取消。对照变更前快照，确认老账号文件、项目集合配置、手动停用及原有渠道状态符合预期。
3. 默认项需回退时，把新账号默认值恢复到上一个值。只要仍有账号绑定某个客户端，就保留对应条目和 Secret。
4. 扩到其他实例时逐台维护。用每台实例自己的配置回读、凭证和真实模型请求分别验收现有在用应用和新应用。等该台涉及的应用全部通过并完成渠道恢复和维护锁收尾，再开始下一台。

## 常见错误定位

| 现象 | 核对项 |
| --- | --- |
| `403 SERVICE_DISABLED`，服务为 `cloudresourcemanager.googleapis.com` | 核对 Google 错误里的 `consumer` 项目编号，在该 OAuth 项目启用服务并回读。用现有凭证刷新项目集合。 |
| Google 回调前出现 `redirect_uri_mismatch` | 核对授权请求用的 Client ID，以及同一客户端里对应的精确回调。 |
| Google 同意页 `403 access_denied` | 核对实际登录邮箱、该 OAuth 项目的受众及测试用户名单、Google Workspace 限制。 |
| `invalid_client` 或 `client_secret_ready=false` | 核对 Client ID 和 Secret 是否配对，以及桌面、远程服务或容器是否载入了正确引用。 |
| `invalid_grant` | 核对是否为原客户端、用户是否撤销授权，以及外部测试授权是否已到期。需要续授权时用账号原绑定。 |
| 项目搜索成功但集合仍为 0 | 核对账号是否能看到预期业务项目、该项目的 Discovery Engine API、应用和区域。 |
| 集合已发现但模型不可用 | 按具体集合检查模型目录、许可、应用状态和额度，用原始错误确认具体阻塞。 |

## 交接记录

- 应用名称、Google Cloud 项目 ID 与编号、公开 Client ID、目标 CPA 实例、验收时间。
- Cloud Resource Manager API 已启用的回读结果、受众状态、四项范围、两条 JavaScript 来源、三条回调及各端 Secret 引用的就绪状态。
- 本地与远程分别记录授权、回调、保存、刷新和上传结果。记录可见项目数、目标集合和模型；模型调用填写通过、失败或未验证。
- 记录旧账号续授权抽查结果、新账号默认项、原渠道恢复结果和待办事项。
