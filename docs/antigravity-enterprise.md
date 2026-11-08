# Antigravity Enterprise provider

`antigravity-enterprise` connects Google Antigravity subscriptions through Business AI Code. It uses the Antigravity desktop OAuth application and a shared account credential with independently routed subscription bindings.

## Authorization and discovery

Use `--antigravity-enterprise-login --no-browser` or the **Antigravity Enterprise** OAuth card in the management UI. Open the complete authorization URL in the browser profile containing the intended Google account. Management login supports the existing manual callback flow if the loopback receiver is unavailable. OAuth uses state validation and PKCE.

The authorization is saved before license discovery. Discovery reads `v1beta:fetchLicenses`; it does not self-assign licenses, enable APIs, modify IAM, or invoke models. A project and location pair identifies one binding. The response does not identify individual billing subscriptions, and the provider does not invent subscription IDs or require administrative license-list permissions.

If initial discovery requires a quota consumer project, configure:

```yaml
oauth:
  providers:
    antigravity-enterprise:
      discovery-quota-project: your-existing-project
```

This project selects the consumer for initial discovery, not the set of returned subscriptions. Each subsequent binding uses its own project in `X-Goog-User-Project`. Supported locations are `global`, `us`, and `eu`; generation never falls back across regions.

One account file owns OAuth material and the versioned `antigravity_enterprise_account` graph. Bindings become runtime-only children with stable indices. The account switch affects every child without overwriting individual switches. Discovery retains settings and disabled states, enables new supported bindings, and preserves existing bindings on incomplete coverage. Confirmed missing licenses remain as unavailable historical bindings.

Reauthorization preserves the current graph. Token refresh is shared across children and persisted once. Account and binding revisions reject stale edits; registration and credential versions fence late metadata and execution results.

## Models and generation

The directory uses `FetchConfig` and a versioned default catalog matching the verified native CLI directory: Gemini 4 Argon, Gemini 3.8 Flash, Gemini 3.7 Flash, and Gemini 3.6 Flash at low/medium/high; Gemini 3.1 Pro at low/high. Fourteen options are exposed before per-binding exclusions and aliases. Explicit server restrictions and supported thinking levels narrow that list.

A config containing only `adminControls` is compatible with the default directory. A failed CCPA `fetchAvailableModels` request is not evidence that this Enterprise directory is unavailable. Directory reads and refreshes never perform generation. Directory visibility and successful generation are reported separately.

The executor uses regional Business AI Code `generateContent` and `streamGenerateContent`, with canonical Gemini content and an explicit subscription entitlement. It supports the existing Gemini, OpenAI Chat Completions, OpenAI Responses, and Claude Messages adapters, including streaming and function-call data. It does not execute client tools. Payload rules run after translation, thinking normalization, signature handling, and entitlement construction; final read-only checks reject resource or model changes.

Standard Gemini `tools: [{googleSearch: {}}]` and its `google_search` spelling are adapted to `enterpriseWebSearch` before payload rules. Existing enterprise search options are retained and duplicate declarations are combined. Nonempty standard search options, such as `timeRangeFilter`, are rejected with a request-level HTTP 400 because the enterprise endpoint cannot enforce them equivalently. Conflicting enterprise search configurations also fail instead of silently dropping restrictions. Payload overrides and filters remain authoritative; a filtered tool is never recreated, and final validation does not rewrite rules that reintroduce an unsupported standard alias. This mapping does not add `codeExecution` or `urlContext` support.

Native Gemini user messages may combine `functionResponse` and ordinary text or media parts. Business AI Code requires separate turns for these groups, so the executor splits contiguous groups while preserving their order, call IDs, signatures, parallel results, declared tools, and calling mode. This normalization runs before payload rules for both streaming and non-streaming generation.

The root `model` field uses the full native option ID, for example `gemini-3.8-flash-low`. Family and canonical response names are separate metadata; sending the bare family name does not select the same option. The native `model` and `aicode` fields form a protobuf oneof, so first-party generation sends only `model`. Unsupported native safety enums are removed before payload rules, and the final read-only validation rejects incompatible values introduced by those rules.

`gemini-4-carbon` is not assumed available and is not in the default directory. A credential-pinned management model test may request this exact unlisted model with `allow_unlisted_model: true`; this explicit generation consumes quota and does not enable a cloud feature or add a routing alias. `countTokens` is unsupported rather than emulated by another provider.

## Weekly quota and cooldown

Weekly observations come from `FetchQuotaStatus`. The shared pool key includes provider, project, location, edition, and bucket, and excludes the OAuth account. Multiple credentials for the same pool do not create additional quota. Different pools' percentages are never summed.

The current weekly bucket is `gemini-enterprise-weekly`. Fraction and amount have explicit presence and are mutually exclusive: zero means exhausted and absent means unknown. No absolute token balance is inferred from a percentage or a screenshot's seat count. The seven-day cycle starts with upstream use; the returned reset timestamp is authoritative. A missing reset timestamp remains unknown.

Only subscription use is supported. An enabled or unknown overage setting blocks generation. `overageInfo: {}` represents proto3 `enabled=false`. The server must enforce the disabled overage policy; a cached positive balance cannot itself guarantee a request stays below quota.

Successful quota observations expire after two minutes and at their reset boundary. Enabled bindings receive bounded background metadata refreshes. Exhausted shared pools block all their subscription models, and recovery requires a fresh observation. Unknown reset times trigger metadata rechecks instead of an invented seven-day timer. Refreshing metadata, clearing local cooldowns, rotating tokens, and disabling temporary cooldowns cannot fabricate quota or bypass the subscription-only boundary.

Expired positive observations and unknown metadata are not model cooldowns: they may reach the executor's metadata refresh, but generation still requires a current subscription-only observation. Failed metadata reads return a local preflight error without cooling a valid credential or using an expired balance. Background refresh starts immediately, force-refreshes quota every minute, and uses three persistent workers with one pending job per binding. A slow binding does not delay scheduling the other bindings. Actual exhaustion and explicit upstream weekly restrictions remain separate hard gates.

Short unclassified 429s use 10/20/40/60-second backoff; explicit retry hints take precedence. Permission failures are scoped to the affected binding, model availability errors to that model, and invalid OAuth grants to the account. Metadata failures do not count as generation failures. In-flight generation and streams are not replayed after output begins.

## Management API

Only `/v8/management` is extended:

| Operation | Endpoint |
| --- | --- |
| Account list with nested bindings | `GET /credentials` |
| Rediscover licenses | `POST /credentials/antigravity-enterprise/discover` |
| Edit a binding | `PATCH /credentials/antigravity-enterprise/bindings` |
| Binding directory | `GET /credentials/models` |
| Pinned explicit generation test | `POST /credentials/models/test` |
| Shared weekly quota | `GET /quota/antigravity-enterprise` |
| Cooldown reset or quota recheck | `POST /routing/cooldown/reset` |

Account operations identify the saved parent. Binding operations include its filename, child `auth_index`, `binding_id`, and expected revision. Ambiguous multi-binding requests fail instead of choosing the first binding. The UI distinguishes unknown balances, actual zero, stale observations, and missing reset times. Opening a directory or refreshing licenses/quota performs no model calls.

CLIProxyAPIHome-managed mode is not supported by this standalone account lifecycle. Existing Antigravity and Gemini Enterprise credentials retain their own provider types and are not implicitly converted or merged by email.

## Live verification on 2026-11-08

The authorized account discovered four visible project/location bindings: `glossy-cipher-514102-r6/global`, `stately-lodge-514102-k9/global`, `stately-lodge-514102-k9/us`, and `tidy-bounty-514102-i1/global`. All fourteen default options returned HTTP 200 and visible `OK` or `OK.` text on every binding (56 successful model requests). The observed Gemini 3.1 Pro response model was `gemini-3.1-pro-preview`.

Separate streaming checks passed for Gemini, OpenAI Chat Completions, OpenAI Responses, and Claude Messages. Function-call output, a follow-up containing the tool result, and inline image input also passed. These protocol checks used `gemini-3.8-flash-low` on the first binding; they do not imply that every model/protocol combination was exercised live.

An explicit `gemini-4-carbon` request returned HTTP 403 with `MODEL_NOT_ALLOWED_BY_ADMIN` on all four bindings. Separate `gemini-4-carbon-low`, `gemini-4-carbon-medium`, and `gemini-4-carbon-high` probes reached upstream and returned HTTP 404 on every binding. Carbon remains outside the routing directory. These are observations for this account and these bindings, not a claim about global model availability.

Weekly metadata reads succeeded before and after generation. Every binding reported `remainingFraction: 1`, disabled overage, and no reset timestamp; the displayed precision did not expose an exact balance or the cost of these small requests. The test used a process-memory credential and no persistent account store. It did not change cloud configuration, license assignments, or IAM settings.

Local acceptance passed `go test -p 1 ./...` (115 packages with tests), the server build, and frontend `bun run verify` (2,084 tests across 198 files, ESLint, TypeScript, and the production build). Fixture browser checks covered the account/binding view, a 420-pixel container, both themes, quota states, and an independent binding switch. They used synthetic data rather than a live management login. The final frontend single-file artifact was copied to local `static/management.html` and its SHA-256 matched. No deployment was performed. The race detector was not run because this machine had CGO disabled and no C compiler.
