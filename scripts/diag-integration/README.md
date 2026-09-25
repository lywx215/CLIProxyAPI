# DIAG-07 isolated integration driver

Status: candidate for code review. Overall acceptance is blocked by Windows
Application Control; this delivery does **not** claim the required six-process
run or complete acceptance. See the versioned delivery and matrix under
`coordination/diagnostics/20260924/DIAG-07-*`.

The Go command composes the existing Gin request-ID/diagnostic middleware,
OpenAI/Gemini handlers, auth Manager, Gemini executor, translator and HTTP stack.
It loads no configuration/auth store and starts no model updater. A final
transport dial allowlist restricts it to the explicitly supplied 127.0.0.1
upstreams. Only this new test entry has control endpoints.

The Python driver calls the frozen gcli harness and the Node wrapper calls the
frozen Aito fixture. The wrappers contain no copied business implementation.
gcli runs from a new temporary cwd with an empty, separate `--root`; a Python
audit hook rejects non-127.0.0.1 connects. The Aito fixture's approved real
Express/RequestHandler/Registry/generation/converter/WS flow uses a fake browser.
Only the caller, provider and browser are simulated. Each subprocess receives
an environment allowlist, synthetic credentials and its own temp root. Node 24
is required. Windows subprocess creation uses `CREATE_NO_WINDOW`.

## Build and run

Use the repository's Go toolchain and existing approved dependencies. No pip or
npm installation is required. Run from this repository root. Set the five
dependency/runtime paths to existing local installations; do not supply real
service configuration or credentials.

```powershell
$Gcli = "$env:USERPROFILE/.codex/worktrees/14f9/gcli2api"
$Aito = "$env:USERPROFILE/.codex/worktrees/ca98/Aitoapi-custom"
$Python = $env:DIAG07_PYTHON # Set to the existing approved gcli venv Python.
$Node = "$env:USERPROFILE/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin/node.exe"
$QaDeps = "$env:TEMP/gcli-diag02-validation-deps"
$env:PYTHONDONTWRITEBYTECODE = '1'
$env:GOPROXY = 'off'
go build -o "$env:TEMP/diag07-driver.exe" ./cmd/diag-integration
go build -o "$env:TEMP/diag07-analyze.exe" ./cmd/diag-analyze

python scripts/diag-integration/run.py `
  --gcli $Gcli --aito $Aito --python $Python --node $Node `
  --qa-deps $QaDeps --driver "$env:TEMP/diag07-driver.exe" `
  --analyzer "$env:TEMP/diag07-analyze.exe" `
  --binary-provenance <hash-bound-build-provenance.json> `
  --output "$env:TEMP/diag07-new-six-process-run"
$RunExit = $LASTEXITCODE
```

**Do not rebuild, rename or retry the currently refused driver to evade policy.**
The commands above describe normal reproduction in an authorized environment.
The recorded blocked file must remain in place until its owner resolves policy.
After explicit clearance for the recorded file, skip the build lines and use
that unchanged binary. For the independent executable subset, add
`--downstream-only`; the report explicitly records `sixInstances=false`.
Every output directory must be new; no earlier run is overwritten or deleted.

Every cross-repository service start verifies exact HEAD, clean status, the
manifest digest, all member hashes and the 73-file set. CPA must descend from
the frozen base and have changes only in DIAG-07 support/report paths. No remote
branch is fetched. The published contract is never changed. Set
`PYTHONDONTWRITEBYTECODE=1` for all validation commands too.
`trackedProductionClean` checks tracked differences only; it does not certify
untracked files. `revisions.json` separately records invocation HEAD, current
support-file hashes, actual binary hashes, embedded Go build metadata and
hash-matched build attestations. Do not label an older executable with invocation
HEAD. The required provenance JSON has `driver` and `analyzer` objects, each with
`sha256`, `baseRevision` and `sourceQualification`, plus any source hashes/build
records. Generate these from the actual build, never from a guessed current HEAD.
The retained R2 `binary-provenance.json` applies only to the exact recorded hashes.
Only `main.go` in the historical blocked record corresponds to the driver binary;
its Python/CJS hashes are contemporaneous support snapshots and later changed.

Exit codes: `0` means executed assertions **and all acceptance gaps** cleared
(currently unreachable in every environment because required gaps are hardcoded); `1` is an assertion or
collection/close failure; `2` is preflight/setup/uncaught execution failure;
`3` means scoped checks passed but mandatory acceptance gaps remain. The live
driver now invokes `check_evidence.py` before publishing a successful scoped
result. The HTTP result list alone is not semantic acceptance.

```powershell
python scripts/diag-integration/check_evidence.py <run-directory>
python scripts/diag-integration/check_offline.py `
  --analyzer "$env:TEMP/diag07-analyze.exe" `
  --live <run-directory> --pair <full-run-directory> `
  --output <new-offline-report-directory>
```

For the historical Aito-only smoke, explicitly pass `--required-service aitoapi`
and `--peer-plan <historical-peer-plan.json>` to the offline checker. R2 retains
that independent configuration map in `replay-03/`. Historical run-06 replay also
needs `--live-known-loss aito4`: its original export manifest predates that flag.
Use `check_evidence.py --downstream-only` for historical downstream-only semantic
replay; an absent mode declaration defaults to requiring full integration.

The evidence checker joins response request/trace IDs to exact file/line
records. It checks classification, EOF distinctions, candidate/reasoning/actual
converted usage, header handling, attempt identities, cleanup, DEBUG absence,
workers and restarts. It does not infer a call from a timestamp. The offline
checks distinguish actual exports from generated conflict/version/size inputs.
Their exit 0 is scoped validation, never overall DIAG-07 approval.

`analysis.json` imports only sources without declared loss;
`analysis-known-loss.json` imports all sources and preserves each known-loss
declaration. `analysis-scopes.json` records exact input hashes and exclusions.
The first report proves relationships only within its stated subset. The frozen
analyzer correctly blocks every verified edge in the all-source report when any
source has known loss. Neither report certifies global collection completeness.

Each CPA request records its independently configured peer map. The checker
requires an actual call, unique owner/receiver, matching trace and parent span,
expected service/deployment/instance/alias, matching response peer IDs when
present, and the analyzer's unique verified remote edge. Full mode requires both
services and all four configured peer targets. Offline expectations come from
controlled requests and actual calls, not a fixed edge total. HTTP/provider
errors do not by themselves invalidate graph identity. Cancellation permits only
an observed absent receiver (`missing_peer`) or absent receiver terminal
(`terminal_evidence_incomplete`); conflicts, distrust and known loss never qualify
for this exception. Complete cancellation evidence may still verify a graph
edge without proving business success or full diagnostic coverage.

## Configuration and controls

Instances use `DIAG_ENVIRONMENT=test`, `DIAG_DEPLOYMENT_ID=diag07-local` and
distinct configured instance IDs. Restart/worker cases intentionally reuse only
the instance ID, retaining fresh boot IDs. `DIAG_PEERS` uses explicit origin,
path, service and deployment, for example:

```json
[{"alias":"gcli1","origin":"http://127.0.0.1:19091","pathPrefix":"/","service":"gcli2api","deploymentId":"diag07-local"}]
```

These ports are examples; the runner allocates ports automatically. gcli's
required port argument uses a briefly reserved ephemeral port; a bind race
fails readiness rather than killing another listener. CPA/Aito bind port zero.
Peers authorize outbound propagation; they do not authenticate inbound callers.
All three producers retain `callerAlias=null`, scope `unknown`. CPA/gcli peer
refresh behavior differs from Aito's production startup-only environment read;
fixture control APIs are not production hot reload support.

CPA uses `--throttle --rate 100 --first-delay 10` for a token-rate-dominated
case, versus 1000/100 for a first-delay-dominated case. Native Gemini handlers
carry DIAG-05 throttle diagnostics; OpenAI handler responses alone cannot prove
that diagnostic family. The first configuration has been built but its live
execution is blocked. Actual elapsed credit/cancellation are separately covered
by existing component tests and remain distinct from pending live coverage.
The unchanged Go `-throttle` help still describes the old fixed values; the
actual `-rate` and `-first-delay` flags control them. Its source stays byte-identical
to the refused binary. CPA nested retry counts/statuses, Aito empty/thought 502
projection and token-rate case 89/provider_output remain unexecuted expectations.
On the first authorized live run, investigate any mismatch before changing an
expectation; passing offline tests does not validate these predictions.

`internal/util/util.go:60-76` calls `diagnostics.NotifyDebugDisabled()` before
lowering logrus level. The epoch check in `internal/diagnostics/semantic.go`
retains an off/on interruption even between observations; this is source evidence,
not a new live CPA DEBUG result. CPA call terminals report `debugCapture:"none"`
(`internal/diagnostics/transport.go:180`), while gcli can report
`enabled_throughout`. Do not directly compare those call-level fields across
services or infer full coverage; CPA access capture remains unknown.

Aito control travels over the owned child's stdin: DEBUG change, browser
reconnect, dispatch snapshot and explicit `fixture.close()`. Its close reply is
recorded, followed by actual exit status. HTTP shutdown is used for gcli/CPA.
The final Aito worker is killed after observing the browser dispatch count;
this does not prove the response was still active at the instant of the kill.
Its missing unflushed records are declared with analyzer `-known-loss aito4`.
A server terminal does not prove HTTP EOF. Late independent calls are retained.

## Retained limits and rollback

Raw stdout stays in the new local scratch tree. Public exports contain only
diagnostic records plus explicitly synthetic request/response fixtures. The raw
mixed stdout check is separate: CPA's fixture readiness JSON is quarantined,
while ordinary legacy lines are omitted. gcli writes diagnostic JSONL to a
separate sidecar; its raw stdout is not that export. Neither fact is evidence of
production diagnostic loss.

gcli has a 16 MiB per-boot hard stop, no rotation and no directory quota. A file
near the threshold is only a clue. Exact shutdown_asyncgens timing, production
collection, browser-internal HTTP/images/VNC, POSIX fork, CGO race, approved older
CPA live rolling, full proxy/backpressure equivalence and full six-instance
chains retain explicit matrix gaps. No production deployment is authorized.

Stop only processes created by this runner. Normal shutdown and forced teardown
are separately reported; an unexpected forced close fails the run. Scratch
directories are deliberately retained. Rollback is to stop the owned fixtures
and omit/revert this standalone support commit; there are no production runtime
edits, config migrations, remote operations or changes to the other repositories.
