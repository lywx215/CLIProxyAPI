# Diagnostics v1 contract candidate

Status: **pending coordinator code inspection and Claude review**. This is a
reviewable freeze candidate, not an approved release or an implementation.
Wire identifier: `ai-proxy-diagnostics/1`. Artifact version: `1.0.0-rc.1`.
The coordinator alone records the approved full Git commit and manifest digest.
Changing any candidate byte invalidates an earlier review. Consumers must copy
this directory byte-for-byte from the approved commit, record both identities,
and run the vectors in their own language. Do not invent a private v1 variant.
Each consumer repository must also apply `contracts/diagnostics/v1/** text eol=lf`
in its own `.gitattributes` (adjust the prefix if copied elsewhere), and verify
both checked-out and indexed bytes. Copying files alone is insufficient on Windows.

## Contents and precedence

`record.schema.json` defines the strict public exchange envelope and event data;
`peers.schema.json` defines parsed DIAG_PEERS; `bundle.schema.json` defines export
provenance. `vectors/*.json` are language-neutral input/output cases.
`fixtures/valid` and `fixtures/invalid` exercise schema validation. `examples/`
contains synthetic JSONL, never captured model traffic. `validate.py` is an
offline contract-test oracle, not a runtime library, production parser, exporter,
or DIAG-06 analyzer. `requirements.txt` pins its validation dependencies.

This document, schemas and vectors must agree; disagreement blocks approval.
The complete Chinese plan explains implementation scope. The original Claude
review and its original plan snapshot are historical evidence, not normative
instructions (in particular, their always-on usage, host-only allowlist and WS
changes were not adopted). Required/nullable fields must be present; optional
event data is never filled with invented zeroes. All objects reject unknown
properties. Extending this allowlist requires coordinator review; legacy logs
need not conform until explicitly projected into the public exchange format.

## Identity and counts

Resource identity is local `environment/deploymentId/service/instanceId/bootId`.
Never read resource fields from a request. Environment and deployment default to
`unassigned`, nodeLabel/buildCommit to null. The service is fixed by the binary.
Use a confirmed platform replica UID, then a unique configured instance ID, else
a random UUID; record `platform`, `configured` or `ephemeral`. Configuration
must not copy one instance ID across replicas. Generate a random UUIDv4 bootId
once per actual worker, after fork. Multiple workers can share instanceId but
not bootId. A restart gets a new bootId. Existing Aitoapi bootId may be reused
only if it already obeys this lifecycle. No identity files, registry or host/IP
inference. buildCommit identifies code, not a machine. pid is auxiliary only.
The same instanceId with different bootIds is not by itself evidence of bad
configuration: legitimate workers/restarts have this shape too. Keep these
processes distinct; report conflicts only when there is additional contradictory
identity evidence, without claiming that configuration errors are always detectable.

Resource labels are operator-selected non-sensitive tokens (1–64 ASCII
alphanumeric/`._-`); instanceId permits 128 characters. Invalid optional resource
configuration falls back as above with a bounded reason code, never the value,
and must not stop model service startup. Resolve reliable platform identity
before the configured fallback; invalid/absent platform identity is unavailable.

Each inbound request creates a fresh **server span** and local requestId using
the existing service convention. Local request IDs are not globally unique.
Each observable actual outbound HTTP request or server-side browser dispatch
creates one **call span**, parented by its owning server span. `serverSpanId`
explicitly carries this local owner on every request-scoped record (equals
spanId for server records). HTTP traceparent carries the call span ID. Each call
gets a concurrency-safe, one-based callNo within its server. No attempt spans.
Unobservable TCP retransmission is not a call. Visible redirects are separate
calls with callKind=redirect, retaining the original retry owner's attributes.

Attempts belong to business code: `(resource, serverSpanId, retryScope,
attemptId)`. If no attemptId exists, use attemptNo only when the retry owner
guarantees one-based uniqueness in that server/scope; otherwise leave both null
and report unknown counts. An attempt can own several calls. Do not infer
attempts from HTTP errors, parent reuse, or transport entries. Request failures,
call failures and attempts are separate totals. Authentication/metadata/other
synchronous auxiliary calls have explicit callKind and null model attempt
unless the owner truly associates them; detached work starts its own context.

Event identity is `(service, instanceId, bootId, spanId, logSeq)`. Process events
use null spanId and their own boot-local sequence. Importers also retain source
file and line. Same key plus structurally equal JSON is one event with multiple
provenances; a different payload is an event_conflict, retaining all versions.
Changes in environment/deployment under the same identity are conflicts, not
silent overwrites. Span identities reused across resources/trace IDs are flagged.
Never deduplicate on requestId, caller ID, timestamp, message or stream `seq`.

## Headers and source scope

Use a conforming propagator for [W3C Trace Context Level 1 (2021)](https://www.w3.org/TR/2021/REC-trace-context-1-20211123/).
Header names are case-insensitive. Application inputs are **framework-parsed
per-field values**, with protocol OWS already removed, retaining field order and
multiplicity. Go uses header value slices, ASGI uses its field-pair list, and Node
must use `headersDistinct` or `rawHeaders` for both traceparent and tracestate,
not comma-joined `req.headers`. Do not read raw sockets or reconstruct discarded
wire whitespace. `http-ingress.json` distinguishes wire examples from observable
fields; `headers.json` labels synthetic parser-unit negatives separately.
Multiple traceparent values (even identical or comma-joined) are invalid.
Use the Level 1 future-version algorithm; emitted context is version 00, with
only the sampled bit retained. New roots use flags 00. Invalid/missing context
creates fresh trace/server IDs without changing model HTTP behavior. Invalid
traceparent discards tracestate; invalid tracestate alone does not discard the
traceparent. Combine multiple observable tracestate values with exactly `,` in
field order (no extra spaces inserted by Node's merged-header view), validate keys
and limits, and emit at most one field. This profile drops the entire tracestate
above 512 ASCII bytes or 32 nonempty members; empty members are ignored. No
proprietary member is added. Traceparent is bounded to 512 bytes as an explicit
local resource limit, not a universal W3C length rule. Measure the observable
field value; wire OWS removed by the HTTP parser cannot be counted. The application
does not trim the supplied field again. Whitespace inside the fixed 55-character prefix is invalid;
future-version extension bytes after the required hyphen remain opaque and may
include ASCII spaces. As a local profile restriction, a comma is forbidden
anywhere, including unknown extensions, to reject merged multi-value context.
Control/non-ASCII extension bytes are also rejected. No header value is logged.

Custom IDs must be a single value, 1–128 ASCII characters from `[A-Za-z0-9._:/-]`.
Do not additionally trim custom IDs at the application boundary. Internal
whitespace is invalid; HTTP `X-Request-Id:  a ` normally arrives as `a` and is
valid. A synthetic parser-unit input ` a ` is invalid but is not a claim about
that wire request. Reject duplicates, comma lists, empty/control/non-ASCII
values; record only the rejection enum and bounded length, never the input.
X-Diag-Trace-Id on a response additionally must be a nonzero lowercase 32-digit
hex ID. Invalid peer IDs become null, with rejection metadata; no graph identity
is fabricated. requestId must have a header-safe representation; if an existing
internal ID does not, retain it internally and report this adapter gap instead
of silently changing queue/cancellation keys.
Only a configured peer's response contributes peerRequestId/peerTraceId.
For other targets, ignore X-Diag-* response values and report null IDs with
peerIdRejected=none. `not_configured` is not a rejection code. These ignored
values must still be stripped at the local response commit boundary.

Default callerRequestId source is X-Request-Id. A **locally authenticated,
explicitly configured inbound adapter** may prefer X-Diag-Request-Id, then
X-Request-Id. A rejected preferred value falls back to the next valid value and
retains a rejection code. DIAG_PEERS is outbound configuration and never
authenticates inbound callers. Other caller header adapters require an explicit
project mapping and review. callerIdSource records `header` and `trust`
(`unverified`, `authenticated`, `configured_peer`, `none`); it never claims that
a head name proves a new-api origin. callerAlias comes from existing auth state,
never a caller header or raw/key-hashed credential. Its scope is `deployment`,
`boot` or `unknown`: use an existing safe internal ID, else an in-process random
mapping. Boot-scoped aliases include instanceId and bootId in query scope.

Caller lookup scope is environment/deploymentId/service/callerAliasScope/
callerAlias, plus instanceId/bootId for boot scope. Unknown alias or unassigned
deployment widens the search and lists all candidates, never merges them.
Duplicate caller IDs across principals or boots are distinct candidates. Even
fully matching scoped caller IDs are search aliases, not graph edges or billing
keys. X-Trace-Id, X-Diag-Trace-Id and X-Correlation-Id never establish inbound
parentage. No ID controls auth, account selection, cancellation, DEBUG or billing.

At **every automatic inbound-to-outbound copy source**, exclude traceparent,
tracestate and all X-Diag-* before copying; this is not a global deletion of
headers deliberately constructed by a provider adapter. Inventory existing
copy/write points, including error, retry and redirect branches. Do not infer
header provenance from a value or from its resemblance to incoming context.
Apply filtering only to proven automatic copies, not merely to an API that
accepts optional explicit extra_headers. Inventory the call sites and actual
arguments first; if no automatic copy exists, do not change that path. This
contract does not assert that current gcli2api routes leak inbound context.

On an allowed peer request, after existing business headers are built, operate
on a per-request copy: case-insensitive remove/Set traceparent, tracestate and
all X-Diag-*; emit only traceparent, optional valid tracestate, and
X-Diag-Request-Id=local requestId. No request X-Diag-Trace-Id is needed.
Existing X-Request-Id/X-Trace-Id retain their project/provider semantics. Do not
globally delete those business fields. When injecting traceparent/tracestate,
the diagnostics module sets a minimal request-local ownership marker for that
pair; the marker is local metadata, never an HTTP field, body property or log.
For a non-peer request, always remove X-Diag-*; remove traceparent/tracestate
only when this module owns them, then clear its marker. Other existing trace
fields belong to explicit business construction after the source filtering and
remain untouched. There is no `providerOwnedTrace` value supplied by a final hook.

Every redirect retains ownership metadata until diagnostic-owned fields have
been removed from the new request copy, **before** any new provider header writes.
This applies to headers actually carried to that next hop. A client rebuilding
from a pristine initial request may already have no module-owned fields and an
empty marker; never mutate the original request to force header inheritance.
The final boundary rechecks the actual destination: re-inject a new call context
and mark ownership for an allowed peer, or leave the cleaned non-peer request
unmarked. This includes same-origin redirects outside the allowed path. It never
restores previously overwritten provider values; a provider adapter may construct
its new-hop fields through its existing supported path after cleanup. An owned
pair left on a non-peer request is also removed defensively by the final boundary.
Do not guess ownership by comparing values. Preserve the existing redirect policy;
if metadata/cleanup cannot be retained on a path, report it uncovered and do not
inject internal context on that path. No new redirects, requests or retries.

At normal response commit, replace all peer X-Diag-* with exactly one local
X-Diag-Request-Id and X-Diag-Trace-Id. Capture IDs only from configured peers
before replacement. Do
not change existing X-Request-Id/X-Trace-Id, force an early 200, add SSE frames,
rewrite JSON, or widen allowed CORS origins. Cover error and streaming commits.

## DIAG_PEERS exact matching

DIAG_PEERS is a JSON array (unset/empty string means `[]`), at most 64 entries:

```json
[{"alias":"gcli-pool","origin":"https://gcli.example.test","pathPrefix":"/v1","service":"gcli2api","deploymentId":"gemini-pool"}]
```

All five fields are required. `deploymentId` is null if unknown; it must not be
invented from the hostname. The last two fields are expected peer evidence, not
actual instance identity. Do not export origin or raw URL into event logs.
Configuration is read locally, never accepted via request headers. Invalid JSON
(including duplicate object keys),
entry, duplicate alias or overlapping route entries disables **the entire peer
set** for that snapshot, records only config_invalid, and leaves business routing
untouched. Hot reload atomically replaces the snapshot; invalid reload disables
propagation, rather than retaining a surprising old authorization. No wildcard,
DNS lookup, suffix match, userinfo, query, fragment or regex. HTTP is permitted
for explicitly configured local/private peers; HTTPS and HTTP are different.

Canonical origin is scheme (`http`/`https`, lowercase), ASCII DNS host
(lowercase, no trailing dot), canonical IPv4 or bracketed RFC5952 IPv6, and
effective port (80/443 defaults). Explicit default ports match omitted defaults.
Reject Unicode hostnames (configure their punycode ASCII form), IPv6 zone IDs,
noncanonical IPv4 numeric spellings, invalid ports, backslashes and percent escapes in
authority. Configuration origin may have no path or a single `/`, normalized
away. No path/query/fragment can be embedded in the configured origin.
The numeric-spelling rejection applies to IPv4 alternatives such as leading
zeroes or integer/hex forms. Valid IPv6 spellings, including expanded forms,
are accepted and normalized to RFC5952 form before comparison.

pathPrefix is `/` or an ASCII slash-separated path using alphanumeric/`._~-`
segments, maximum 256 characters; no trailing slash except `/`, empty, dot or
dot-dot segments, percent encoding or backslash. Matching is case-sensitive on
the outgoing escaped path and requires equality or a following `/` segment
boundary. `/v1` matches `/v1` and `/v1/chat`; not `/v10` or `/V1`. `/` explicitly
authorizes the whole origin. Target query does not affect matching and is never
logged. Target fragments, dot/empty segments, any percent-encoded path or
backslash conservatively suppress propagation (not the business request).
This intentionally narrower target policy avoids cross-language URL-decoding
disagreements; adding encoded route support is a future reviewed change.
Match the actual destination origin and escaped request-target path available
at the final send boundary, after the client's URL normalization, not the original
configuration spelling or base-URL input. A dot segment already removed by httpx
cannot be observed and must not be reconstructed. A dot segment still present
in the actual request-target suppresses propagation. Vectors explicitly identify
this boundary; equivalent original URLs need not retain equivalent spellings.
Same-origin overlapping prefixes are invalid, even if their
alias/service values agree; no order-dependent first-match rule. Limits and
canonicalization are tested independently of the JSON Schema shape checks.

## Event data, DEBUG and privacy

The flat envelope has resource, trace, caller, attempt and sequence fields.
`data` holds the event-specific schema; `recordKind` is `basic` or `debug`.
`spanKind` is `process`, `server` or `call`. Nullable fields are explicit null;
logSeq starts at 1. ts is UTC with milliseconds. Times in new measurements are
nonnegative monotonic elapsed milliseconds, not cross-host timestamp subtraction.
Counts are nonnegative JSON integers up to 2^53-1 for all languages.

| Event | Required meaning |
| --- | --- |
| diag.process | Worker startup/config change, resource identity, pid, access/debug switches, config revision, declared capabilities, sinkDroppedTotal |
| diag.server | Exactly one local terminal per server, route template, committed status, wireStatus, endReason, deliveryState, elapsed time, actual callCount and coverage |
| diag.call | Exactly one terminal per call, targetAlias, peer expectation, callKind, actual upstream status, endReason, elapsed time, sanitized response IDs and coverage |
| request.normalized | DEBUG: bounded before/after structure and transformation reasons; never message contents |
| upstream.attempt_finished | DEBUG: owner attempt outcome, usage, output counts, terminal/parser evidence, failure classification |
| response.converted | DEBUG: protocol and delivery mode, upstream versus delivered usage and output counts |
| throttle.finished | DEBUG: enabled/config revision, actual rate/token basis and waits, cancellation |
| diag.truncated | A bounded replacement at the original identity/sequence; originalEvent, originalRecordKind, reason; never the rejected payload |

Basic records obey the existing access-log switch, independently of DEBUG;
disablement/failure can leave gaps. They contain **no usage, model-content
summary, credentialRef, normalization details or throttle values**. No hidden
detailed collection when DEBUG is off. Existing stats and business protections
remain unchanged. New detailed events, including severity WARN/ERROR, always
obey local DEBUG. Only these four common semantic observations are required;
existing generation events can be adapted without duplicating their payloads.
`legacyEvent` is optional allowlisted provenance in DEBUG data, never a second
terminal. Legacy schemaVersion, seq and deliveryOutcome are left untouched in
the project's existing log. Public projection is separate from legacy shape.

Unknown usage is null with present=false; observed zero is value=0/present=true.
Usage metrics include source and basis. Raw numeric token fields are allowlisted
by protocol (no arbitrary upstream object), including OpenAI `total_tokens` and
Gemini `cachedContentTokenCount` when present. These raw fields do not create new
business counters or redefine normalized output/reasoning totals. Preserve candidate/reasoning/output
separately; completion_tokens may already contain reasoning. Never sum repeated
cumulative frames or different attempts into deliveredUsage. A value of 87 has
no special classification. Without DEBUG these details are unavailable.
Output summaries count text UTF-8 bytes, ordinaryTextNonWhitespaceChars, thought
bytes, valid tool calls/media and candidates independently. Whitespace bytes do
not prove effective text; zero text alone is not empty. Success requires
known valid termination and valid effective output; partial parsing cannot prove
success. wireStatus=200 can coexist with error/incomplete semantics.
deliveryState=local_finished proves only local finish, not caller receipt/billing.

All producers use explicit allowlists at every nested object. No arbitrary
exception/message, headers, full URLs, body, prompt, tool arguments, image,
signature, key, cookie, email, account filename or raw auth object. Schema
validation is necessary, not proof of redaction: even an ID-shaped secret must
never be supplied as an alias. Only named allowlisted reason/error enums enter
public data. Unknown provider strings become `other`/`unknown`, not verbatim.
No fingerprints or hashes of model text are added in this release. Existing
legacy hashes remain outside the public v1 exchange projection.

Basic lines target <=2048 UTF-8 bytes; **all** lines including prefix and LF must
be <=4096 bytes. Drop optional fields first; if still too large emit a schema-valid
diag.truncated stub with the same identity/logSeq and recordKind. A basic stub
must not retain detailed fields. If even a stub cannot be written, record loss
where possible. Pure JSONL or exactly `@diag ` plus JSON through the process's
unified writer; never wrap in another text prefix. This is not an atomic-write
guarantee. Verify multi-worker stdout collection or use separate process files.
Reuse bounded sinks, reserve terminal capacity, do not create new generic queues
in all projects. Basic terminals never enter a DEBUG-cleared queue.

## Lifecycle and completeness

HTTP calls settle once at body EOF, early Close, read/transport error or cancel,
not at receipt of headers; failed connection attempts still have call terminals.
Server terminal follows local send finish/error/cancel, not generator creation.
Preserve reader/writer/upgrade interfaces, cancellation, backpressure, redirects,
proxy/TLS settings and usage counters. Do not pre-read, buffer full model bodies,
add network calls, retries, timeouts, flushes or rate-limiting changes.
ContextVar tokens reset only in their owning context; immutable call handles
isolate concurrent work. Capture fields before asynchronous enqueue/flush. Seal
the holder at terminal. Late observations stay associated with their old attempt
in existing bounded legacy diagnostics; public v1 does not append events after
that span terminal or rewrite its earlier snapshot. Never relabel them current.

Allocate one logSeq per constructed public event immediately before enqueue,
including a terminal and a replacement stub. Not constructing a gated event
uses no sequence. All public basic/debug events of a span share this sequence.
`coverage.expectedLastLogSeq` on a terminal equals its own logSeq;
`droppedForSpan` is known count up to the snapshot or null, not sink-wide loss.
`sinkDroppedTotal` is boot/sink cumulative evidence only. Coverage records
debugCapture (`none`, `interrupted`, `enabled_throughout`, `unknown`) over the
**whole** lifetime, not just endpoint switches; debugCapture=none includes a
request not opted into mid-request enablement. Track truncatedEvents separately.
Basic access disablement during a span is recorded as `accessCapture`.

A diag.truncated whose originalEvent is diag.server/diag.call proves that a
terminal was constructed: it is a terminal **stub**, not a full terminal or
bilateral outcome record. Count it separately from full terminals, set
terminalMissing=false, and mark debugCoverage=partial due to known truncation,
even though its coverage snapshot is absent. Never infer success, complete
coverage or a verified edge from the stub. Other stubs do not establish a terminal.

Offline debugCoverage precedence: known interruption, loss, truncation, conflict
or sequence gaps => `partial`; else missing/contradictory terminal or unknown
capture/counters => `unknown`; else debugCapture=none => `none`; else
enabled_throughout, accessCapture=enabled_throughout, exactly one valid terminal,
and contiguous 1..expectedLastLogSeq with zero known loss/truncation => `full`.
Missing basic terminal also independently sets terminalMissing. A post-terminal
sink loss can only be detected with export/sink evidence. `full` is relative to
the declared instrumented capabilities and observed export, never proof against
undetectable crashes, uninstrumented paths or truncated export tails. Do not
infer a stage never ran merely because its DEBUG event is absent.
In particular, accessCapture=unknown prevents `none` as well as `full`.
accessCapture=none alongside a full basic terminal is contradictory and yields
`unknown` absent a higher-priority known loss. Duplicate full terminals, or a
terminal logSeq disagreeing with expectedLastLogSeq, also yield `unknown` unless
an actual event conflict/gap/truncation already requires `partial`.

## Bilateral evidence and conflicts

After exact duplicate removal, use traceId only to find candidates. Verify a
remote edge only when one diag.call (T,S) and one diag.server (T,parent=S)
uniquely pair, with peerConfigured=true and expected service/deployment (if
known) matching the receiver. Actual peer instance is from receiver resource,
never targetAlias. If callerRequestId came from configured X-Diag-Request-Id,
it must equal the caller's requestId. If response peerRequestId/peerTraceId
exist, they must equal the receiver requestId/traceId. Missing optional response
IDs alone do not invalidate unique bilateral evidence. Unknown source provenance
or untrusted imported files cannot be upgraded to verified. Trust is declared by
the importer for a controlled export; it is not asserted by the record itself.

| Classification | Meaning |
| --- | --- |
| verified | Unique, conflict-free bilateral evidence from controlled sources; not cryptographic proof |
| external_parent_unverified | Receiver declares a parent but no matching call is available |
| missing_peer | Configured peer call has no matching receiver terminal |
| ambiguous_parent | Multiple sender candidates or receivers for the same parent; no guessed retry |
| context_mismatch | Observed response trace differs; cannot alone assert a process restart |
| peer_conflict | Known service/deployment/request/source disagreement |
| event_conflict / span_conflict | Reused identity with disagreeing contents or ownership |
| untrusted_source | Pair exists but source control is not established |
| not_participating | Third-party HTTP/browser dispatch has no expected instrumented receiver |
| root | No declared inbound parent; independent root candidate |

Diagnostics are a set of findings, not an overwriteable success enum: retain all
applicable conflicts; no verified edge if any conflict applies. Missing local
serverSpanId owner is separately missing_local_parent. Detect cycles and keep
them as span_conflict; never create a recursive infinite tree. Same request ID
across workers, different roots under a trace and repeated inbound traceparent
remain distinct nodes. Provenance time ranges and clock skew never repair an edge.

## Aitoapi adaptation

| Existing field/event | Public projection and limit |
| --- | --- |
| request_id / requestId | local requestId; preserve queue/cancel/management key |
| request_attempt_id / attemptId | attemptId under local server + retryScope; keep original attempt lifecycle |
| seq / recentEvents[].seq | stream sequence only; allocate independent logSeq |
| deliveryOutcome=success | deliveryState=local_finished only when local response finish is known |
| deliveryOutcome=aborted/error/unknown | map only verified local cancellation/failure; otherwise unknown; preserve legacy value |
| logsDropped | sinkDroppedTotal; never droppedForSpan |
| firstEffectiveMs / browserDurationMs | Keep in existing gated legacy logs/statistics; do not project into public v1 timing |
| new reliable monotonic server observations | Public timing metrics, timingSource=server_monotonic; unobserved values null |
| generation.attempt_finished | upstream.attempt_finished, with existing attempt/result/usage evidence |
| existing conversion boundary | response.converted only if actual delivered protocol usage is observable |
| generation.request_finished | DEBUG semantic evidence only; not a replacement for independently gated diag.server |
| generation.dispatch / browser_closed | server-side call start/terminal using request/attempt/socket association; no browser child span |
| observationComplete | parser/business observation flag; never public log completeness |
| schemaVersion / bootId / buildCommit | retain legacy meanings; independent diagnosticSchema; verified worker boot lifecycle |

No browser script/WS protocol, queue, ACK validation, retry, stats or management
schema changes. The public holder must not enter proxyRequest spread/serialization.
Unmapped legacy events stay in existing gated logs; they are not exported by
copying arbitrary nested fields. `aito-mapping.json` contains mapping vectors.
The public timing object only contains new, reliably identified monotonic server
measurements; its source is fixed to server_monotonic, including when all values
are null (no observation asserted). Never fill it from legacy wall-clock/browser
fields or tag mixed clocks with a single source. No recalculation of Aitoapi
business durations/statistics, and no required new timer just to populate a field.

## Export and versioning

bundle.schema.json lists contract version/digest, per-file SHA-256, relative
sanitized source alias, resource identities, time range and capture/sink evidence.
No absolute user paths, credentials or original text logs in the export bundle.
Only schema-valid structured records are eligible. Malformed diagnostic lines
are counted with file/line/byte-length/reason, never copied verbatim into reports;
retain the original locally for investigation. Unknown schema versions are
reported, not coerced. Bounded imported line/file/record sizes belong in DIAG-06.

The minimum supported validation baseline for this artifact is Python 3.12;
the verified interpreter is CPython 3.12.10 with the locked requirements.
No compatibility claim is made for older Python versions. Newer interpreters
must rerun the vectors; they have not been verified in this delivery.
Run `python contracts/diagnostics/v1/validate.py` with requirements installed.
`--write-manifest` deliberately regenerates SHA256SUMS after validation; regular
validation never modifies it. Manifest entries cover every file in this directory
except SHA256SUMS and transient Python cache files, sorted by relative POSIX path,
formatted `<lowercase sha256><two spaces><path><LF>`. Files are UTF-8 without BOM,
LF and a final newline, with `.gitattributes` preserving bytes. The **contract
digest is SHA-256 of the exact SHA256SUMS bytes**; no self-hash or Git SHA is
embedded. Git source commit is recorded separately in coordinator/consumer
reports. Version metadata must not assert approval. A future breaking contract
needs a new diagnosticSchema major version; pre-approval fixes keep rc.1 but
change the digest and require another exact-HEAD review.
