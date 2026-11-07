# Gemini Enterprise Web: phase one

This optional provider supplements the existing `gemini-enterprise` provider with native Gemini inline attachment and image inputs. It supports the Cloud-hosted Gemini Enterprise Plus web application only. The configuration default is disabled. Historical local and production acceptances are recorded separately in the verification report; they do not establish acceptance of later code changes.

## Routing and supported input

Keep using the existing public model name and native `POST /v1beta/models/{model}:generateContent` or `:streamGenerateContent` route. A single user Content containing text and Base64 `inlineData` is routed in its entirety to `gemini-enterprise-web`. Ordinary text retains its existing Enterprise route. No client selection header or Files API upload is required.

```json
{"contents":[{"role":"user","parts":[{"text":"Describe this image"},{"inlineData":{"mimeType":"image/png","data":"<base64>"}}]}]}
```

The parser accepts Protobuf JSON Base64 alphabets, padding variants and snake-case aliases. It rejects malformed Base64, duplicate JSON keys and unsupported MIME types. Web defaults allow up to 12 attachments, 50 MiB of decoded attachment bytes and a 68 MiB request body; `upstream.gemini-enterprise-web.input-limits` can configure these limits. Supported inputs are `text/*`, PDF, JSON, XML, PNG, JPEG, WebP, HEIC and HEIF. Standalone Business parser callers retain their original limits.

The default `history-mode: single-turn` accepts one user Content and returns 409 for media history. With `history-mode: serialized-context`, the complete visible history is serialized into a fresh Plus session and all supplied user attachments are uploaded again. Truncated history may begin with `model`. Omitted roles default to `user`; explicit roles must be `user` or `model`. The last user Content becomes `current_input`; later model fragments retain their roles and order in `model_continuation`. A nonempty model prefix requests only the new continuation, while empty or filtered thought-only model placeholders keep normal answer behavior. At least one user Content is required. Historical thoughts and signatures are filtered, and historical tool calls remain context records rather than executable requests. Ordinary text-only history keeps the existing Enterprise route.

Serialized Web history defaults to 50 logical Contents (adjacent equal roles share a logical Content), 4,129 raw fragments, 4 MiB of accumulated text/signatures/tool records, 256 parts per logical user Content, and 4,096 per logical model Content. All input thresholds, including tool depth/nodes and generation-config size, are configurable under `input-limits`; the complete default list is in `config.example.yaml`. History is not silently truncated. Limits are snapshotted when a request is admitted, so hot updates apply to new requests without changing in-flight parsing. The management runtime status exposes the effective `input_limits`; saving limits alone never enables Web.

Serialization headroom grows from validated text measurements under the same provider-wide memory budget. Attachment requests do not reserve the configured text maximum when their actual text is small. Raising input limits can still require a larger memory budget; a 429 resource rejection remains distinct from a 413 input-limit rejection. Visual configuration edits preserve the nested limits, which can be edited directly in YAML. Input-limit saves verify backend support before writing and confirm all effective limits afterward; failures retain the draft. See the [input-limit reference](gemini-enterprise-web-input-limits.md) and [history contract](gemini-enterprise-web-history.md).

Input-limit rejections emit a warning even when debug logging is off. The request ID, provider, stage, HTTP status, safe field path, `limit_name`, `observed`, `maximum`, and `observed_at_least` identify the failed check. A true `observed_at_least` means counting stopped early; it is a lower bound, not a complete request measurement. Byte measurements use UTF-8/raw JSON/decoded attachment bytes as named by the limit; `declared_request_bytes` identifies a Content-Length rejection before reading the body. For example, `field="contents[7].parts[0].text" limit_name="history_text_bytes" observed=4194305 maximum=4194304 observed_at_least=true` identifies the default accumulated history-text budget. Only allowlisted structural fields and numeric measurements are logged, without request text, Base64, filenames, cookies, tokens, full source or raw upstream errors. Debug stage logs also retain bounded content/attachment counts in the ordinary log formatter.

Complete media token counting, image generation, independent Files API, automatic browser sign-in, upstream session continuation/import and Home execution are unavailable. Non-native protocols are not translated into the Web channel. An unsupported model/alias, disabled channel, unavailable credential or resource rejection never falls back to the text channel.

## Local text token estimates

Web reuses the ordinary Enterprise channel's embedded Gemma3/SentencePiece tokenizer and its shared lazy initialization. No Google token-count request or model call is added. Ordinary Enterprise execution, routing, tokenizer implementation and response fields are unchanged.

The counter reads the final `streamAssistRequest.query.parts[].text` after payload rules. This includes serialized visible history and context framing where history compatibility is enabled. It never counts attachment Base64, uploaded file bytes, transport file IDs, cookies or thought signatures. Input text is a separate subset, not a complete prompt measurement. Returned answer text and returned thought summaries are estimated separately after the existing stream parser has removed a proven terminal cumulative prefix. Historical thought text already filtered by the request parser is not reintroduced. Code/result text follows the ordinary Enterprise counting convention; citations and suggested-query metadata are not answer-token text.

Management usage records retain nullable counters and add optional provenance:

```json
{
  "input_tokens": null,
  "output_tokens": 7,
  "reasoning_tokens": 3,
  "cached_tokens": null,
  "total_tokens": null,
  "estimate": {
    "source": "local_gemma3_sentencepiece",
    "input_text_tokens": 23,
    "incomplete": true,
    "response_complete": true
  }
}
```

These example values are illustrative. `incomplete` remains true because attachment and provider-internal usage are unknown. `response_complete` records whether a complete upstream answer was observed, not a client acknowledgement or complete billing measurement. Missing thought summaries remain null; explicit measured zero remains zero. A tokenizer failure or insufficient accounting workspace also leaves the affected field unknown, without rejecting an otherwise valid generation.

Native Gemini responses contain only available output/summary counters in `usageMetadata`. The separate `usageEstimate` extension uses the provenance schema above. `promptTokenCount` and `totalTokenCount` are omitted. Streaming emits this extension once at successful termination, never as a fabricated success after truncation, cancellation or upstream errors. Failed request records can retain text estimates already obtained with `response_complete: false`. Pre-generation validation/upload failures do not become model usage.

Text retention is bounded to 4 MiB and workspace is charged to the existing Web resource lease before tokenization. A conservative 256 bytes per input byte plus fixed overhead is reserved for SentencePiece workspace, then released; retained buffers remain charged until request cleanup. Very large contexts may therefore have unknown text estimates even when generation succeeds. This is an admission-accounting bound, not a hard RSS limit. `countTokens` remains unavailable for complete media requests.

The management frontend shows an independent input-text column, `≈` for local output/summary estimates, and the tokenizer source in six languages. CSV/JSON exports retain provenance and unavailable values. Complete-input, cache and total fields remain unknown; incomplete aggregate costs are not presented as complete costs. Old records are not recalculated. These estimates are not authoritative Gemini billing or full internal-reasoning usage.

## Google Search with inline media

Add `"tools": [{"googleSearch": {}}]` at the request root of a native `generateContent` or `streamGenerateContent` request with inline media. The `google_search` spelling is also accepted. Requests with text only continue on the existing Enterprise route.

In the Plus app's Assistant settings, enable web grounding and select **Google Search**. Each explicit Web search request checks the selected app's current `webGroundingType` through `lookupWidgetConfig` before creating a session or uploading media. A disabled, missing, or Enterprise Web Search setting returns HTTP 400. The app's default toggle can be off. The check adds one read-only RPC to explicit Web search requests. [Google Cloud Assistant configuration](https://docs.cloud.google.com/gemini/enterprise/docs/configure-assistant)

The adapter sends an empty `toolsSpec.webGroundingSpec` alongside the existing Web tool registry. Time filters, search type options, nonempty `toolConfig` and combinations with other tools return HTTP 400. Payload rules run once on the final request. If a rule removes or changes the requested grounding spec, submission returns HTTP 400.

The upstream decides whether to search. Verified response references and supported text spans are returned as candidate `groundingMetadata` when present, in the final frame for streaming. A retrieved document reference alone cannot identify the source tool. The adapter leaves `webSearchQueries` and `searchEntryPoint` absent. The Google Search request and response path has synthetic HTTP coverage, with live Plus Search acceptance pending.

## Configuration and resource isolation

```yaml
upstream:
  gemini-enterprise-web:
    enabled: false
    max-concurrent: 2
    max-inflight-bytes: 268435456
```

Concurrency is 1–16; budget is 64–4096 MiB. The configuration supports the v8 configuration API and existing file watcher. Changes require no restart. Saving a Cookie never enables the channel. Disabling admission immediately rejects new media requests with 503, while accepted requests finish naturally. Fast disable/re-enable and lower limits preserve existing reservations and per-Cookie ownership. History-buffer growth uses the accepted lease's budget ceiling and charges all live reservations; new admissions use the current limit. Occupancy may temporarily exceed a newly lowered limit until accepted work finishes.

The runtime belongs to the existing Auth Manager. It retains independent global concurrency and buffer reservations across executor/config replacement. Each credential and each distinct secure Cookie can own at most one business request. Busy requests receive 429 immediately; there is no media queue. Text requests do not acquire these reservations.

Before request logging can read the body, a bounded incremental scanner recognizes media only in native content parts, authenticates the client and acquires admission. Schema/argument fields with the same name do not divert text. Unchanged request bodies reuse that classification; interceptor replacements are checked again. An unclassified prefix uses at most 64 KiB in memory, then a temporary file that is cleaned on every exit. Unknown Content-Length conservatively reserves the maximum envelope. Incorrect declared lengths are rejected. Compressed native Enterprise bodies are rejected with 415 to preserve these read-time guarantees.

The reservation includes request storage, parsed Base64, decoded bytes, upload framing and delivery headroom: `2 × wire bytes + 0.75 × wire bytes + 32 MiB`. It is an allocation/admission envelope, not an RSS meter. Unknown length therefore needs about 219 MiB even for a small attachment. The per-request reservation survives downstream delivery. At the default budget, large simultaneous requests can hit the byte limit before reaching concurrency 2. Existing reservations use their original snapshot after a limit change.

Uploads stream decoded bytes through the Plus resumable upload protocol. Media bodies are excluded from request/session hashing and retained logging; request logs contain a bounded omission summary. Upstream output is bounded, malformed/truncated/failed output never receives an invented successful terminal event, and cancellation releases all claims and leases. Ordinary text keeps its existing session handling.

## Cookie credentials, available models and shared aliases

Use **OAuth Login → Gemini Enterprise Web** to add a Cookie credential. Edit existing credentials from their credential cards. The channel switch and resource limits are in **Configuration** only. Provide a label, the Plus HTTPS page URL at `vertexaisearch.cloud.google.com`, Google SID and SAPISID family Cookies, and optional User-Agent/proxy/priority. The credential editor does not accept model mappings or an editable model list.

Manual **Verify Cookie** authenticates and refreshes the account's upstream available-model catalog without generating content. The safe catalog snapshot contains actual IDs, display names and its observation time, bound to the Cookie/page fingerprint. Replacing authentication material clears that snapshot. Model lists and the OAuth alias editor read the observed catalog without contacting Google automatically. Listed models and successful media tests remain distinct evidence.

Manage every public-name-to-upstream-ID mapping in **OAuth model aliases → Gemini Enterprise Web**, using the existing v8 `oauth.model-alias.gemini-enterprise-web` configuration, hot reload, fork/force-mapping and prefix semantics. Raw available upstream IDs work without a self-alias rule. Example:

```yaml
oauth:
  model-alias:
    gemini-enterprise-web:
      - name: gemini-3.5-flash
        alias: existing-public-model
        fork: true
```

Alias direction follows the existing editor: `name` is the real upstream ID; `alias` is the public model name. Web ignores credential-local OAuth aliases. No display-name guessing or Auto fallback is performed. The unreleased local 8214 credential's six former identity mappings were replaced by its six observed upstream models; identity aliases are omitted by the existing shared alias normalizer. Move any nonidentity mappings from an earlier local build to the shared configuration before refreshing/saving that credential. Legacy `models` fields are not used and are removed on its next normal credential save/verification.

The URL supplies the region, configuration ID and optional Google account index. For example, `/ca/home/cid/<id>` selects `ca-discoveryengine.clients6.google.com` and `locations/ca`; `/home/cid/<id>` selects `global`. Treating every page as global causes a configuration-not-found response for regional apps. Business Cookies and its JWT protocol are outside this provider's scope.

The dedicated form reads only safe credential summaries. Cookies are write-only, are not refilled or persisted in browser storage, and are removed from the draft after successful submission, closing or changing connection. Omitting Cookies on edit preserves existing values. Generic proxy/priority and status edits use the same credential mutation path; generic raw credential upload cannot bypass the dedicated Web endpoint.

| v8 management route | Purpose |
|---|---|
| `POST /credentials/gemini-enterprise-web` | Create a Cookie credential; does not enable the channel |
| `PATCH /credentials/gemini-enterprise-web` | Edit by `name` and `expected_revision`; omitted Cookies are retained |
| `GET /credentials/gemini-enterprise-web/status` | Read effective settings, active requests, reserved bytes and rejection counters; no Google call |
| Existing credential list/status/delete/refresh routes | Safe summaries, credential enable/disable, deletion and manual authentication/catalog refresh |
| `GET /credentials/models?name=...` | Models backed by the observed upstream catalog, with shared aliases and actual upstream IDs |
| `GET /routing/model-definitions/gemini-enterprise-web` | Actual observed upstream IDs for the shared alias editor; no generation or Google call |
| `PUT /config/oauth/model-alias` | Existing shared OAuth alias configuration, including the Web provider |
| `POST /credentials/gemini-enterprise-web/models/test` | Credential-pinned image-and-text-attachment generation test |
| `PATCH /config/upstream/gemini-enterprise-web` | Shared visual/YAML configuration source |

All routes above are relative to `/v8/management`. No v0 route or fallback was added. Example creation fields:

```json
{"label":"Web input","cookies":"SID=<cookie>; SAPISID=<cookie>; ...","page_url":"https://vertexaisearch.cloud.google.com/ca/home/cid/<config-id>","model_policy":"exclude_argon"}
```

Updates require the persisted revision returned in the safe summary. Stale edits return 409. The existing Store persists Cookie material in the normal protected credential file/backend; it is a secret at rest in that store and is not returned by ordinary lists or edit forms. Explicit administrative credential export remains an existing privileged operation. Revision checks also compare the persisted local source before a store write; this does not add a distributed transaction between separately running proxy processes.

Authentication reads the Plus bootstrap page, obtains its API configuration, and verifies access through `lookupWidgetConfig`. First-party SAPISID signatures are calculated per HTTP request. Bootstrap authentication metadata is cached only in memory and invalidated when authentication/proxy material changes, including full Store reloads. The safe available-model snapshot is persisted with its credential. No OAuth JWT is generated. Late results from replaced/deleted credentials cannot repopulate authentication/model evidence. Runtime status polling does not initiate verification.

The server can renew an existing session from the secure, root-path `.google.com` values `SIDCC`, `__Secure-1PSIDCC`, and `__Secure-3PSIDCC` returned by the Plus bootstrap. It first verifies the candidate with `lookupWidgetConfig`, then saves it under the credential revision. A rejected candidate is discarded and a newer manual edit takes precedence. Per-request proxy overrides and revision-pinned model tests do not save rotations.

The server checks enabled Web credentials when the channel starts or a new Cookie is installed, then approximately every 10 minutes with a stable offset per credential. Each scheduled check calls Google Accounts `RotateCookies` and verifies any new `__Secure-1PSIDTS` and `__Secure-3PSIDTS` values on the Plus page and directory before saving them with the credential revision. An unchanged rotation result verifies the existing Cookie without increasing the revision. Manual verification and verified bootstrap Cookie updates preserve the scheduled deadline. The server checks one credential at a time and defers busy credentials. Checks use authentication and directory lookup only.

A rotation 401 or a rejected rotated candidate triggers a separate Plus check with the saved Cookie. If that succeeds, the credential stays eligible and shows **Renewal will be retried**. Temporary failures, permission failures and degraded renewal schedule another check after five minutes, or later if Google supplies a longer valid `Retry-After`. Only a confirmed 401 or Google sign-in redirect for the saved Plus session pauses checks and removes the credential from request selection. Replacement or successful manual verification restores eligibility. The credential summary reports the last check, last verified authentication, last committed renewal and next scheduled check, when known. Closing the channel stops new scheduled checks.

## Manual credential classification

Every Web credential must be manually classified. No subscription API, upstream model list, authentication check or successful generation infers its class. This is independent of the shared OAuth model alias configuration.

| Saved `enterprise_web.model_policy` | Allowed upstream model IDs |
|---|---|
| `argon_only` | Only `gemini-4-argon` |
| `exclude_argon` | Supported upstream catalog models except `gemini-4-argon` |
| Missing | Unclassified; generation is paused until a human chooses a class |

POST requires an explicit class; PATCH preserves an omitted class and rejects invalid values, including null, `auto`, `catalog`, `custom` and `unclassified`. Safe summaries normalize missing values to `model_policy: unclassified`; old files do not need a bulk migration. They also return `effective_model_ids`, the intersection of the observed catalog, manual class and global exclusions. `available_models` retains the complete upstream catalog. The alias editor continues to show the upstream catalog, including models outside a particular credential's class.

Choose the class in the OAuth Cookie form or dedicated credential editor. Cards show the class and distinguish upstream catalog count from allowed model count; credential lists offer a class filter and localized search. Unclassified credentials remain editable and can verify Cookies without generation. Cookie replacement preserves the manually chosen class; confirm it when switching accounts. Missing classification support on an older backend is shown explicitly.

Aliases, prefixes and thinking suffixes are resolved before checking the actual upstream ID. Web selection filters classes before priorities, rotation and retry; pinned requests and model tests cannot bypass it. A pending generation returns a clear 503, a pinned policy mismatch returns 400, and a model-test request on an unclassified credential returns 409. A media request with no eligible Web credential never falls back to the text provider. Classification does not add work or network calls to ordinary Enterprise text execution.

Classification edits use the existing credential revision and persisted-source checks, then republish allowed models and scheduling state. They preserve Cookie authentication caches, the raw catalog and resource leases. Before authentication/upload and at final generation submission, execution validates the manual policy and exclusions. A new class that disallows a not-yet-submitted generation causes 409; an already submitted generation completes naturally. Generation evidence has a separate epoch from authentication, so late results cannot be attributed to a new classification, including watcher edits. The final classification/revision barriers run after optional token-counter initialization, immediately before submission. Safe credential summaries include the custom User-Agent so ordinary metadata edits do not accidentally change the authentication fingerprint; Cookie values remain write-only.

## Model availability tests

The model popup offers the same single-model, sequential-batch, cancel, elapsed-time, error-detail and cooldown controls as Enterprise. Web tests pin the selected credential, revision and expected actual upstream ID. They upload one generated three-color image and one text attachment with a private random code, then generate once. The prompt contains neither expected code nor color answers. A completed response must correctly read both inputs; a plain `OK` or wrong colors fails. Tests consume upstream usage and run only after an explicit user action.

The test endpoint accepts `name`, `auth_index`, public `model`, `expected_upstream_model`, and `expected_revision`. Credential/alias changes return 409. Disabling the Web channel blocks tests. Cancellation uses the normal request context and admission leases. The result identifies `provider: gemini-enterprise-web` and `test_kind: inline_media`; it does not claim OAuth collection/resource binding. Web does not show the old Enterprise OAuth, quota or region-policy panels.

## Execution and response behavior

The existing Auth Manager, selector fallback, credential Store, watcher, proxy and model aliases are reused. Before session creation, a Web authentication failure or a Google Search mode mismatch can advance to one other eligible credential, subject to the configured credential limit. The backup must belong to the same model policy and use a distinct Cookie session. Pinned requests remain on the selected credential. One selected Cookie creates a fresh Web session, uploads every attachment under the resolved engine/session resource with resumable `uploadFile`, then sends exactly one `widgetStreamAssist`. A failed upload prevents generation. An uncertain submission is marked as started and is not replayed with another Cookie. Single-turn media bypasses conversation affinity/hash derivation while retaining the configured account selection strategy. The native Gemini handler owns SSE framing. Executor chunks contain JSON once.

User payload rules are evaluated once on the final assist business payload, after built-in construction. Read-only validation then enforces the selected model, configuration, session and uploaded-file bindings. Rules may not silently change those bindings. Necessary HTTP framing/signing follows this barrier. API responses, stream errors and usage records retain the independent Web provider and actual selected model. Text estimates use the local accounting described above; attachment and complete totals remain unknown, and Base64 length is never a tokenizer estimate. Read-time admission errors reuse the executor's safe error type, reason and field-path contract. Positively identified media rejected before admission contributes one preflight rejection, without model usage.

The reference implementation is AIStudio2API commit `ee1fa3ac63924886efbead9f50f6c49d256f2574`. Protocol/parser/stream validation code was adapted under its MIT license, retained at `internal/runtime/executor/helps/enterpriseweb/NOTICE`. The reference checkout remains unchanged.

## Local checks and synthetic browser fixture

The companion frontend branch has the same name, `codex/gemini-enterprise-web-phase1`. OAuth creation, safe credential editing and Configuration use the same v8 contract and cache invalidation. Only Configuration contains the channel controls. Unknown/error/unsupported status never claims successful shutdown. Provider names, icons, themes, filters, cards, dashboard counts, manual validation, mappings, aggregate unknown usage, search, YAML round trips and six locales are included.

For local synthetic UI checks, build the companion frontend and run `go run ./examples/gemini-enterprise-web-fixture`. It binds only `127.0.0.1:8847`, uses an isolated temporary auth/config directory, serves the local frontend build and replaces all Google traffic with a synthetic transport. Its printed fixture key and Cookies are test-only. The hold/release endpoints allow inspecting a disabled channel with one accepted request still in flight. This example is not a production entrypoint.

The example supports `--live-google` only for an explicitly authorized real local acceptance, and `--data-dir <directory>` to retain the isolated configuration/credential Store across local restarts. Real Cookie material must remain outside Git. The 8214 acceptance instance is loopback-only and was left enabled at the user's request for manual follow-up. Other installations retain the disabled default.

See [the local verification report](gemini-enterprise-web-phase1-verification.md) for actual measurements, commands, coverage and remaining real-service acceptance.
