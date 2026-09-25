# CLIProxyAPI diagnostics v1

This module consumes the frozen `contracts/diagnostics/v1` artifact from commit
`bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`. Its wire schema is
`ai-proxy-diagnostics/1`, artifact `1.0.0-rc.1`, manifest byte digest
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`.
The contract directory is unchanged. This implementation is subject to DIAG-05
coordinator inspection and exact-commit review; it is not a release approval.

## Configuration and identity

The process reads `DIAG_ENVIRONMENT`, `DIAG_DEPLOYMENT_ID`, `DIAG_NODE_LABEL`,
`DIAG_INSTANCE_ID`, and `DIAG_PEERS`. Resource identity is initialized once by
the logging adapter, independently of requests. Invalid labels fall back to
`unassigned`, null, or a random UUID and report only `config_invalid`.
`service` is always `cliproxyapi`; `bootId` is a fresh worker UUID. The build
commit comes from the existing binary build metadata when it is valid hex.

No platform variable in this repository is confirmed to identify a replica.
The runtime therefore uses a valid configured instance ID or a random UUID,
never a hostname, IP, process ID, service ID, or identity file. `NewResource`
also supports a confirmed replica UID for embedding hosts; automatic environment
inference is intentionally absent. Do not configure one instance ID for distinct
replicas. Restarts/workers remain distinct by boot ID.

`DIAG_PEERS` is the exact JSON array defined in the contract. The current process
environment is re-read at the existing server configuration reload boundary.
An unchanged raw value does not increment the revision or emit a config event.
A changed value publishes a complete immutable snapshot; invalid JSON, duplicate keys,
overlaps, unknown fields, and invalid entries disable the entire peer set.
This does not reload a shell environment from a file or alter business routing.
Changing the parent shell/deployment environment normally requires a process
restart; only an embedding process that updates its own environment can expose
a changed value at this reload boundary. Resource labels stay fixed until restart. IPv6 comparison uses parsed
`netip` addresses; mapped IPv6 remains distinct from an IPv4 authority.

## Runtime boundaries

Gin diagnostics runs after the existing local request ID middleware and outside
recovery. It keeps the existing eight-hex request ID on model routes. Other
routes receive a diagnostic-only ID using the same local convention; their
legacy access-log/management identity is unchanged. Caller IDs never replace
these IDs. The SDK cancellation adapter carries only the diagnostic context
across its existing parent choice; cancellation policy is unchanged.

The default inbound adapter accepts only `X-Request-Id` as an unverified alias.
`DIAG_PEERS` does not authenticate callers. The parser supports the frozen
authenticated-adapter behavior, but this patch installs no inbound peer trust
configuration. Auth principals in this application may be raw API keys; they
are not exported as aliases or hashed. Caller alias/scope remain null/unknown.

Finalized clients are copied, and their completed transport is decorated after
proxy, TLS/fingerprint, compression, and pool selection. Cached/context transports
are never replaced in their owners. Antigravity explicitly uses the undecorated
base builder until its HTTP/1.1 configuration is complete. Devin's concrete
transport assertion and compression clone happen before decoration. Existing
usage tracking stays outside diagnostics and remains the sole caller of
`MarkUpstreamAttempt` at that boundary. It supplies the `model` call-kind label;
unclassified synchronous helper calls use `other`. The conductor labels each actual Gemini/Antigravity executor dispatch with a
locally generated attempt ID and a server-local sequence under
`conductor_gemini_family`. Explicit re-execution creates a new identity. Only sends explicitly marked `model` by the existing usage wrapper inherit it.
Authentication, metadata and grounding calls do not acquire a model attempt just
by sharing the executor context. Redirects retain the source send's model-owner
label; an auxiliary redirect does not become model-owned. No new business labels
are added, and redirects do not create an attempt.
Direct executor calls without this owner retain null attempt fields. Existing
usage attempt trackers are untouched. Redirect calls use `redirect`.

Every observable RoundTrip under an active server gets a new call span and an
one-based call number allocated under the server mutex. Each hop checks its actual URL,
escaped path, and any conflicting Host override. Response IDs are read only from
configured peers. There is no body pre-read. EOF, early Close, read/transport
error, or cancellation settles once; a cancellation callback is detached after
settlement. Optional body `io.Writer` and `io.WriterTo`, Gin streaming/hijack
interfaces, and transport `CloseIdleConnections` are preserved when present.
The wrapper does not add timeouts, retries, network calls, or flushes.

Injection is performed on the RoundTripper's request clone. Its ownership marker
contains that exact request pointer and is never written into the input request,
`ireq`, or the server context holder. Go redirects rebuild headers from pristine
`ireq.Header`. Thus an initial explicit business trace can reappear on a nonpeer
redirect; it must remain unmarked and must not be deleted. Clients actually
carrying injected headers have separate owned-cleanup vector tests. No redirect
policy is replaced to imitate another language's client. This is Go rebuilding
a new unowned hop from the untouched business input, not this module restoring
a value it overwrote. DIAG-00 R3/coordinator explicitly confirmed this distinction.

## Logging and completeness

`diag.process`, `diag.server`, and `diag.call` use the existing ordinary INFO
access-log gate (`logrus.IsLevelEnabled(InfoLevel)`), independent of DEBUG.
The existing `request-log` option controls legacy request/response body capture;
it is not a basic access-log switch. Suppressing INFO suppresses all new basic
records, while context propagation and response IDs continue.

The logging adapter sends complete `@diag ` lines through the existing logrus
writer/rotation path. The custom formatter recognizes a private Go type and
adds no text prefix. There is no new queue. Lines include LF in the 4096-byte
limit; oversized records become a same-identity/sequence `diag.truncated` stub.
All three delivered synthetic examples are below 2 KiB.
No usage, content structure, credential, raw URL, raw error, model name, or
throttle data is collected in basic records.

The process declares HTTP, normalization, attempt-result, conversion and throttle
capabilities for the paths in the DIAG-05 matrix below. Basic and DEBUG events
share the server's sequence allocator. Calls currently contain one basic
terminal, so each call's own sequence remains 1. Server Finish seals immediately,
waits for already-constructed semantic records, then snapshots counters and
allocates its terminal sequence. No subsequent semantic event may be appended.

Each DEBUG exchange registers a counter under the same lock used to seal the
server. Its once-only Finish unregisters it after its observations have settled,
even if sealing or a DEBUG transition already suppressed them. If an exchange
is still outstanding when the server seals, `debugCapture=interrupted` records
that capture ended before the observation lifecycle settled. This conservatively
reports partial coverage; it does not synthesize an attempt result or EOF, claim
an attempt terminal exists, or wait for an executor's cleanup/read/channel. The
server never reads mutable exchange payload summaries from another goroutine.
Late cleanup cannot amend the earlier terminal or append semantic records.

Pending observations have not necessarily constructed public events: they do
not increment droppedForSpan or reserve fictitious sequence numbers. That
counter continues to describe known constructed-event losses, and the terminal's
expectedLastLogSeq remains its actual sequence. Even contiguous sequences and a
custom acknowledged sink's zero dropped count cannot imply full coverage when
capture is interrupted; the frozen coverage rules give known interruption
precedence over unknown sink/access evidence.

A request opts into DEBUG only at server start. `NotifyDebugDisabled`, called
before the application's existing `util.SetLogLevel` transition, advances a
process epoch. A disabled/re-enabled request remains interrupted and does not
resume collection. Mid-request enablement does not opt in or backfill. Out-of-band
embedders that change logrus directly must notify this module on disable; those
unannounced, unobserved off/on transitions are outside the lifetime guarantee.
The production configuration paths all use the existing utility.

Access capture remains `unknown`; sink-wide dropped total remains null. The
production logrus adapter marks per-span zero-loss acknowledgements unavailable,
so `droppedForSpan` is null unless a positive engine-observed loss is known.
Custom synchronous sinks with acknowledged errors count known per-span drops.
Serialization failure and line oversize use a same-sequence truncated stub;
truncated events are tracked separately. A sink panic is contained and counted.
A terminal cannot retrospectively report its own downstream write failure.
DEBUG lines use the DEBUG logrus gate; basic lines use INFO. Neither this module
nor an offline consumer can claim full production export coverage from these
unknown acknowledgements.

Gin normal Write/WriteString/Flush/WriteHeaderNow commits replace peer `X-Diag-*`
with local IDs; errors and streaming use the same boundary. Existing business
IDs are untouched. A status-only response is finally committed by Gin after its
middleware returns: IDs are prepared without forcing a write, and the record
reports unobserved commit/status/delivery. Raw hijacks are also unknown, because
Gin's cached status is not the raw upgrade status. No websocket protocol changes
are made. CORS exposes the two new diagnostic response IDs through the existing
origin policy.

## Entry and executor coverage matrix

Coverage requires an active diagnostic context; detached jobs and direct SDK
calls without Gin diagnostics are not implicitly attached to a model request.

| Boundary/path | Server context/terminal | HTTP calls and peer propagation | Limits |
| --- | --- | --- | --- |
| Main Gin API, OpenAI/Claude/Gemini model routes | Yes | Through builders below | Existing handler return/write lifecycle |
| Errors, auth rejections, 404, OPTIONS, management HTTP | Yes | Only when a listed builder is actually used | No management business identity changes |
| Empty/status-only responses | Context and terminal | N/A | Final implicit Gin commit unobserved; no invented wire status |
| Inbound WS/raw upgrade | Context and terminal | No browser/message spans | Raw commit/delivery unknown; no diagnostic handshake injection |
| OpenAI-compatible, Gemini API, Vertex, Gemini CLI, Kimi, Meta, xAI HTTP | Yes | `NewProxyAwareHTTPClient` | Model label only where existing usage wrapper owns request |
| Claude and Codex HTTP/SSE | Yes | `NewUtlsHTTPClient`, including protected-host/fallback transports | Existing TLS profiles preserved |
| Codex direct image HTTP | Yes | `NewProxyAwareHTTPClient` | No image/body inspection |
| Antigravity HTTP/SSE/count tokens | Yes | Final `newAntigravityHTTPClient` return | HTTP/1.1/pool configuration precedes decoration |
| Devin Connect-RPC HTTP | Yes | Final `NewDevinHTTPClient` return | Native/custom transport compression branches preserved |
| Local-only Gemini CLI auxiliary passthrough | Yes | After `util.SetProxy`; automatic source filtered | Retains its original cancellation parent; kind `other` |
| Grounding URL/video downloads using shared helper | If caller carries it | Shared builder, kind `other` | No metadata/provider semantics inferred |
| OAuth/refresh through shared builders | Only if still request-owned | Synchronous calls observed as `other` unless labeled | Detached/background refresh has no model span |
| Codex/xAI websocket dial/message paths | Inbound only | Not covered | Existing dialers bypass final HTTP clients; no injected context |
| AI Studio websocket relay/browser | Inbound only | Not covered | Dispatch and browser protocol unchanged |
| Codex Live SDP/capabilities via `AuthManager.HttpRequest` | If original context retained | Depends on selected HTTP executor above | Live websocket/sideband dialers not covered |
| Plugin host normal HTTP bridge | If context survives plugin boundary | Normal shared-builder branch | No claim across RPC context serialization |
| Plugin host custom `WireProfile` client | Inbound only | Not covered | Own request holder/redirect/header ordering boundary; not decorated |
| Management APICall custom client | Inbound only | Not covered | Explicit request-header API, own transport/timeout |
| Background registry/model refresh, standalone auth/store/home clients | No request association | Not covered | No global transport interception or new network calls |
| Context-supplied RoundTripper | Context and outer call boundary | Wrapped after provider configuration | Its private nested sends/URL rewrites are not observable; it must honor the request destination |

The baseline has no `internal/api/modules/amp` directory despite the historical
architecture note; no AMP coverage is claimed. DIAG-05 detailed coverage is specified below; other providers retain HTTP-only
coverage.

## Header source inventory

| Actual source and consumers | Proven provenance | Change/final order/redirect behavior |
| --- | --- | --- |
| `sdk/api/handlers/gemini/gemini-cli_handlers.go`, `CLIHandler` auxiliary branch | Entire `c.Request.Header` automatically copied to fixed cloudcode HTTP target | `CopyInboundHeaders` excludes standard trace and every X-Diag field before copy; final client then applies peer policy on every hop |
| `internal/util/header_helpers.go`, `extractCustomHeaders`, `$Header` branch, invoked by executor `PrepareRequest`/provider header builders | Dynamic lookup in actual incoming/Gin headers, selected by configured attribute | Filter actual inbound traceparent/tracestate/X-Diag source names; reserve only X-Diag destination names. Explicit business-source mappings to traceparent/tracestate remain; legacy IDs and other dynamic headers remain; final configured peer injection occurs afterwards |
| Same utility, literal `header:*` attributes | Explicit configured business header values | Retained, including provider standard trace headers; final peer replacement/nonpeer X-Diag cleanup still applies on covered clients |
| `claude_executor_cloaking.go/resolveIncomingClaudeHeaders`, `claude_executor_request.go/copyClaudeCallerFingerprintHeaders`, `applyClaudeHeadersWithNativeProfile` | Incoming headers used for detection and explicit named fingerprint/session/beta fields | No blanket copy to upstream; allowlists contain no diagnostic/standard trace fields; literals go through the utility above; no extra filtering of detection input |
| `codex_executor_request.go/applyCodexHeadersFromSources`, `applyCodexDirectImageHeaders` | Incoming named Codex/session/agent fields, then configured attrs | No standard trace automatic copy; preserve provider IDs; final uTLS/proxy client applies diagnostics |
| `codex_websockets_request.go/applyCodexWebsocketHeaders` | Explicit named Codex/session headers plus configured attrs | No standard trace automatic copy; dynamic attrs filtered at the shared source; websocket final boundary remains uncovered |
| `internal/client/codex/live/live.go/protocolHeaders`, websocket `directRealtimeHeaders`, sideband constructors | Named protocol allowlists from inbound headers | No traceparent/tracestate/X-Diag allowlist entries; HTTP requests may reach a covered executor, raw WS paths remain uncovered |
| `sdk/cliproxy/auth/conductor_execution.go/NewHttpRequest` | Explicit caller-supplied header map; live/server route call sites construct protocol maps | Do not treat optional header API itself as automatic copying; existing clone and credential preparation unchanged |
| `codex_executor_request.go/applyModelHeaderOverrides` | Registry/model configured literal overrides | Explicit business construction; no source filtering; final covered transport decides peer injection |
| `internal/api/handlers/management/api_tools.go/APICall` | JSON request's explicit headers, separate management API | Not an automatic copy of HTTP inbound header collection; left unchanged and outbound uninstrumented |
| `internal/pluginhost/http_bridge.go`, plugin HTTP API headers | Explicit plugin-provided request object | Not assumed to copy inbound traffic; no blanket filter of explicit API; custom WireProfile not instrumented |
| Executor response `Header.Clone`, shared `FilterUpstreamHeaders`, error response copy, Gemini CLI full response copy | Upstream response fields | Read peer IDs at decorated transport before consumer copying; normal Gin commit removes all upstream X-Diag fields, writes exactly local pair, preserves X-Request-Id/X-Trace-Id |
| `internal/logging/cpa_trace.go` | Local credential-selection callback and old local request ID | Existing X-CPA-TRACE-ID lifecycle and value unchanged |
| New `diagnostics.prepareRequest` / `responseWriter.prepare` | Locally owned span/resource context | Clone-only per-hop injection; pointer-bound marker; normal response commit replacement; no changes to business bodies or error frames |

## Validation

The actual Go parser/final-boundary functions consume 188 shared cases across
headers (59), HTTP ingress (6), peers (52), outbound (8), redirects (4), peer
responses (11), copy sources (2), resources (8), source scope (12), and coverage
(26). Foreign-service resource vectors exercise the same worker algorithm;
the runtime binary service is separately asserted to remain `cliproxyapi`.
Graph/count/semantic/Aito mapping vectors are contract/oracle or later consumer
responsibilities, not claims of a Go graph analyzer or Aito implementation.

Runtime tests additionally use local fake upstreams for Go redirects with an
explicit original business trace, two receiving replicas, concurrent call
allocation, body EOF/Close/error/cancel, upgrade/writer capabilities, peer
reload invalidation, error/stream commits, logging gates, source filtering,
SDK cancellation context, all shared client branches, and usage observer
equivalence. No production model, credential database, or remote config is used.
The synthetic artifact is generated by `TestSyntheticRecordArtifact` using
`DIAG_TEST_RECORDS`; `testdata/validate_records.py` validates it against the
unchanged public schema and semantic invariants.

Deployment stdout aggregation across real workers and external custom-client
internals remain unverified. Use separate per-process files/collection identities
or validate the deployment collector in DIAG-07; a 4096-byte bound is not a
cross-platform atomic-write guarantee. The delivery report records exact test
results and Windows application-control restrictions.

## R1 audit evidence and retained limits

The [R1 disposition and source evidence](../../coordination/diagnostics/20260924/DIAG-04-R1-disposition.zh-CN.md)
records the complete builder/transport inventory, production middleware order,
log consumers, representative real executor tests, and terminal truth table.
The source-excerpt companion contains unchanged code needed for independent review.

The real Execute tests run Codex, Claude (the uTLS builder's loopback fallback),
Gemini and Kimi, each with/without an actual ServerSpan for HTTP 200 and 503.
They check response bytes/error status, request method/path/body fields, HTTP/1.1,
usage tracking, peer injection and a single model call terminal. They do not
claim a fresh native TLS fingerprint/HTTP2 handshake test against protected hosts.
The existing transport tests still cover the configured concrete transport branches.

Successful health probes suppress the old text access line but retain one basic
server terminal while INFO is enabled. This deliberately preserves server-span
completeness across all ingress; DEBUG remains unrelated. Management/Home HTTP
requests also emit terminals. No request sampling or path-level exemption was
introduced. Operators should account for this volume; the INFO switch remains
the shared gate. SkipGinRequestLogging has no production callers in this baseline.

Production Home and TUI hooks both install LogFormatter, preserving the raw
line (TUI trims its newline). TUI ALL/INFO show the new INFO records without old
prefix coloring; WARN/ERROR exclude them. Management cursor reads preserve raw
lines, and its legacy after-timestamp reader now recognizes this schema's UTC ts
so diagnostics cannot inherit a previous text line's cutoff. External embedders
installing arbitrary logrus formatters/hooks are outside this verification.
The existing usage statistics consume typed usage events, not these text lines.

The existing broad call endReason=cancelled means its outgoing context ended,
including a context deadline or http.Client.Timeout; it does not distinguish
caller cancellation from a client timer. A transport error with a live context
remains transport_error. No timeout was added and no timeout classification was
changed in R1. Consumers must not infer the cancellation initiator from this field.
Server client_cancel similarly reports an ended inbound context, not provenance.

Server Finish seals and snapshots under the span mutex, then emits outside it;
a blocked sink cannot block a late call's sealed-span check. Existing response
write ownership remains unchanged: SSE select loops serialize writes, and the
nonstream keepalive stop function waits for its goroutine before the final write.
No new asynchronous writer or backpressure mechanism was introduced. Linux/race
validation remains unavailable in this Windows environment.


## DIAG-05 semantic observation matrix

| Path | Request / upstream / conversion | Throttle |
| --- | --- | --- |
| Gemini generateContent and streamGenerateContent executor | Yes; request contents, raw frames before usage filtering, actual converter output | Gemini HTTP handlers, enabled or disabled |
| Antigravity ordinary Gemini generate/stream | Yes; wrapped request/response handled | Gemini HTTP handlers when used |
| Antigravity internal stream-to-nonstream path (including Gemini 3 Pro) | Yes; original frames and actual collected result, deliveryMode=collected | Same Gemini nonstream handler |
| Gemini-family output to OpenAI chat | Delivered numeric usage, text/thought counts; incomplete tool fragments remain unknown | OpenAI handlers not instrumented in DIAG-05 |
| Gemini-family output to OpenAI Responses or Claude | Delivered numeric usage only; detailed output counts/result unknown | Those handlers not instrumented in DIAG-05 |
| Native Gemini Interactions, count-token, auxiliary passthrough | Not covered by these semantic hooks | No claim |
| Antigravity compaction | Recursive summary `e.Execute` is observed as an ordinary exchange under the outer attempt identity; the compaction wrapper/capsule is not separately observed | No dedicated compaction throttle |
| Other executors, custom plugins, browser/WS paths, new-api | HTTP matrix above only where applicable | No new dedicated logic |

`protocol.go` retains only bounded numeric summaries, never complete response
bodies, arguments, signatures, hashes, names or raw errors. Inspection operates
on bytes already read by the executor. JSON observations are bounded to 4 MiB
and depth 64; an exceeded bound produces unknown observation without
changing the business parser. No frame buffering, additional read, request,
flush, timeout, retry or response channel is introduced. Upstream frames are
observed before the existing SSE usage filter, and converted chunks only after
their existing channel send succeeds. Cleanup failures remain the existing
business/logging responsibility and cannot replace the recorded generation result.

Request before/after counts and the last four message structures distinguish
caller-supplied empty turns from resulting added empty turns. The frozen contract
has no `insert_empty` operation or phase label. Only differing `contents` positions (up to 16) use the allowlisted `other`
operation/reason. Envelope/model/config-only changes add no transformation;
position comparisons do not reconstruct edits or claim a removed user, a
particular cleanup rationale or any gcli2api normalization. Model aliases and
credential references remain null; no supposedly-safe model/account string is
accepted from business data. Other input protocols have unknown request structure.

Usage is observed independently from accounting. Only explicit finite,
nonnegative safe integers are present; zero remains present. Raw fields are
protocol-allowlisted. Gemini candidate and reasoning counts remain separate;
outputTotal is their observational sum only when both are explicitly known.
An absent component is not invented as zero. The converted protocol's actual
output field is read directly, including zeros its converter may synthesize.
OpenAI completion/output totals and Claude output totals already include reasoning
on these translators; reasoning is not added again. Repeated cumulative usage
snapshots replace rather than sum earlier snapshots. These projections neither
feed nor modify usage.Detail, the existing token accounting, or billing. The
number 87 is used solely as one fixture's 7+80 output.

Upstream success requires observed terminal evidence for every observed Gemini
candidate, complete parsing and effective ordinary text, valid-shaped tool call,
or media part. Thought-only and whitespace-only output are not ordinary text.
Error frames and read errors override success. Native client receipt is never
inferred from a successful local conversion or HTTP 200. For protocol fields or
validity not observed, counts/result remain null/unknown instead of guessed.

Timing source is server_monotonic. `firstEffectiveOutputMs` is measured from this
server's start at its first effective frame observation, independently per
exchange. First raw byte, response commit and first downstream effective timing
are currently null: header arrival or a parsed frame is not a first-byte clock.
`attempt.totalMs` starts at the conductor's executor dispatch when that owner is
present, otherwise at the directly observed HTTP operation's start. No prior
attempt, wall clock, legacy stat or remote timestamp is reused. Direct calls with
null attempt identity must not be grouped into a fabricated business retry.

The throttler observes its already-randomized effective rate/TTFT and the actual
wait branches. `configRevision=unversioned` explicitly records that the existing throttler has
no config revision counter; the rate and delay are the actual immutable selection. `tokenCount` is the token
numerator actually considered by this limiter, including a canceled pending
chunk; it is not evidence that those tokens reached the client. Streaming uses
the existing per-chunk text estimate and does not reconcile a trailing cumulative
usage frame. Nonstreaming records the selected provider-output, provider-candidate or estimated
branch and the existing minimum-one clamp. Planned waits sum positive residual
waits after upstream elapsed time; actual waits include only time in those wait
branches, including cancellation. Disabled/no-wait paths do not invent token use.

See the DIAG-05 delivery for actual tests, synthetic artifacts, source formulas,
local DIAG-07 entry instructions and environmental restrictions. Production
multi-worker collection and external logrus lifecycle changes remain unverified.


## DIAG-05 coordinator R1 follow-up

The deterministic cancellation regression uses the real Gemini Gin handler,
conductor, executor and diagnostics middleware. Both upstream-wait and
throttle-wait cancellations hold Body.Close behind a channel until Gin has
returned and emitted its server terminal. The test releases cleanup only after
checking interrupted coverage and absent attempt/conversion terminals; it never
drains or closes the business stream. Pending-exchange unit tests also cover
multiple registrations, concurrent duplicate Finish, normal completion before
sealing and rejected post-seal registrations.

The auxiliary-call regression uses one actual conductor attempt context for
model/other/auth/metadata sends and Go-managed redirects through the real helper
client/usage wrapper. It additionally invokes the actual Antigravity grounding
HEAD helper with an in-memory transport. The helper retains its existing redirect
policy and output. Transport projection now requires the pre-redirect model
label, because these auxiliary helpers contain no explicit owner association;
context inheritance alone is insufficient evidence. The contract's explicit
owner-association exception is preserved for future proven owners; it is not
inferred for these paths.

See `coordination/diagnostics/20260924/DIAG-05-R1-disposition.zh-CN.md` for the
reproductions, exact test outcomes, environmental restrictions and revised HEAD
handoff. This is a coordinator-requested revision, not a Claude approval.


## DIAG-05 R2 review follow-up

Live scanner read errors settle the exchange before the existing Err send in
Gemini and Antigravity. Gemini retains its preceding DONE conversion and sends;
Antigravity retains its clean-only tail branch. Native Gemini and OpenAI chat
converters produce no DONE payload here. Gemini's Responses converter may produce
`response.completed`; the real Responses forwarder still consumes Err but its
existing framer suppresses an additional wire error after that terminal. Tests
preserve this business behavior while requiring the diagnostic read failure
before server sealing. No new wait, drain, close, retry, EOF or business frame is
introduced. Cancellation still uses deferred cleanup and the pending/interrupted
mechanism. An external client cancelling during tail delivery can still interrupt
capture; this revision does not guarantee late delivery snapshots after sealing.

Size/depth/candidate limits are local observation limits: result/origin/stage/error
are unknown unless stronger failure/cancellation/block evidence exists,
parserFinishOk and terminalSeen are null, and both upstream and delivered output
aggregates are null. Already observed explicit usage snapshots are retained as
observed evidence, not a claim that the final provider usage was seen. Actual
malformed JSON/non-object payloads retain parse_error; unsupported string finish
reasons retain terminal evidence and verified output counts with an unknown
result. Neither case becomes success. The closed schema is unchanged.

The retry scope is `conductor_gemini_family`: attemptNo counts only those actual
conductor dispatches, not all providers in a mixed pool or inner HTTP retries.
Recursive compaction summary exchanges reuse that outer identity; consumers must
not infer another conductor attempt from another exchange record.

DIAG-06 handoff: process capabilities advertise implemented event families, not
per-route semantic coverage. Non-Gemini routes can have enabled_throughout DEBUG
capture with no semantic events; use the matrix above rather than capabilities
alone to determine applicability. Tests that change the process-wide DEBUG epoch
or logger configuration must remain serial (no t.Parallel); this is not a
per-test epoch. Empty conversion results remain conservatively unknown because
the observer does not prove downstream protocol completion merely from a zero
output count. Upstream empty classification remains independently available.
