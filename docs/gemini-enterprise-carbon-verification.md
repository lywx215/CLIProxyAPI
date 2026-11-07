# Carbon application admission verification — 2026-11-07

## Tested scope

Backend and management frontend worktrees remain on
`codex/gemini-enterprise-web-phase1`, with their pre-existing unrelated changes
retained. Worktree HEADs were `bc1d8e9d` and `283f069`; these are reference HEADs,
not commits containing this uncommitted feature. The new feature concerns only
ordinary OAuth `gemini-enterprise`. No deployment, production restart, live
application enablement, or Google configuration mutation was performed.

Local evidence files below are under ignored `temp/` directories and are not
release artifacts. The implementation contract is documented in
[Carbon application admission](gemini-enterprise-carbon-admission.md).

## Backend and frontend checks

- `go test ./... -count=1`: passed, 113 tested packages, zero failed packages.
  Final increment log: `temp/carbon-credential-operations-go-full.log`.
  The earlier single-application stage is retained in
  `temp/carbon-admission-go-full-single-app.log`.
- `go build -o temp/carbon-credential-operations-server.exe ./cmd/server`: passed.
- Related policy, credential store, SDK service, routing and management suites
  passed. The tests cover default closure, verified-subscription qualification, separate
  native candidates, custom whitelists, exclusions, aliases, prefixes, thinking
  suffixes, priority, selectors, round-robin, retries, fixed applications and quota
  enforcement. Closed ordinary business calls return `503` without another
  provider taking over; existing media requests retain Web routing.
- Deterministic lifecycle tests cover concurrent application evidence writes,
  duplicate/cancelled tests, stale revisions, access refresh, trusted refresh-token
  rotation, restart, reauthorization, binding changes, Store failures, temporary
  failures, quick close/reopen and old model failures arriving after new proof.
- Management tests cover fixed random-code validation, actual-model session
  proof, wrong/missing models, missing/foreign/conflicting sessions, truncation,
  permission errors, redirects, oversized proof responses, forged test IDs,
  explicit false, import sanitization and provider spelling normalization.
- `bun run verify`: passed, **2037 tests, 20460 assertions, 195 files**, zero
  failures; ESLint, TypeScript and Vite production build passed. Frontend log:
  `../Cli-Proxy-API-Management-Center/temp-carbon-batch-delivery-verify.log`.
- Frontend coverage includes application cards and counts, model modal, separate
  candidates and business lists, six languages, native Carbon once at the end of
  batch tests, strict unchanged collection revision after a test, explicit false
  persistence, failure handling, old-backend behavior, and actual delayed Promise
  tests for cancellation and connection-switch isolation. Contradictory admission
  summaries cannot display availability without `can_enable=true` and valid proof.
- The compiled frontend was copied to backend `static/management.html`. Both
  SHA-256 values are
  `95131df8d8b503667438ef0835ab2c51ca9961f56dfd6e48b68c97d71e6988df`.
  Manifest: `temp/carbon-credential-operations-asset.json`.

No GUI was used. Static rendering, accessibility labels and API behavior are
covered; browser layout, keyboard interaction and visual acceptance remain
unperformed. Full Go race instrumentation was not part of this run.

## Isolated real Google management test

The explicitly authorized paid credential's single global application in project
`1002653018111` was loaded into an in-memory Auth Manager. Its source file was not
rewritten. Assigned subscription eligibility was read from Google, not simulated.
No deployed service or other application was used.

The first attempt returned the complete verification text but could not save
successful evidence because the text parser did not expose its private session
metadata. It correctly left Carbon closed. The parser now has an optional private
session observer used only by this management test; executor regressions verify
that ordinary replies do not expose session metadata and that conflicting sessions
cannot create proof. The failed attempt is retained in
`temp/carbon-admission-live-first-failed-20261107.json`.

The corrected live test ran **13:58:14–13:58:46 Asia/Shanghai**:

| Check | Observed result |
| --- | --- |
| Assigned subscription | `ok`, paid (`free_trial=false`) |
| Closed candidate visible | Yes; absent from registered business models |
| Unauthenticated management test | `401` |
| Authenticated fixed-application test | `200`, `status=ok` |
| Complete random code and exact saved session model | Verified `gemini-4-carbon` |
| Upstream generations in corrected attempt | Exactly one, Carbon only |
| End-to-end management test time | 29,838 ms |
| Collection edit revision | Remained 1 after saving evidence |
| Manual switch after successful verification | `enabled=false`, `available=false`, `can_enable=true` |
| Ordinary business request after verification | `503`; no second generation |

Evidence: `temp/carbon-admission-live-20261107.json` and `.log`.
Source SHA-256 remained
`b6acf7e26a84ec48da9ce054f5686cbe07358b0411ce82fd745715cf2bfd2249`.
The ephemeral proof disappeared with the test process; no real application was
opened automatically. Manual open/close and enabled-business streaming/nonstreaming
are covered by synthetic tests. The new gate's enabled-business live streaming
path was not exercised because real application enablement was outside this run.

## Credential configuration, batch tests and trial qualification increment

The subsequent explicitly authorized increment preserves the single-application
workflow and adds credential-level configuration and Carbon-only batch testing.
Configuration uses five-application batches and at most two foreground requests;
generation tests use at most two workers. Successful tests never enable an
application automatically. The result modal exposes only individual manual
switches through the existing proof-ID and revision checks. It adds no persistent
task center or automatic retry of uncertain model submissions.

Google configuration Mock checks cover preserving other models/defaults/tools,
malformed-range zero writes, deduplication, idempotency, partial failure followed by a
new explicit run, permission failures, redirects, remote configuration conflicts,
unknown-write readback without replay, cancellation and bounded resource cleanup.
Application snapshots bind the current imported set, authorization generation,
stable authorization fingerprint, resources, necessary revisions and connection
configuration. Trusted refresh works across batches; explicit reauthorization with
the same refresh text still invalidates a snapshot. The snapshot lifetime is 15
minutes, with caps of 128 snapshots and 8192 frozen application entries.

Focused and full related API/helper checks passed:
`temp/carbon-google-acceptance-packages.log` and
`temp/carbon-credential-operations-management-focused.log`. Twelve new frontend
operation tests cover the two credential-card buttons, remote-mutation notice,
missing-application handling, fixed targets, batch limits, concurrency, cancellation,
connection changes, stale responses, old-backend behavior and six-language markup.
`404`, `409` and rate limiting stop scheduling new batch tests; already started
tests follow the existing cancellation/submission rules.

A final compatibility check found that an older valid account graph may omit the
default region or collection. Configuration snapshots now compare the same
canonical complete resources used by the frontend and projected credential;
foreign or incomplete client targets still fail. The legacy-default and explicit
binding-race checks, four related full package suites and service rebuild passed
(`temp/carbon-google-legacy-defaults-packages.log`). This last management-only fix
does not execute in the ordinary text benchmark.

Only Carbon's former trial-label hard rejection was removed. It still requires a
verified assigned effective subscription, complete strict model proof and manual
enablement, with the existing exclusions, custom whitelist, credential state,
quota and final-payload rules. Argon, Auto and other models retain their earlier
subscription behavior. No previous side-window results were imported as proof.

One minimal live test used only the authorized **AU** application, from
**14:29:09 to 14:29:36 Asia/Shanghai**:

| Check | Observed result |
| --- | --- |
| Assigned subscription | `ok`, `free_trial=true` |
| Existing Google configuration | `unchanged`, HTTP 200 |
| Engine/Assistant configuration writes | Zero; the test transport prohibited them |
| Google resource reads | Three: Assistant, Engine and saved session |
| Management model test | One Carbon generation, complete random code, saved model confirmed |
| Management test elapsed time | 23,177 ms |
| Local switch and ordinary business after test | Remained closed; business request returned 503 |
| Source credential file | Unchanged; same SHA-256 as above |

The runtime proof was memory-only and disappeared with the isolated process.
Evidence: `temp/carbon-admission-live-au-20261107.json` and `.log`. Google writes
and multi-application failures were tested with Mocks; no real bulk configuration,
bulk model testing, or real application activation was performed.

## Implicit Assistant model range correction

The first deployed credential configuration flow rejected an empty or omitted
Assistant model allowlist before configuring the Engine. This was a local
protection, not a Google permission response. The corrected flow preserves the
implicit Assistant range byte for byte in the Mock and configures only the
Engine Carbon override. It reports `verification_required` with
`assistant_defaults_preserved` instead of claiming full configuration or model
access. The user must still run the existing fixed-model generation test and
explicitly enable the application after successful verification.

Coverage includes empty, omitted and null fields, an omitted generation config,
already-enabled Engines, shared resources and retries without rewriting, malformed
fields with zero writes, concurrent Assistant changes, cancellation, preservation
of local admission settings and proof, and all six frontend languages. This
correction does not infer an allowlist from the model catalog or change the
ordinary Enterprise execution path. These checks use synthetic Google responses;
they do not establish that Google permits Carbon with an implicit Assistant range.

## Carbon management state and model list corrections

Saving Carbon test evidence increments the account's persistence revision while
preserving each application's binding revision. The batch dialog originally used
the account revision to invalidate all rows. That mislabeled ordinary successful
test saves as external changes and marked even untouched switches as failed.
The corrected management contract exposes runtime-only authorization revision
and account registration epoch values, including an initial authorization value
of zero. Test proof saves and trusted OAuth refreshes keep that fence stable;
authorization replacement, explicit reauthorization and account recreation change
it. Application binding, policy and local settings retain their own checks.

The model dialog also unconditionally hid the native Carbon ID and always labeled
the access card a non-business candidate. The corrected UI lists native and alias
Carbon business models only when the current backend admission is available,
updates the displayed counts, and distinguishes an admitted model from a closed
test candidate in all six languages. The credential model API already followed
effective admission correctly; extra regression coverage verifies its native,
prefixed and alias models without changing the runtime execution policy.

Verification remains API and command based. No browser GUI check or additional
Carbon model generation was performed for these corrections. The previous
ordinary-text performance shortfalls below remain open; management-only edits
do not establish a new performance result.

Final command verification for these corrections passed: `go test ./...`
(113 tested packages), the service build, and frontend `bun run verify`
(2051 tests across 196 files, 20620 assertions, zero failures, plus ESLint,
TypeScript and single-file build). Logs are
`temp/carbon-management-ui-fixes-go-full.log` and the companion frontend's
`temp-carbon-management-ui-fixes-final-verify.log`. The frontend bundle copied to
the backend has SHA-256
`7cac8d9737af331148a40fbb6a89a6b5c87ab589c014d96e7a72b0c1ca0c2767`.
A read-only API check against the target server confirmed that an already-admitted
application's response contained Carbon in its business models; that check made
no Google or generation requests.

## Ordinary text performance

The saved pre-admission benchmark binary and each changed binary use the same
native HTTP handler, immediate Mock executor, CPU affinity, four Go processors,
payloads and iteration count. Each measurement series contains five interleaved
before/after pairs with alternating order; each of four scenarios has 10,000
requests per binary per pair. Equal warm-ups are excluded and retained separately.
No model or Google authentication requests are made by this benchmark.

This measures the incremental manual-admission change against the existing dirty
checkout, which already contains earlier Web work. It cannot clear the historical
Web regression records of **5.34%** and **7.75%**.

The first series did **not** meet the all-scenarios p95 goal:

| Web | Text | Before median p95 (µs) | Initial median p95 (µs) | Change | Largest paired change |
| --- | --- | ---: | ---: | ---: | ---: |
| Off | 1 KiB | 307.2 | 327.3 | +6.54% | +21.97% |
| Off | 32 KiB | 1398.2 | 1389.1 | -0.65% | +0.95% |
| On | 1 KiB | 330.8 | 314.6 | -4.90% | +8.52% |
| On | 32 KiB | 1394.6 | 1408.3 | +0.98% | +4.81% |

Raw samples, allocations, throughput and timing data:
`temp/carbon-admission-performance/results.json` and `pair-*.log`.
The initial changed binary is retained as
`temp/carbon-admission-pre-optimization-gemini.test.exe`.

Two concrete avoidable costs were removed after this result: an unused boolean
that increased a per-request allocation bucket from 64 to 80 bytes, and a private
test-context lookup for models already registered by the ordinary path. No final
payload gate, alias rule or submission policy was relaxed. The first failed series
remains part of the evidence rather than being discarded as noise.

The second series, after these two optimizations, also **did not** meet the
all-scenarios goal:

| Web | Text | Before median p95 (µs) | Optimized median p95 (µs) | Change | Largest paired change |
| --- | --- | ---: | ---: | ---: | ---: |
| Off | 1 KiB | 322.2 | 315.8 | -1.99% | +18.22% |
| Off | 32 KiB | 1434.0 | 1377.1 | -3.97% | +0.97% |
| On | 1 KiB | 322.9 | 339.9 | +5.26% | +37.25% |
| On | 32 KiB | 1409.6 | 1440.3 | +2.18% | +15.40% |

Raw optimized samples: `temp/carbon-admission-performance-optimized/results.json`
and its `pair-*.log` files. Small-request allocation medians returned to baseline
(61,849–61,851 bytes/op, 494 allocations/op). The large paired timing variation
prevents attributing every difference to code, but it does not establish a pass.
No further repetitions were run merely to obtain passing numbers. The existing
historical Web performance failures remain open.

After the newly authorized credential-operations and trial-qualification increment,
a third five-pair series was required because the production policy code changed.
It used the same saved baseline, CPU affinity, four processors, Mock, alternating
order and 10,000 iterations per scenario. It also **did not pass** all scenarios:

| Web | Text | Before median p95 (µs) | Final median p95 (µs) | Change | Largest paired change |
| --- | --- | ---: | ---: | ---: | ---: |
| Off | 1 KiB | 318.7 | 352.2 | +10.51% | +32.98% |
| Off | 32 KiB | 1492.0 | 1434.4 | -3.86% | +1.66% |
| On | 1 KiB | 362.2 | 322.6 | -10.93% | +2.37% |
| On | 32 KiB | 1434.8 | 1474.5 | +2.77% | +14.39% |

Final samples, including ns/op, throughput, allocations and each paired delta:
`temp/carbon-admission-performance-credential-operations/results.json` and
`pair-*.log`. Allocation counts decreased from 494 to 492 per request; small-text
allocation medians decreased from 61,850 to 61,816–61,819 bytes/op. This does not
resolve the latency acceptance failure. Timing variation remains substantial,
and no claim of a production p95 guarantee or a <=5% overall regression is made.

## Earlier capacity results

Token capacity testing is finished, and its isolated host is stopped. The earlier
real calls used the previous admission implementation; they are empirical capacity
evidence, not automatic permission to open applications under this feature.

- Largest fully completed tested input: **1,026,699 local estimated tokens**.
  The smallest explicit `PROMPT_TOO_LARGE` probe was 1,042,418; intervening
  `UNAVAILABLE` results are inconclusive.
- Largest complete tested visible output: **58,374 local estimated tokens**.
  A larger request produced about 64,661 visible estimated tokens and a length
  notice, but did not complete the required output fixture.
- All **14** short tests across 2/4/8 concurrent requests succeeded. The 4/8 groups
  overlapped one longer probe. These small samples do not establish a sustained
  concurrency ceiling or production RPS guarantee.
- Counts are local Gemma 3 tokenizer estimates, not Google's native Carbon billing
  tokens or a published exact hard limit. `maxOutputTokens` was not enforced by
  the tested Enterprise endpoint.

Full capacity report and 47 recorded probes:
`temp/enterprise-carbon-capacity-20261107/report.md` and `results.json`.

## Security and remaining acceptance

Known source access/refresh/ID tokens and application secrets were compared against
task source files, reports and built frontend artifacts without printing the
values. The scan found zero matches, and the source credential checksum remained
unchanged. Refreshed live-token output was also checked in the live wrapper before
its process ended. Fingerprints, random test prompts and raw responses are not
returned in ordinary management summaries.

This is a local, uncommitted implementation and API-only validation. It does not
constitute a production deployment, a grant for the other applications, or browser
visual acceptance.
