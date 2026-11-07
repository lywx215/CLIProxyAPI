# Gemini Enterprise Web phase-one verification

Local verification date: 2026-11-03. Both repositories use `codex/gemini-enterprise-web-phase1`. Backend baseline: `bc1d8e9d9a47ae51952d1aee971f1f8c5120ae68`; frontend baseline: `283f069f21b082f9e3b1d6a641c0b251a9b54282`. Existing unrelated working-tree changes were retained. No commit, push or deployment was performed. AIStudio2API remains read-only. The user subsequently authorized real Plus upload/generation and creation of a local credential from ixBrowser profile 8214; those API-only results are recorded below.

## Acceptance commands

- `go test ./...`: passed after the shared-alias/catalog/model-test changes; latest complete output: `temp/enterprise-web-classification-go-full-complete.log`.
- `go build -o <local-temp>/cli-proxy-api-enterprise-web-classification.exe ./cmd/server` and the local acceptance fixture build: passed.
- Companion frontend `bun run verify`: passed; 1,986 tests, zero failures, lint without warnings and production single-file build successful.
- `git diff --check` in both repositories: passed.
- Deterministic concurrency, shutdown/reopen/lower-limit, cancellation, replacement, resource reclamation and persisted-revision tests: passed.
- An instrumented `go test -race` was attempted but could not execute: this Windows toolchain has `CGO_ENABLED=0` and no GCC. This is not recorded as a race-detector pass.

Raw local outputs are intentionally ignored by Git. The latest frontend output is `<local-temp>/enterprise-web-classification-frontend-verify.log`. The first latest full Go run exposed an obsolete disabled-channel assertion, which was corrected to expect 503, and one failure in the existing `TestEnterpriseCatalogPreservesCooldownAndFencesRevision`. That existing test passed an immediate focused rerun and the final full rerun without a change to its implementation. The failed output remains in `temp/enterprise-web-global-models-go-full.log`; it is not counted as a pass. Source-file tests and the synthetic fixture are reproducible without credentials or Google access. Vite emitted its existing advisory about `__dirname` with a future native config loader; the build completed successfully.

## Protocol, lifecycle and resource coverage

Actual native HTTP and Auth Manager tests cover text versus media routing, TXT/PDF/image inputs, several attachments sharing one Cookie/session and one generation, unlisted-model/disabled-channel failures, shared alias projection and credential-local alias exclusion, unsupported histories, token counting rejection, malformed Base64/MIME/count/image-output validation, payload-rule ordering and binding preservation. Upload failure prevents generation; uncertain submission is not replayed even with two credentials and configured retries. Synchronous and streaming truncation/failure/cancellation cannot invent STOP. New model-test checks require both correct generated-image colors and the random attachment code; text-only success, wrong colors, stale revisions and stale upstream model bindings fail.

The shared runtime tests use barriers and a controllable clock for admission/disable races, fast reopen, lower limits, capacity oversubscription, independent Cookie claims, bootstrap-cache expiry and credential replacement. Client authentication occurs once before media admission. Prefixes larger than 64 KiB spill to a cleaned temporary file; escaped inline keys and fragment boundaries are covered. Incorrect lengths cannot bypass the reservation. Ordinary Auth Manager publication, full Store reload and dedicated edits invalidate changed authentication material; late results are excluded, while an unchanged reload or label edit retains evidence. Native HTTP tests parse each SSE data frame as JSON and require exactly one successful completion.

The management tests use a real temporary FileStore. They cover safe Cookie CRUD, omitted-Cookie preservation, stale persisted revisions, authentication/version replacement, common-field restrictions, enable/disable/delete, authentication-only verification without generation, readonly status, hot disable/drain/reopen and explicit zero/false handling. Plus verifies its bootstrap and configuration access; it does not generate an OAuth JWT. Resumable uploads reject an untrusted destination before sending any Cookie or file bytes.

A streaming discard Mock consumes decoded resumable uploads without retaining another full copy. Request logging is enabled for all six size/length cases below. Each completed with one upload and one generation, bounded logs, and zero retained admission bytes/requests. An initial measurement found excessive generic session-identity JSON copies; single-turn media now bypasses that hashing/affinity path. Plus rechecks sampled approximately 117.2/118.2 MiB for the two 50 MiB cases and returned goroutines from 4 to 4.

| Decoded attachment MiB | Content-Length | Wire MiB | Reserved MiB | Sampled heap increase MiB | Allocated MiB | Goroutines before → after |
|---|---|---:|---:|---:|---:|---|
| 1 | Known | 1.33 | 35.67 | 4.27 | 4.27 | 3 → 4 |
| 1 | Unknown | 1.33 | 219.00 | 70.85 | 70.86 | 4 → 4 |
| 10 | Known | 13.33 | 68.67 | 37.26 | 37.26 | 4 → 4 |
| 10 | Unknown | 13.33 | 219.00 | 91.85 | 91.86 | 4 → 4 |
| 50 | Known | 66.67 | 215.33 | 117.23 | 183.95 | 4 → 4 |
| 50 | Unknown | 66.67 | 219.00 | 118.27 | 185.20 | 4 → 4 |

The first request initializes the shared usage worker once; subsequent requests retain no extra goroutines. Tests also enforce that sampled server heap stays within the reserved envelope. Heap samples are process-local observations at transport/delivery boundaries, not continuous OS RSS measurements. The admission byte counter records reserved processing envelopes rather than currently resident pages. Go GC and ordinary text work can make process-wide memory differ from that counter.

## Controlled ordinary-text performance before manual classification

Windows amd64, Go 1.26, Intel i7-12700. The source archive at baseline `bc1d8e9` and the final shared-alias implementation run the same portable native HTTP/credential-selection/Enterprise-policy benchmark with identical immediate Mock generation. The baseline has no Web hook. The final version registers a Web credential with an observed catalog and runs admission middleware, separately disabled/enabled. Google and TCP/model latency are excluded.

Each benchmark process and children are pinned to CPU 0, with `-test.cpu=1`, 500 ms calibration and five repetitions. Per-request p95 uses Windows QueryPerformanceCounter. The escaped case contains actual six-byte JSON `\u4f60` sequences at both wire sizes; an earlier fixture accidentally used literal UTF-8 Chinese, so its old escaped-case labels/results are superseded by this corrected comparison. The latest baseline and after runs did not overlap builds or regression-test processes. Baseline and final binaries were run consecutively, so desktop scheduling and ordering noise remain possible.

| Revision / mode | Text encoding | Text wire KiB | Median p95 µs | Requests/s | Bytes/op | Allocs/op | Median p95 change |
|---|---|---:|---:|---:|---:|---:|---:|
| baseline | ascii | 1 | 152.3 | 10366 | 57748 | 479 | +0.00% |
| baseline | ascii | 32 | 650.5 | 1984 | 215968 | 490 | +0.00% |
| baseline | escaped | 1 | 158.9 | 10145 | 57940 | 480 | +0.00% |
| baseline | escaped | 32 | 766.7 | 1837 | 220065 | 491 | +0.00% |
| Web off | ascii | 1 | 164.1 | 9527 | 61548 | 490 | +7.75% |
| Web off | ascii | 32 | 630.7 | 2023 | 172916 | 490 | -3.04% |
| Web on | ascii | 1 | 158.1 | 9955 | 61548 | 490 | +3.81% |
| Web on | ascii | 32 | 622.3 | 2011 | 172916 | 490 | -4.34% |
| Web off | escaped | 1 | 166.0 | 9629 | 62092 | 496 | +4.47% |
| Web off | escaped | 32 | 801.2 | 1726 | 177365 | 496 | +4.50% |
| Web on | escaped | 1 | 167.2 | 9647 | 62092 | 496 | +5.22% |
| Web on | escaped | 32 | 799.8 | 1712 | 177365 | 496 | +4.32% |

The shared-alias comparison does **not** completely meet the 5% acceptance target: ASCII 1 KiB with Web off regresses +7.75%, and escaped 1 KiB with Web on regresses +5.22%. ASCII 1 KiB baseline p95 ranges from 151.8 to 157.9 µs, off from 162.7 to 168.0 µs, and on from 156.2 to 159.9 µs. Escaped 1 KiB baseline ranges from 157.3 to 165.3 µs and on from 164.1 to 168.4 µs. These are five-run medians with measurable scheduling/order variation; faster enabled rows do not prove that enabling Web accelerates text. An earlier complete comparison recorded a worst median of +0.67%, but the shared-alias and subsequent manual-classification results supersede that earlier acceptance claim. No repeat-until-pass measurement is substituted for these results.

ASCII 1 KiB adds 3,800 bytes / 11 allocations; ASCII 32 KiB removes about 43,051 bytes with the same allocation count. Corrected escaped 1 KiB adds 4,152 bytes / 16 allocations; escaped 32 KiB removes about 42,700 bytes / adds 5 allocations. The media signature/auth/upload changes do not add network operations to ordinary text.

Raw shared-alias files are `temp/enterprise-web-global-models-performance-baseline.txt`, `temp/enterprise-web-global-models-performance-after.txt` and `temp/enterprise-web-global-models-performance.json`. Historical results remain in `temp/enterprise-plus-performance-baseline.txt`, `temp/enterprise-plus-performance-after-clean.txt`, `temp/enterprise-plus-performance.json` and the earlier overlapped trial `temp/enterprise-plus-performance-after.txt`. Source archives and test artifacts are local only. During compilation the workspace volume lacked space; only this task's inactive build artifacts were removed, and benchmark/server outputs were moved to the current user's temporary directory on C:.

## Mixed-load observations

Two actual Web executor requests each upload a 10 MiB attachment and wait at the synthetic generation boundary. Native 32 KiB text requests continue through the ordinary Enterprise executor. A third Web request immediately receives 429. Both accepted requests finish with STOP after release, and all reservations drain. Request logging is enabled. Historical pre-Plus repetitions, 100 idle and 100 overlapping text requests each:

| Run | Idle text p95 ms | Mixed text p95 ms | Change | Sampled mixed heap increase MiB |
|---|---:|---:|---:|---:|
| 1 | 7.131 | 2.866 | -59.8% | 73.65 |
| 2 | 8.065 | 4.535 | -43.8% | 102.81 |
| 3 | 4.914 | 2.944 | -40.1% | 102.81 |
| Plus mock, during parallel regression processes | 3.4923 | 7.8027 | +123.4% | 74.07 |

The slot-isolation and reclamation assertions pass. These shorter file-logging measurements have substantial warm-up/GC/filesystem noise: cold idle requests can be slower than later overlapping requests. An earlier trial before the final classifier change also had a +26.1% individual p95 result (2.9735 → 3.7506 ms); it is retained as noise evidence, not used to replace the controlled text comparison. They do **not** establish a ≤5% mixed-load tail guarantee. The latest ordinary-text comparison also has two cases above its target; sustained mixed workload, OS RSS and real upstream latency remain separate performance acceptance work. The repository default remains disabled; the authorized local manual-acceptance instance is enabled at the user's request.

## Frontend correspondence

| Area | Implemented correspondence | Verification |
|---|---|---|
| Page ownership | Cookie creation is on OAuth Login; existing cards open the safe editor; only Configuration contains switch/concurrency/budget | New route/source/render tests passed; the old credential settings card was removed |
| Effective switch | Pending-operation guard, confirmed backend state, unknown/unsupported handling, in-flight drain display | Browser accepted one held request, switched off, observed active=1/draining, rejected the next request with 503, then observed active=0 |
| YAML / search | Three canonical paths, explicit false preserved, unrelated fields retained | Visual-config tests and browser search/diff/save; diff retained `enabled: false` |
| Cookie form | Dedicated editor, blank secret, URL parsing, optional proxy/UA/priority, draft retention; no model mapping fields | Form/serializer tests reject credential mapping writes; API-only Cookie replacement preserved credential identity and updated revision |
| Mapping operations | OAuth model alias editor is the only mapping entry; actual catalog IDs, global alias/prefix/exclusion rules | Projection tests and a real temporary alias save → list → media test → native generation → scoped cleanup passed |
| Types / provider presentation | DTO allowlists, names, icons, filters, credential cards and dashboard counts | Unit tests and browser credential/dashboard views |
| Common actions | Existing single/bulk status/delete, priority/proxy controls and refresh-all include Web | Management tests; browser bulk disable/enable, authentication-only refresh-all and delete |
| Authentication versus generation | Verification authenticates and updates catalog; popup offers explicit image/TXT tests without old OAuth/region panels | No-generation refresh tests; semantic media regressions and one real media test; sequential batch/cancel controls reuse existing tested behavior |
| Logs / usage | Independent provider and selected actual model, media log summary, unknown token details and aggregate cards | Native logging tests, usage normalization and model/API card render tests |
| Quota / resources | Local byte reservations separately explained; old OAuth quota panels excluded | Provider capability tests and visible settings help |
| Connection/error handling | Connection revisions, stale-response guards, draft retention, status error/404 without v0 retry | API tests and browser logout; unsupported polling stops until a refresh/connection change |
| Locales / accessibility | Six locale namespaces, labelled controls, shared alias controls, media-test help and live status | Locale-key parity and latest form/modal render checks; historical desktop/narrow-screen checks predate final layout |
| Themes / cleanup | Light/dark styling, no browser Cookie persistence, closed-form cleanup | Screenshots and synthetic edit/logout flow; fixture stopped and temporary viewport reset |

Historical browser screenshot artifacts: `temp/enterprise-web-phase1-desktop.png`, `temp/enterprise-web-phase1-narrow.png`, `temp/enterprise-web-phase1-narrow-editor.png`, and `temp/enterprise-web-phase1-dark.png`. These used synthetic Cookies and precede the move to OAuth Login and the Plus-only form. The user subsequently required API operations without GUI, so the latest routes/layout were checked through source/render tests and the served build, not a new browser interaction. Desktop/narrow-screen and full keyboard/screen-reader checks of the final layout remain for manual confirmation.

## Real Plus acceptance through APIs

The account was obtained only from the explicitly authorized ixBrowser profile 8214 through its local API/CDP cookie API. No GUI interaction was used in the acceptance. Cookie material stayed in memory during transfer to the loopback management endpoint and was saved through the ordinary credential Store outside Git. The local server ran the real Auth Manager, native Gemini handler, admission middleware, management API and Web executor with real Google HTTP transport; synthetic transport was disabled explicitly.

The supplied `/ca/home/cid/...` app uses the `ca` regional endpoint. Querying `global` returned a configuration-not-found response. Reading the bootstrap region and selecting `ca-discoveryengine.clients6.google.com/v1alpha/locations/ca` succeeded. `lookupWidgetConfig` supplied the real engine and six explicit model IDs; only `gemini-3.5-flash` has generation evidence in this acceptance.

| System request | Result | Elapsed | Semantic checks |
|---|---|---:|---|
| Native `generateContent`, one Content with text + PNG + TXT | HTTP 200, STOP | 10.359 s | Correct red circle at upper left, blue square at upper right, green triangle at bottom center; exact randomized TXT code |
| Native `streamGenerateContent?alt=sse`, one Content with text + PNG + PDF | HTTP 200, valid JSON SSE, one STOP | 10.495 s | Same correct image description and exact randomized PDF code |

The second request was sent after restarting the local process and reloading the saved credential; no browser cookie fetch or reinjection was needed for that request. Integration exposed and fixed duplicate SSE `data:` framing and a missing Web-model-index rebuild on Store load. Both received regression tests before the successful retest. Earlier unsuccessful attempts remain in the ignored evidence JSON rather than being reported as passes.

An independent direct-API upload/generation also succeeded with two uploads and one `widgetStreamAssist`. A subsequent read-only session lookup confirmed `queryConfig.google.discoveryengine.googleapis.com.Assistant.model_id = gemini-3.5-flash`. This direct probe is separate from the two native-system requests above.

Evidence: `temp/enterprise-plus-8214-api/system-verification.json`, `system-stream-response.txt`, `full-flow.json` and `lookup.json`. These contain test results and safe credential summaries, no Cookie/authorization values. The local credential is named `Enterprise Plus · ixBrowser 8214`; its six initial identity mappings have now been replaced by its six observed upstream models. Safe list/create responses did not echo Cookies. Manual verification succeeded, and saving the credential preserved the then-disabled channel state.

At the user's final instruction, the isolated loopback instance at `http://127.0.0.1:8847` remains enabled with concurrency 2, budget 256 MiB, one request per Cookie, and zero in-flight requests/reserved bytes at the final check. The configuration and credential are retained outside Git under the current user's LocalAppData directory for manual follow-up. This explicitly enabled acceptance instance does not change the repository default `enabled: false`.

## Shared aliases and model-test API acceptance

The latest change was checked through local APIs only. Its first Cookie refresh encountered an upstream login redirect (302) before generation. The explicitly authorized ixBrowser 8214 account was still signed in: its current session read the Plus page through the browser API request interface. Current Cookies were transferred in memory to the dedicated loopback PATCH endpoint, moving the existing credential from revision 1 to 2. Verification then returned six upstream models. This was an explicit replacement; the server does not synchronize or renew browser Cookies automatically.

| Latest system request | Result | Elapsed / evidence |
|---|---|---|
| Cookie verification / catalog | HTTP 200, six actual IDs | No generation; safe summary has no `models` mapping field |
| OAuth alias editor definitions | HTTP 200, same six upstream IDs | Read-only cached catalog; no Google call |
| Save temporary global alias → `gemini-3.5-flash` | HTTP 200; list shows alias and actual ID | Shared OAuth alias API, not credential metadata |
| Media model test through alias | HTTP 200, `status=ok`, `test_kind=inline_media` | 8.724 s server elapsed; correct image colors and exact random TXT code |
| Outdated expected upstream ID | HTTP 409 | Rejected before generation |
| Native Gemini text + PNG + TXT through alias | HTTP 200, STOP | 8.118 s; correct shapes/positions and exact random TXT code |
| Scoped temporary-alias cleanup | HTTP 200 | Six raw IDs remain; shared alias map restored to its initial empty state |

Both successful model calls used `gemini-3.5-flash`; the other five listed models were not generated against. The saved credential has no legacy per-credential mapping field, its persisted catalog contains six entries, and the served frontend hash matches the latest build. Evidence: `temp/enterprise-web-global-models-verification.json`. The initial authentication failure remains in `temp/enterprise-web-global-models-initial-expired-cookie.json`.

## Manual-classification acceptance

The follow-up adds only manually selected `argon_only` and `exclude_argon`. Missing metadata remains unclassified. Cookie authentication, model catalogs and previous generation evidence never infer a class. Shared aliases remain global. Safe summaries retain the full `available_models` catalog and add the current class and `effective_model_ids` after policy/exclusions.

- Final complete `go test ./...`: passed (`temp/enterprise-web-classification-go-full-complete.log`); main server, local fixture and benchmark builds passed. A second full run includes the new deterministic source-reload test.
- Frontend `bun run verify`: passed, 1,986 tests, zero failures, lint and TypeScript/production build passed. The existing Vite config-loader advisory remains.
- Selection tests cover round-robin, fill-first, weighted selection, prefixed shared aliases, thinking suffixes, over-broad stale registry state, priorities, pinned requests, pre-submission retry and global exclusions. No disallowed class reaches execution.
- Deterministic transport barriers cover classification changes during authentication, after an upload and after generation submission. Disallowed unfinished requests return 409; submitted requests finish. Cookie caches, raw catalogs and resource leases survive a class-only change, and old generation evidence is fenced independently of authentication.
- Management checks cover required/invalid/null classes, legacy pending credentials, omitted-policy preservation on Cookie replacement, persisted revision/source conflicts, read-only catalogs, authentication-only verification while unclassified and source reload without timer sleeps.
- Frontend checks cover normalization of legacy/unsupported replies, required selection, revision-bound serialization, localized search and class filtering, raw-versus-effective counts, pending guidance, disabled tests and policy-sensitive result identity. Bulk deletion under a class filter receives only the visible credential names. No GUI was used; final layout/accessibility checks remain manual.

The API-only synthetic instance on loopback port 8851 exercised two real management credentials with distinct synthetic Cookies, the real Store/Auth Manager and mock upstream transport. Argon and non-Argon shared aliases each generated successfully through native Gemini. A pinned wrong-class test returned 400; after changing the sole Argon credential to non-Argon, an Argon media request returned 503. The upstream call counters stayed unchanged for both rejections. Null policy returned 400, stale revision returned 409, raw catalogs remained intact, and authentication remained cached through policy edits. Temporary credentials/aliases were removed and the synthetic process was stopped. Evidence: `temp/enterprise-web-classification-api.json`.

The real local acceptance instance was drained before updating its process, then its channel was restored to **enabled**. The saved 8214 credential remains enabled but **unclassified**, at revision 2, without adding a policy field to the legacy file. It retains six upstream definitions and has zero effective callable models. A native inline attachment request returned 503 with classification guidance; a model-test request returned 409 `model_policy_unclassified`. Neither performed real generation. The served frontend hash matches the new build, and final active requests/reserved bytes are zero. Evidence: `temp/enterprise-web-classification-local-state.json`. The new local process is separate from production; no deployment, commit or push occurred.

### Latest text-performance rerun

The final classification binary and the same original baseline were each pinned to CPU 0, using identical Mock native HTTP/Enterprise text execution, `-test.cpu=1`, 500 ms calibration, and five repetitions per case. Builds and regression tests had finished before measurement. Baseline ran first, followed by final Web-off/Web-on; ordinary desktop work and the idle local acceptance process remain possible noise sources.

| Revision / mode | Encoding | Wire KiB | Median p95 µs | Requests/s | Bytes/op | Allocs/op | p95 change |
|---|---|---:|---:|---:|---:|---:|---:|
| baseline | ascii | 1 | 151.3 | 10504 | 57748 | 479 | +0.00% |
| baseline | ascii | 32 | 641.5 | 2013 | 215969 | 490 | +0.00% |
| baseline | escaped | 1 | 152.5 | 10476 | 57940 | 480 | +0.00% |
| baseline | escaped | 32 | 754.4 | 1884 | 220066 | 491 | +0.00% |
| Web off | ascii | 1 | 154.1 | 10252 | 61548 | 490 | +1.85% |
| Web off | ascii | 32 | 615.9 | 2053 | 172915 | 490 | -3.99% |
| Web on | ascii | 1 | 153.9 | 10417 | 61548 | 490 | +1.72% |
| Web on | ascii | 32 | 603.1 | 2076 | 172916 | 490 | -5.99% |
| Web off | escaped | 1 | 162.0 | 9938 | 62092 | 496 | +6.23% |
| Web off | escaped | 32 | 780.7 | 1763 | 177365 | 496 | +3.49% |
| Web on | escaped | 1 | 160.2 | 10016 | 62092 | 496 | +5.05% |
| Web on | escaped | 32 | 777.1 | 1759 | 177365 | 496 | +3.01% |

The latest worst median p95 regression is **+6.23%**, escaped 1 KiB with Web off; Web on is **+5.05%** in that case. Baseline p95 ranges from 151.7–154.5 µs, off from 160.6–168.9 µs, and on from 158.9–161.8 µs. The ≤5% target therefore remains **not fully met**. The prior +7.75% shared-alias result remains a failed-target observation; neither it nor the older +0.67% run is hidden or replaced by a claim of stable compliance. No text authentication/subscription network calls were added by classification.

Raw files: `temp/enterprise-web-classification-performance-baseline.txt`, `temp/enterprise-web-classification-performance-after.txt`, and `temp/enterprise-web-classification-performance.json`. One initial invocation used incorrectly parsed PowerShell flags and did not run benchmarks; its diagnostic is retained in `temp/enterprise-web-classification-performance-invalid-flags.txt`. The corrected invocation uses a quoted argument array. Mixed-load/RSS limits from the earlier section remain unverified; this change does not claim a new mixed-load capacity result.

## Cookie renewal recovery and credential failover

Synthetic tests verify a rotation 401 with a working saved Cookie, a rejected candidate with a working saved Cookie, confirmed expiry, no new Cookie, backoff, revision conflicts, and scheduled recovery after replacing authentication material. Streaming and non-streaming tests verify pre-session failover to one same-class credential, explicit Google Search mode checking, pinned calls, the configured retry limit, and the submission boundary. Full Go tests, server compilation and frontend `bun run verify` passed. Real dual-account failover and live Search remain separate checks.

The five-run CPU 0 interleaved comparison reused the original mock HTTP baseline. The worst median p95 change was **+5.34%** for escaped 1 KiB text with Web enabled (155.5 → 163.8 µs). The sample ranges were 150.1–159.3 µs and 158.2–168.4 µs, respectively. The 5% target remains unmet. The prior +7.75% result remains a historical failed-target observation. An unpinned exploratory run reported +38.76% in the same case and a +29.34% difference between identical baseline modes. Both runs and their raw samples remain in `temp/enterprise-web-resilience-performance*.json` and the matching raw text logs.

## Remaining acceptance

The later explicitly authorized ultra-cli2 deployment and real model calls are recorded below. Other listed models, long-running real concurrent traffic, OS RSS under sustained load and final GUI layout/accessibility remain unverified. The ≤5% ordinary-text target has not been completely met in the latest controlled run. The reverse-engineered Plus contract can change; functional acceptance is not a production capacity or long-term session-continuity result.

## Authorized ultra-cli2 deployment and profile 8219 acceptance

On 2026-11-05 the user explicitly authorized deploying local changes only to `https://ultra-cli2.zeabur.app`, followed by importing ixBrowser profile 8219 and testing real generation. Both repositories remained on `codex/gemini-enterprise-web-phase1`. No Git push or other service deployment was performed. The backend and verified frontend were packaged as a sanitized source upload addressed to the existing `u01/cliproxyapi` service. Local configuration, account files, logs and temporary diagnostics were excluded.

- Final deployment: `6aec59ea03a5d91147653970`, RUNNING at 2026-11-05 18:31:07 +08:00.
- Runtime version: `local-enterprise-web-20261105-900d66e6440b-r2`.
- Runtime commit: `bc1d8e9d9a47ae51952d1aee971f1f8c5120ae68-dirty-900d66e6440b-r2`.
- Source archive snapshot: `9c932d4a2465725be5ee1626423315dc1ef159eeedf19e6c2538ec57239c9b1f`.
- Served frontend SHA-256: `dab2066ecd0fa66f5542d7f5126e9d87ca7f0a66a41bd0e7e4313b5c91f1b8f4`, matching the verified local frontend build.

All five New API channels routing to this instance (240, 271, 272, 273, 276) were already manually disabled before maintenance. Targeted stop requests were observed reaching both active Nginx backends; the standby also logged a later database channel-cache sync. No reliable provider-wide live count was available, so maintenance used the authorized **300-second time fallback** (309.89 seconds observed), not a claim of 95% completion. These five originally disabled channels remain disabled. New API was not restarted.

The initial upload's prepare response timed out, but subsequent API inspection confirmed its deployment had been accepted. The first image exposed the new backend and matching frontend but retained stale build metadata from existing environment values. Only the staged Docker build identity was corrected; a second upload supplied the final verifiable runtime identity above. Both builds were confined to the same target and maintenance window. Environment variables, Git binding, nine original public models and three original credential records were preserved. The disabled New API channel 271 passed a real streaming `gemini-3.8-flash-enterprise` administrative channel test in 9.762 seconds. The other tracked Zeabur instances retained their original deployment IDs and commits.

The user then explicitly classified profile 8219 as **Argon-only**. Its Cookie was fetched through the existing local ixBrowser/CDP APIs and sent over HTTPS to this target's dedicated credential endpoint. Browser access was disconnected before server authentication and generation. Cookie values were excluded from normal responses and reports. The imported credential is labeled `Enterprise Plus - ixBrowser 8219 - Argon`, uses the supplied US Plus application, and is enabled. The server retained the complete seven-model catalog while exposing only `gemini-4-argon` as callable under its manual policy. No model alias was stored in the credential.

| Real Web test | Result | Evidence |
|---|---|---|
| Server-side Plus authentication and catalog | HTTP 200 | Authentication verified, seven catalog entries, one effective model |
| Management model test: native text + PNG + TXT, non-streaming | HTTP 200, `status=ok` | `gemini-4-argon`, correct private TXT nonce and red/blue/green image blocks; 12.273 seconds server elapsed |
| Initial native streaming text + PNG + PDF | HTTP 200, STOP, no error event | The response included a trailing courtesy sentence and failed whole-response exact matching. The initial fixture omitted its independent expected-nonce record, so PDF nonce correctness is not claimed for that attempt. |
| Recorded native streaming text + PNG + PDF recheck | HTTP 200, three events, STOP, no error event | Exact pre-recorded PDF nonce and ordered image colors matched; 14.071 seconds end-to-end |

The rechecked stream also appended a courtesy sentence. Media-reading acceptance passed; strict answer-only formatting did **not** pass. The original attempt and the recheck remain separately recorded. Three real Web generations were submitted in total; no uncertain or failed submission was automatically replayed.

Final profile 8219 state: revision **3**, `argon_only`, `auth_verified=true`, `cookie_check_status=verified`, last verified at **2026-11-05 18:37:57.968 +08:00**. The credential records a verified Cookie update at **18:37:57.148 +08:00** and a next scheduled check at **18:48:01.968 +08:00**. This short acceptance does not establish long-term renewal stability. Web remains **enabled**, with concurrency 2 and a 256 MiB budget; active requests and reserved bytes returned to zero. Original credentials and existing effective configuration were preserved apart from the requested Web enablement. Object-store scopes differ from the other tracked services, and their deployments remain unchanged.

The separate 8214 isolation observation remains independent: at 2026-11-05 18:43 +08:00 it was still running, with no browser reconnection or model calls. It entered confirmed `relogin_required` at **2026-11-04 23:04:37.131 +08:00** and had no automatic revision increase; sustained renewal therefore has not passed. The original local 8214 credential remains unchanged at revision 13.

Sanitized release and call evidence is exported to `temp/ultra-cli2-deploy-20261105/acceptance-summary.json`. Credential material and full private diagnostic responses are kept only in current-user DPAPI files outside the repository.

## Local text-token accounting verification (2026-11-05)

This update was developed locally on `codex/gemini-enterprise-web-phase1` in both repositories. No deployment, production inspection, browser connection, credential mutation or real model call was performed. Synthetic HTTP transports exercise native Gemini streaming and non-streaming requests through admission middleware, Auth Manager, Web uploads, generation parsing and response framing.

Passed checks:

- Web output and returned thought-summary estimates reuse Enterprise's local tokenizer. Input-text estimates read final payload-rule output, independently of full input/attachment counts. Split text and a terminal cumulative snapshot produce the same counts as the shared tokenizer; signatures and transport metadata are excluded.
- Completed responses contain one usage extension. Truncated responses and cancelled/uncertain submissions do not emit successful termination or invent complete output/total usage. Upload/session boundaries and the single generation submission remain intact; request reservations return to zero.
- Both single-turn and serialized-history native HTTP tests preserve source metadata and nullable counts, including bounded request logs with no Base64 or Cookie leakage.
- Insufficient accounting workspace and tokenizer errors leave affected estimates unknown. Snapshot export/import, deduplication, Redis queue JSON, plugin usage clones and plugin stream/non-stream parsing preserve provenance and unknown media totals.
- Ordinary Enterprise tokenizer/executor implementations were not modified. Its usage/chunk/failure, nullable-token, plugin-protocol and frontend accounting regressions passed.
- Targeted Go command: `go test ./internal/runtime/executor ./internal/runtime/executor/helps ./internal/usage ./internal/redisqueue ./internal/pluginhost ./sdk/cliproxy/usage -run 'TestEnterprise(WebUsage|WebSingleSession|WebNativeHTTP|WebSerializedNativeHTTP|UsageAcrossChunks|PluginUsage)|TestWebToken|TestTokenPresence|TestUsageToken' -count=1`.
- Server build: `go build -o <Windows TEMP>/cliproxy-enterprise-web-token-test.exe ./cmd/server` passed.
- Frontend `bun run verify` passed: 2,005 tests across 192 files, ESLint, TypeScript and Vite build. Six-language static rendering covers local-source labels, separate input text, approximate output/summary, zero versus unknown, incomplete responses, old records and unchanged ordinary Enterprise rows. Incomplete aggregate costs remain unavailable. Vite emitted its existing future-native-loader `__dirname` warning.

The literal `go test ./...` attempt did **not** pass. The workspace's concurrently added input-limit defaults are 4 MiB of text, while `TestEnterpriseWebLimitDetailSurvivesManagerBeforeUpstream` still expects rejection just above 1 MiB. `TestEnterpriseWebSerializedMixedLoadIsolation` also fails at admission with a buffer-budget 429 before token accounting. In addition, ignored `temp/web-input-limits-config-20261105/baseline/...` package copies fail to compile because they omit required types/helpers. These other changes and temporary files were left intact; this run is not a full-regression success.

Run logs are in Windows TEMP as `enterprise-web-token-targeted.log`, `enterprise-web-token-go-full.log` and `enterprise-web-token-frontend-verify.log`. Browser visual/interaction testing and real-model accounting verification were not performed under the API-only/local scope. No new performance acceptance claim is made; earlier 5.34% and 7.75% p95 failures are not superseded by these functional checks. Historical production records cannot be backfilled from this change.

## Local integration audit (2026-11-05)

Both repositories remain on `codex/gemini-enterprise-web-phase1`. This audit
integrates Cookie renewal/classification, shared model aliases, history and model
continuation, configurable input limits, Google Search, resource admission and
local token estimates. Existing unrelated working-tree edits were preserved.
No production API, fingerprint browser, real credential or real model was used.
The existing 8847 service and production services were not restarted or changed.

### Conflicts corrected

| Interaction | Correction and regression evidence |
| --- | --- |
| Credential summary and dedicated editor | Return the safe custom `user_agent` in summaries. Renaming a verified credential now round-trips its agent without clearing authentication/catalog state. Frontend PATCH omits an unknown unchanged agent on older backends; an explicit empty agent remains supported. Cookies are never returned. |
| Configurable input limits and save confirmation | Normalize all fourteen actual `input_limits` fields. Changed YAML limits require backend support before saving and matching effective values afterward. Deleting fields confirms defaults; invalid integers, old backend replies and late responses cannot appear successful. Unrelated saves remain independent of Web status. |
| Growing history buffers and hot limit changes | Additional history reservations use the accepted lease's original ceiling and account for every live lease. Lowering limits, closing/reopening or raising limits does not replace that snapshot; new admissions use current limits. Release/cancel paths return all occupancy. |
| Token-counter initialization and credential policy | Initialize optional input counting before the final revision/model-policy checks so a first tokenizer load cannot extend the stale-policy window before submission. Payload rules still run once before read-only counting/validation. |
| Admission errors and execution diagnostics | Read-time failures now use the same sanitized error formatter as executor failures, preserving the error type, fixed reason and safe field path. A confirmed media input-limit rejection before lease acquisition increments the preflight counter once; it is not a model request. Raw reader errors are not disclosed. |
| Documentation and frontend help | Remove obsolete fixed 33-message history hints in all six languages. Current defaults are 50 logical Contents, 4 MiB text and 12 attachments. Update history role/continuation behavior, local text estimates and budget semantics. History/input-limit guides are included in the deliverable file set. |

### Verification completed

- Initial source regression: `go test ./cmd/... ./internal/... ./sdk/... ./test/...` passed.
- Final **literal `go test ./...` passed**, including the previously failing
  input-limit and mixed-load cases. This supersedes the failed full-regression
  outcome in the preceding token-accounting entry for the current integrated
  tree; the older failure record is retained as historical evidence.
- `go build -o <Windows TEMP>/cliproxy-web-integration-server.exe ./cmd/server`
  passed. The isolated synthetic fixture also compiled successfully.
- Final frontend `bun run verify` passed: **2,011 tests, 193 files, 20,233
  assertions**, ESLint, TypeScript and the single-file Vite build. Vite retains
  its existing future-native-loader `__dirname` warning.
- New combined executor tests cover both streaming and non-streaming history
  attachments with a text-only latest message, Google Search, final input-text
  token estimates and shutdown/limit changes during upload. Each request uploads
  twice, creates one session, submits one generation and releases its resources.
  Existing classification, Cookie rotation/replacement, fallback, cancellation,
  alias/exclusion and ordinary Enterprise tests passed in the complete suite.
- API-only acceptance used an independent loopback fixture at port 62856 with a
  fresh temporary config/store and synthetic transport that blocks external
  network calls. Eight checks passed: default runtime status; credential
  creation/verification/edit/revision conflict; hot history/input-limit changes;
  non-streaming history media; streaming history media; read-time 413
  classification/counter; graceful draining; reopening. It submitted exactly
  three synthetic generations, with zero active requests/bytes afterward.
  The frontend normalizer accepted the actual API response and all fourteen
  effective limits. Bun emitted a directory-mismatch diagnostic after that
  standalone contract check, which exited zero; the subsequent full frontend
  verify passed.
- The fixture process was stopped after acceptance. No persistent service was
  installed. No unresolved Git merge entries or source conflict markers were
  found; scoped `git diff --check` passed in both repositories.

Evidence is retained outside the source tree in Windows TEMP:
`enterprise-web-integration-go-before.log`,
`enterprise-web-integration-go-full.log`,
`enterprise-web-integration-frontend-before.log`,
`enterprise-web-integration-frontend-verify.log`, and
`enterprise-web-integration-f46742182b0a4270af8e47a53772bfd7/api-report.json`
(plus its synthetic check script and runtime-status snapshot).

No new performance comparison, real-model acceptance or GUI verification is
claimed. The earlier **5.34% and 7.75% p95 regressions remain recorded as unmet
targets**. Passing this integration suite does not establish sustained Cookie
renewal or Google model availability, and it does not deploy these changes.

## Authorized integration deployment to ultra-cli2 (2026-11-05)

The user subsequently authorized deploying the integrated changes to
`https://ultra-cli2.zeabur.app` only. The sanitized source upload addressed the
existing `u01/cliproxyapi` service; no Git push or shared-branch deployment was
performed. The staged Linux build passed before maintenance.

- Deployment: `6aeca428381caef92e79b889`, RUNNING.
- Runtime version: `local-web-integration-20261105-744da64261b7`.
- Runtime commit header:
  `bc1d8e9d9a47ae51952d1aee971f1f8c5120ae68-dirty-744da64261b7`.
- Stamped source snapshot SHA-256:
  `6d925e9cabbc42f1597399c56a77971f73eed923a2fd7da8f5e8cf51d791ef16`.
- Served frontend SHA-256:
  `eafa396ba986410b4a2a44be17173a308a6aa658d4d9f946c6457fb052803a8e`,
  matching the locally verified build.

Channels 240, 271, 272, 273 and 276 were already disabled and remained disabled.
Scoped stop requests were confirmed at both active New API backends after
rechecking Nginx topology. Global request logs continued to change, so the
maintenance used the authorized **300-second time fallback** (303.72 seconds
observed), not verified 95% completion. The upload prepare response did not
confirm success, but platform inspection reconciled the accepted deployment;
there was exactly one upload attempt. The exclusive maintenance lease was
released only after health, functional checks and original routing state were
verified.

Management, health and model-list checks passed. The nine public model IDs,
four credential identities/enabled states, service environment, Git binding and
existing effective configuration were preserved. Input-limit defaults are
exposed by runtime status without rewriting the existing source configuration.
Web remains enabled with `serialized-context`, concurrency 2 and a 256 MiB
budget. Target object-storage scope is distinct from the other checked services.

Real acceptance results:

| Request | Result | Measured text subset / output |
| --- | --- | --- |
| Disabled New API channel 271, ordinary Enterprise, streaming `gemini-3.8-flash-enterprise` | Functional success, 11.76 seconds | Existing Enterprise behavior retained |
| 8219, Web `gemini-4-argon`, non-streaming historical PNG/TXT/PDF | HTTP 200, STOP, 15.500 seconds; image nonce, ordered colors, TXT/PDF nonces and user/model history markers all matched | Input text 479; output 89 |
| 8219, Web `gemini-4-argon`, streaming historical PNG/TXT/PDF | HTTP 200, STOP, 17.469 seconds; all independent content checks matched, including the latest text-only turn | Input text 737; output 88 |

Both Web responses identify the local Gemma3/SentencePiece source and match
their management usage records. Complete input, cached and total tokens remain
null. No thought summary was returned in these two calls, so reasoning tokens
also remain null; no known zero was fabricated. This is local text estimation,
not authoritative Google billing. Each response reports one complete usage
extension. Admission resources returned to zero.

Profile 8219 was verified at revision 44 after automatic Cookie maintenance;
the server recorded a committed renewal at 23:48:01.507 +08:00 and a successful
check at 23:48:02.455 +08:00. No browser connection or manual Cookie/classification
change occurred. This short acceptance does not establish long-term renewal.

The latest deployment identifiers/commits of p05, p07, p08 and c03 were unchanged.
New API process start times/restart counts were unchanged. No other server was
deployed or restarted. Evidence:
`temp/ultra-cli2-web-integration-deploy-20261105/acceptance-summary.json` and
`web-acceptance.json` in the same directory. Sensitive snapshots remain encrypted
outside the repository. No new performance or GUI acceptance is claimed.
