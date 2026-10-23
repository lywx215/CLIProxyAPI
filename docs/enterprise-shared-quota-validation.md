# Gemini Enterprise shared quota validation

This change is local development based on backend `6231d4dcaf4d757fe47f89a0d16f2e10b4fc8c9c`
and frontend `13ce4bfec84e7d4b7d4dcb6b53bf21e4ed8e3424`. Both source trees are isolated
from the deployment checkouts. No push, deployment, IAM mutation, credential change,
external Claude/Antigravity review, or real Google/model request was authorized or
performed for acceptance. Fixtures do not establish the format of a real natural
Google shared daily quota exhaustion event.

Latest merged-branch full run (2026-10-23): 107 packages passed, 33 had no test files,
and three test executables were blocked by Windows Application Control. No test
assertion failed. The full suite remains non-green; see the post-merge run below.

## Implementation and regression coverage

| Area | Local evidence |
| --- | --- |
| Independent state | One shared authority in `sdk/cliproxy/auth`, credential/model states retained, no sibling `MarkResult` or fabricated failure counts |
| Membership | OAuth userinfo + canonical project + assigned UserLicense + matching LicenseConfig; unknown identity falls back to the current credential |
| Scope | Same-pool/new-model/alias projection, different project/location/edition isolation, user scope and technical minute scope |
| Dispatch | Built-in selectors, fast/ordinary manager picks, final selected executor gate, registry projection without removing catalog entries |
| Concurrency | 50 refusals merge; 50 due requests admit one verification; cancellation, old success/new refusal, reset/old refusal and rebind/old result fencing |
| Protocols | OpenAI Chat Completions and Gemini Generate Content, streaming/non-streaming, HTTP 429, 200 embedded errors, JSON array, NDJSON thinking, SSE tail, byte-at-a-time reads, no replay |
| Persistence | Separate atomic file snapshot, restart binding restore, corrupt/stale file rejection, store transition, PostgreSQL SQL-driver transaction/version regression |
| Configuration | Credential opt-out, global hot opt-out, opt-out success cannot clear the pool, Home authority boundary |
| Management | Explicit v8 membership/version reset; affected indexes; repeated stale version returns 409; no v0 route changes |
| Frontend | Optional fields and phase normalization, zero-second pending/probing, six locales, backend support gating, old backend fields, response/session guards |

PostgreSQL validation uses a deterministic SQL driver, not a live PostgreSQL server.
A live database restart/migration exercise remains unperformed. The implementation
is single-instance coordination; PostgreSQL is not a cross-process lease/broadcast.

## Commands and environment limits

- Focused Go package and public-route synthetic suites passed during implementation.
- The first full `go test -timeout 240s ./...` run completed with 101 passing packages,
  32 packages without tests, and 8 test executables blocked by Windows Application
  Control. No assertion failure was reported by that run. The blocked packages were
  `cmd/fetch_codex_models`, `internal/clienterror`, `internal/cmd`,
  `internal/githubauth`, `internal/runtime/executor`,
  `internal/translator/antigravity/claude`, `internal/translator/codex/gemini`, and
  `sdk/cliproxy/executor`. This is not a full green test result.
- `go test -race` was attempted; the installed Go environment has `CGO_ENABLED=0`
  and refused the race run. No compiler/toolchain or Windows policy was changed.
- `go build ... ./cmd/server` passed. The ordinary server executable started on
  loopback for synthetic UI checks. This does not establish that blocked test
  executables can run, and does not replace their missing acceptance.
- Frontend `bun run verify` passed its initial complete run: 1,797 tests, ESLint,
  TypeScript and the production single-file Vite build. Final audit results and the
  pinned artifact are recorded below after final verification.

The raw local logs remain in the task's `enterprise-shared-quota` evidence folder,
including the full test run, race refusal, build result, browser scripts and screenshots.
Historical Windows policy evidence in the original development/diagnosis context is
untouched. The built-in browser/Node REPL entry points failed to initialize their
kernel assets; browser QA used the already installed bundled Playwright and Chromium.
No blocked executable was renamed, repackaged or run through an alternative path.

## Browser and local service

The loopback server used synthetic auth files and a test-generated shared state.
It bound only `127.0.0.1:18485`, disabled panel auto-update and used a refused
loopback proxy for upstream traffic. The model catalog/optional quota reads could
not reach Google. The fixture key is synthetic, unrelated to user credentials.

The initial browser pass checked both linked cards, all-text impact, linked count,
native keyboard Space disclosure, impact confirmation, a real v8 shared reset,
refreshing both cards to pending verification, and no JavaScript page errors. Narrow
layout, dark theme, probing/old-backend fixtures and logout fencing are recorded in
browser evidence. Browser-local mocked response scenarios are distinguished from the
real local management operation. No browser test invoked a real model.

## Final artifact and acceptance

The parallel audit covered error classification, state/persistence/concurrency and
frontend lifecycle handling. The resulting changes include conservative handling of
contradictory quota evidence, version fencing across Home/configuration transitions,
safe snapshot copies, reset conflicts, and stale connection/logout responses.

The final frozen-source `go test -timeout 240s ./...` run recorded 89 passing
packages, 32 packages without tests, 19 Windows Application Control blocks and one
assertion failure. The failed assertion was the existing
`TestServiceReplacementWaitsForPublisherExitAndPinsACKedLifetimeDependencies` in
`sdk/cliproxy`: `publisher did not send barrier revision 22`. The exact test then
passed in 5.142 seconds, and a fresh full `sdk/cliproxy` package run passed in
10.597 seconds without source changes. The test's short fake-server heartbeat window
is a possible explanation under full-suite load; the cause has not been confirmed.
Its Home service/client/test files were not modified by this change. The original
failure and all rerun logs are retained; the full test suite is not reported green.

The final run passed `internal/auth/geminienterprise`, `sdk/cliproxy/auth`,
`internal/store` and `test`, including the synthetic public-route integration
cases. Some of these packages were blocked in earlier runs; a later pass does not
erase earlier policy evidence or validate other blocked packages. The blocked
package list is in `go-test-all-final-summary.json`. Final ordinary server build,
Go formatting and `git diff --check` passed. Race testing and a live PostgreSQL
exercise remain unperformed for the reasons recorded above.

Frontend `bun run verify` passed 1,816 tests across 178 files (18,200 assertions),
ESLint, TypeScript and the production build. The isolated frontend commit is
`110c204bd2055ccb165595e06e18d27ca82cd16d`. Rebuilding that commit with
`VERSION=dev-shared-quota` and `BUILD_TIME=2026-10-23T14:45:00Z` produced exactly
the tracked artifact. The commit's `dist/index.html`, rebuilt file, isolated backend
`static/management.html` and HTTP response from the final server are all 4,024,577
bytes with SHA-256:

```text
99c0ab3703c5282306e3ab7e55d0159b16b2c273e9d94425dd4e5ee6474310f4
```

The final server executable SHA-256 is
`4d30357e8494c403addef44ac5535b8a49c78cb4ba7836040dada14b54dcf6dc`.
`artifact-consistency.json` retains the byte counts and hashes.

The final browser passes completed nine actual-loopback management checks and seven
dark/mobile/compatibility checks without page errors. Probing and old-backend
responses were browser-local synthetic fixtures. Connection changes, late responses
and logout/ABA fencing also passed the deferred-adapter frontend tests. Screenshots
include `cards-expanded-light.png`, `shared-retry-confirm.png`,
`cards-pending-dark-mobile.png`, `cards-pending-dark-desktop.png`,
`cards-probing-dark.png` and `legacy-backend-dark.png` in the evidence directory.

The synthetic server was stopped after verification, its executable path was checked
before stopping it, and no listener remained on port 18485 (`cleanup.json`). Both
isolated source trees remain on `codex/enterprise-shared-quota`; backend changes are
recorded in the original shared-quota source commit below, and frontend changes are
in the local commit above. Original checkouts and their unrelated changes were
preserved during isolated development. The later authorized local merge is recorded
at the end of this document. Nothing was pushed or deployed.

## Local commit handoff

| Repository | Branch | Local commit | Scope |
| --- | --- | --- | --- |
| Backend | `codex/enterprise-shared-quota` | `28e8b7f8c447ed134f235e17c3704ae718cfca15` | Shared quota classification, verified identity, coordinator/gates, persistence, v8 reset, synthetic regressions, documentation and matching management asset |
| Frontend | `codex/enterprise-shared-quota` | `110c204bd2055ccb165595e06e18d27ca82cd16d` | Shared recovery display/actions, compatibility and session fencing, six locales, regression tests and production single-file artifact |

The exact two commit IDs, worktree paths and remaining checks are also recorded in
`local-commit-handoff.json` in the evidence directory after the backend commit is
created. This preserves the original source commits separately from later merges.
The local-commit request changes only this handoff record after the previously tested
source was frozen; no application code or built artifact changed during that request.

## Remaining verification

| Item | Current evidence | Remaining action |
| --- | --- | --- |
| Full Go suite | Fresh merged-branch run: 107 packages passed, 33 had no tests, three policy blocks, zero assertion failures | Execute `internal/translator/gemini/interactions`, `internal/watcher` and `internal/watcher/synthesizer` where policy permits, then complete a green full suite; preserve prior policy evidence and do not bypass policy |
| Race detection | Attempt refused because `CGO_ENABLED=0` | Run the affected concurrency suites with a compatible Go/C toolchain and race support |
| Live PostgreSQL | Deterministic SQL-driver transaction/version tests passed | Exercise schema creation, restart/restore, stale version rejection and state-store transition against a permitted test database |
| Real Google behavior | All generation/error scenarios are synthetic; no real model call was made | With separate authorization and a suitable natural event, validate structured shared-quota signals, identity permission behavior, recovery hints and one-request recovery probing |

These are validation gaps, not a request to push, deploy, change IAM, rotate
credentials, run external review models or consume a real Google model quota.

The evidence directory is:

```text
C:/Users/lywx2/.codex/visualizations/2026/10/23/01a16921-7505-7fd2-9719-8788c19f9db5/enterprise-shared-quota
```

The backend worktree is
`C:/Users/lywx2/.codex/worktrees/enterprise-shared-quota/CLIProxyAPI`, and the frontend
worktree is `G:/code/gemini30/enterprise-shared-quota-frontend`. Real natural Google
quota exhaustion, live PostgreSQL restart/migration, race detection and blocked test
executables still need their own permitted acceptance environment before release.

## Local merge into codex/gemini-enterprise-20261023

On 2026-10-23, the user authorized merging both local changesets into
`codex/gemini-enterprise-20261023`, without push or deployment.

- Backend: merge source `28e8b7f8c447ed134f235e17c3704ae718cfca15` into target
  `492756362e5b9ceb953f32fc9a69f5fda4cbbdf4`, retaining the target's field validation
  and trusted public error diagnostics. The merge commit is the commit adding this
  section; its exact ID is in `local-merge-handoff.json` in the evidence directory.
- Frontend: fast-forward target `13ce4bfec84e7d4b7d4dcb6b53bf21e4ed8e3424` to
  `110c204bd2055ccb165595e06e18d27ca82cd16d`. The pre-existing local `.gitignore`
  modification and `.codex` contents were preserved and checked by file hashes.

The backend's one textual conflict was in HTTP error handling. The merged executor
retains the 64 KiB response-body limit, discards oversize bodies before structured
classification, preserves cancellation/read errors and closes the body. Shared quota
errors remain typed for the coordinator and unwrap only a locally constructed safe
diagnostic for public responses. A synthetic public-route regression now requires the
shared HTTP rejection to retain `gemini_enterprise_rate_limit_exceeded`, in addition
to updating the shared pool. Request payload construction and final validation were
retained.

The merged source passed all nine selected Go package suites:

```text
internal/auth/geminienterprise
internal/translator/geminienterprise
internal/runtime/executor
internal/runtime/executor/helps
sdk/cliproxy/auth
sdk/api/handlers
test
internal/store
internal/api/handlers/management
```

These suites include the existing bounded-read/public-diagnostic executor tests and
the shared quota public-route regression. `merge-go-tests.log` records this run.
Formatting and staged whitespace checks passed, and the final ordinary server build
passed with SHA-256
`0cf6021fc87ac1efa818afa1dc1a667d0e6525042a0fde2a9be80903d7a7c640`.
The merged binary was not started for another browser session.

Frontend application source and artifact are unchanged from the previously verified
`110c204` commit, so frontend verification/browser tests were not repeated for this
fast-forward. The target frontend artifact and backend management asset have identical
Git blob `532e86c2156777f033d08639e4f60a12c8da128d` and the previously recorded
SHA-256 `99c0ab3703c5282306e3ab7e55d0159b16b2c273e9d94425dd4e5ee6474310f4`.

No full Go suite was rerun as part of the merge itself. The later full run is
recorded below. The merge-only focused success did not resolve the earlier Home
timing, race, live PostgreSQL or real Google validation gaps. No policy setting,
credential, IAM resource or deployment was changed.

## Post-merge full Go suite (2026-10-23)

A fresh complete backend run was performed on branch
`codex/gemini-enterprise-20261023` at application commit
`93d7faef29cacf9d7eae7e5eaa7ab10fa7782e55`. The worktree was clean before and after
the test command. Only this validation document was updated afterward.

```text
go test -json -timeout 240s -count=1 ./...
```

The run used Go 1.26.0 on Windows/amd64 with `CGO_ENABLED=0`, from 23:33:45 to
23:35:18 Asia/Shanghai (93.06 seconds). `-count=1` forced fresh test execution.
Results across 143 packages:

| Result | Packages |
| --- | --- |
| Passed | 107 |
| No test files | 33 |
| Windows Application Control execution block | 3 |
| Test assertion failures | 0 |

The command exited with status 1 because Windows prevented these test executables
from starting:

```text
internal/translator/gemini/interactions
internal/watcher
internal/watcher/synthesizer
```

Each failure reports `An Application Control policy has blocked this file.` There
was no policy change, alternate executable packaging or execution workaround, and no
automatic retry of the blocked packages. Their tests remain unverified by this run.

The previously failing
`TestServiceReplacementWaitsForPublisherExitAndPinsACKedLifetimeDependencies` passed
in this full-suite run (0.13 seconds), as did its entire `sdk/cliproxy` package
(20.273 seconds). The earlier failure was not reproduced, so it is no longer listed
as a currently failing check. Its original cause is still unconfirmed; the original
failure and rerun logs remain preserved.

Nine top-level tests were skipped, covering opt-in live/native-fixture checks,
TLS capture and the optional Enterprise SDK harness. Their exact names are retained
in the run summary. These skips are not counted as passed tests, and no live Google
or model exercise was enabled. Race detection, live PostgreSQL and real Google
behavior remain separate outstanding validation items in the table above.

Full JSON events, stderr, execution metadata, exit status and parsed summary are
stored in this subdirectory of the evidence directory:

```text
merged-full-go-20261023T153345Z/
  run.json
  events.jsonl
  stderr.log
  completion.json
  summary.json
```

Earlier full-suite and merge-only evidence is historical and has not been replaced.
This latest run narrows the current full-suite execution gap from the earlier 19
policy blocks to the three packages above. `current-validation-status.json` in the
evidence directory records the current remaining items and the documentation commit
separately from the tested application commit. Nothing was pushed or deployed.
