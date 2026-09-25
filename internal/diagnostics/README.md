# CLIProxyAPI diagnostics v1

This module consumes the frozen `contracts/diagnostics/v1` artifact from commit
`bb291667f7b6bd7a1dab6f9b7f906b5871d1306c`. Its wire schema is
`ai-proxy-diagnostics/1`, artifact `1.0.0-rc.1`, manifest byte digest
`ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4`.
The contract directory is unchanged. This implementation is subject to DIAG-04
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
unclassified synchronous helper calls use `other`. No business attempt is
invented: attempt ID/number/retry scope remain null for DIAG-05 to observe at
their actual owner. Redirect calls use `redirect`.

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

This foundation declares only HTTP inbound/outbound capabilities. It constructs
no DEBUG semantic records, even when the application's DEBUG switch is on;
`debugCapture=none`. Terminal log sequence is currently 1, allocated on terminal
construction. DIAG-05 must extend the span's sequence allocation when adding its
four semantic observations, and preserve sealing and terminal invariants.

The shared logger has no lifetime switch notification or downstream collector
loss acknowledgement. Access capture is therefore conservatively `unknown` and
sink-wide dropped total is null. The engine counts serialization/size failures
and errors explicitly returned by a supplied sink. The production logrus adapter
always returns nil: logrus reports writer errors to stderr, not back to this
engine. Its in-memory counter therefore does not observe actual logrus writer,
Home/TUI queue, rotation, or collector losses. Do not interpret
these terminals as `full` debug coverage. Source-scope and coverage helpers apply
the shared vectors; they neither merge events nor establish remote edges.

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
architecture note; no AMP coverage is claimed. No provider-specific detailed
semantic observation is added by this task.

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
