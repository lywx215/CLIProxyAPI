# Gemini Enterprise 管理前端适配与验收

## 评估结论

原实现已覆盖 CLI 登录、账号存储、模型发现与双 SDK 请求，但管理前端没有 Gemini Enterprise 入口，v8 管理接口也不能启动该 provider 的授权。只增加前端卡片无法完成登录，必须同时补齐管理接口与凭证展示契约。

本次改动位于 CLIProxyAPI 与 Cli-Proxy-API-Management-Center 两个仓库。Gemini Enterprise 使用 OAuth 凭证管理，不添加到 API Key 提供商编辑器。配置面板提供入口，实际配置与登录统一放在 OAuth 登录页。

## 用户流程

1. 打开 **OAuth 登录 → Gemini Enterprise**。
2. 填写 OAuth 客户端 ID、项目、引擎及可选资源参数。默认区域为 `global`，集合为 `default_collection`，助手为 `default_assistant`。
3. 对需要 Secret 的客户端，只填写后端环境变量名。Secret 值由后端进程环境提供，不通过前端保存。
4. 在 Google 客户端登记完全一致的 `http://localhost:8788/oauth-callback`，保存配置后发起授权。
5. 后端可在本机 loopback 接收回调。端口被占用时不会停止占用端口的进程，仍可使用手动回调；远程后端也可以粘贴完整回调 URL。
6. 手动回调须使用当前授权尝试的 state。前端拒绝错误来源、错误路径、重复 state 或不完整结果，后端再次验证 pending session 与 provider。
7. 可取消待完成的会话；取消成功（`cancelled: true`）后不会保存新凭证。持久化已开始时返回 `cancelled: false`，应继续查询实际结果；保存完成前状态仍为等待。成功后刷新认证文件列表，查看账号自己的资源绑定、访问令牌到期时间及刷新令牌状态。

配置写入使用局部 PATCH，并通过最新配置的 ETag 防止并发覆盖；如果同一 Enterprise 绑定已经被其他窗口修改，先重新加载再保存。切换连接、管理密钥或退出登录会废弃旧请求。已有账号保留自己的客户端和资源绑定。

## v8 接口契约

| 操作 | 接口 | 说明 |
| --- | --- | --- |
| 读取配置 | `GET /v8/management/config` | 复用前端全量配置缓存 |
| 保存绑定 | `PATCH /v8/management/config/oauth/providers/gemini-enterprise` | 仅写七个公开配置字段，携带 `If-Match` |
| 启动授权 | `GET /v8/management/oauth/auth-url?provider=gemini-enterprise` | 返回 `url`、`state`、`callback_url`、`manual_callback_required` |
| 提交回调 | `POST /v8/management/oauth/callback` | 复用 state/provider 校验与原子回调文件 |
| 查询状态 | `GET /v8/management/oauth/status?state=...` | 成功在凭证持久化之后报告 |
| 取消 | `DELETE /v8/management/oauth/session?state=...` | 复用现有会话取消契约 |
| 凭证列表 | `GET /v8/management/credentials` | 新增公开 `gemini_enterprise` 摘要，不包含 token 或 Secret |
| 手动刷新 | `POST /v8/management/credentials/refresh` | 复用现有 executor 刷新；缺少刷新令牌时提示重新授权 |

未新增或修改 v0 路由；新增凭证摘要仅在 v8 返回，既有 v0 凭证列表响应保持不变。CLI 与管理登录共享账号 ID 和刷新令牌保留规则，不能从其他账号、应用或资源绑定继承刷新令牌。

凭证支持品牌筛选、按项目/引擎/助手搜索、详情、启停、下载及现有模型浏览。模型由后端实际发现，不在前端预填 Argon 或虚构目录。该 provider 没有可用配额和 countTokens 接口，页面明确说明此限制。新增文案覆盖简体中文、繁体中文、英语、俄语、越南语和韩语。

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
