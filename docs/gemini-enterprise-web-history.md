# Gemini Enterprise Web serialized history

This local implementation adds opt-in, self-contained conversation context to the
Gemini Enterprise **Web** input channel. Ordinary Gemini Enterprise requests keep
their existing text route, executor, credentials, history conversion and settings.

## Configuration and rollout

```yaml
upstream:
  gemini-enterprise-web:
    enabled: false
    history-mode: single-turn
    max-concurrent: 2
    max-inflight-bytes: 268435456
```

`single-turn` is the default. Select `serialized-context` in the configuration
panel's Gemini Enterprise Web section to accept multi-turn media requests.
Changing the history mode does not enable the channel. Settings use the existing
v8 configuration API and hot reload. No service restart is needed for this field.

The request keeps the history mode captured by its admission lease, including a
permitted pre-session credential fallback. Changing the mode, closing/reopening
the channel or lowering its limits does not erase existing resource usage.

Historical deployment evidence is separate from current local integration
checks. See the [verification report](gemini-enterprise-web-phase1-verification.md).
The installation default remains `single-turn`; local changes do not update an
existing deployment or credential.

## Compatibility contract

Any inline attachment in current or historical `contents` routes the complete
native Gemini request through Web. A text-only request still uses the ordinary
Enterprise route. The client keeps its public model name and needs no special
header or sticky server routing.

Each request validates its complete input before authentication, creates a fresh
Web session, uploads all attachments into that session and generates one new
answer. Conversation state is entirely supplied by the request; no previous
node, session, file ID or credential is consulted to reconstruct it.

The query consists of a fixed instruction followed by JSON with schema
`enterprise-web-context-v1`. The JSON includes `system_instructions`, `history`
and `current_input`. Content indices, roles, message boundaries and part order
are preserved. JSON encoding keeps client text inside its own fields. Consecutive
messages with the same role remain separate and their parts are checked per
original Content.

This is a contextual compatibility representation, not Gemini's native role
hierarchy. It does not promise native system-instruction precedence or native
function/tool execution semantics. Historical tool records are data, and no
historical tool is executed. Existing Google Search request processing and
application-index checks remain in place.

| Input | Serialized-context behavior |
| --- | --- |
| User/model visible text | Preserved in original order |
| System text parts | Encoded as contextual instructions |
| User/model `inlineData` / `inline_data` | Validated and uploaded using the same input rules, with the original role retained |
| Model `functionCall`, `executableCode`, `codeExecutionResult` | Preserved as historical records |
| User `functionResponse` | Preserved as historical records |
| Historical tool names, IDs, arguments and results | Preserved; nested duplicate fields are rejected |
| Model text marked `thought=true` | Removed from upstream context |
| `thoughtSignature` / `thought_signature` | Removed; visible text in the same part remains |
| `fileData`, file links/old file IDs, unknown content kinds | Explicit 400 rejection with a safe field path |

Omitted roles default to `user`; explicit roles must be `user` or `model`.
Truncated history may start with `model`, and at least one user Content is
required. The last user Content becomes `current_input`. Following model
fragments remain ordered in `model_continuation`; a visible model prefix asks
for only the new continuation. Empty or filtered thought-only placeholders keep
normal answer behavior. Attachments receive an ID such
as `c0_p1` and a filename such as `c0000-p0001.png`, derived from their original
Content/part indices. No content-based upload deduplication or cross-request
attachment cache is used.

Web normalizes `image/jpg` to `image/jpeg`. A single UTF-8 or US-ASCII charset
parameter on `text/plain` is accepted when the decoded bytes match the charset.

## Limits, lifecycle and errors

Serialized context accepts any number of history messages within the byte,
structure and memory budgets. Each Content may contain up to 256 user parts or
4,096 model parts by default. Defaults also allow 12 combined user/model
attachments, 50 MiB of decoded attachment bytes, a 68 MiB request body and 4 MiB
of cumulative history text/tool data. Hidden thought/signature data
also consumes the input limit. Tool objects/arrays have bounded fields, depth
and node counts. Serialization and escaping allocations are charged before
encoding; a structurally valid request can still exceed its memory allowance.
These limits are configurable under `input-limits` and fixed in each admission
snapshot. See the [input-limit reference](gemini-enterprise-web-input-limits.md).

Incremental HTTP media detection recognizes attachments at any history position.
Media requests remain on the Web route when a byte or memory limit is exceeded.
The parser reserves space before retaining each message and part.

Generation uses the global concurrency and memory budgets. Requests sharing a
Cookie identity can submit concurrently with the same verified snapshot. Cookie
renewal waits for those submissions before publishing a new snapshot, and
submitted requests release the Cookie snapshot while keeping global reservations
through delivery. The Auth Manager selects credentials using Argon classification,
aliases and exclusions. Authentication and application-search mismatch may use
the existing single permitted fallback before session creation. Fixed credentials
and model tests do not switch. Session creation, uploads and generation are never
replayed on another account.

Classification and credential identity are rechecked before subsequent uploads
and generation. Manual Cookie replacement makes a not-yet-submitted request
fail with 409. Verified automatic Cookie rotation remains usable by the same
request. Submitted work can finish; its evidence cannot update a newer
credential revision. User payload rules run once on the final business payload,
followed only by binding validation and transport framing.

| Rejection | HTTP |
| --- | --- |
| Multi-turn media with `single-turn` | 409 |
| Malformed or unsupported history | 400 |
| Body bytes, history text/structure or attachment limit | 413 |
| Budget/concurrency unavailable | 429 |
| Channel disabled or no eligible credential | 503 |
| Credential replaced or classification changed before submission | 409 |

The existing status endpoint exposes `history_mode`, `supported_history_modes`,
`history_count_policy: unlimited`, `supports_model_history_inline_data: true`
and `preflight_rejections`. Counters use a bounded set of fixed reason codes and
are per process. Reading status performs no Google call. Pre-session rejection
counts are separate from submitted model failures and token usage. Unknown token
counts remain unknown; local text-only subset estimates are marked separately.
Read-time media limit errors retain the same safe reason/field-path contract as
executor rejections. Debug stage logs contain only safe request/model IDs,
counts, byte totals, fixed stages/reasons and thought/signature filtering counts.
For attachment validation failures, the preflight log includes a fixed
`attachment_reason` and the structural field path.

## Execution diagnostics

For submitted model requests, existing usage records may contain a Web
`observation`. It separates the Google HTTP status, verified answer result,
and the client HTTP status and delivery result. For example, both HTTP statuses
can be 200 while `upstream_result` is `incomplete` and `error.code` is
`stream_truncated_json`. When the successful answer has already been verified,
a client write failure changes only the delivery result.

Before response headers are sent, a failure can still use its own HTTP status.
After the stream starts, the handler sends a single safe SSE error if the
connection can be written. The successful terminal is emitted only after the
parser verifies the full upstream response. The `Web request failed` warning
includes the request trace ID, stage, statuses, safe reason and timing without
requiring debug or full request logging. Timings cover the Web executor from
validation through client delivery. First byte, summary and answer timings are
relative to the generation submission. Search for the same trace ID in the
request details and the log. Old usage records show that details were not
recorded.

## Frontend correspondence

The configuration panel supports selection, validation, search and YAML round
trips. Saving an unrelated field preserves the Web settings and does not require
a Web status query. Saving history settings checks backend capability first and
confirms the actual runtime afterward. A failed or unconfirmed save retains the
editable draft through the existing recovery path. Connection revisions fence
late responses, including logout/connection changes.

Older backends without the new status fields keep their existing Web controls;
the history selector is disabled with an unsupported-version explanation. No v0
fallback was added. Labels, help, runtime mode and rejection reasons are present
in English, Simplified/Traditional Chinese, Korean, Russian and Vietnamese.
The selector uses the existing labeled, keyboard-accessible form component.

## Validation boundaries

The automated suite uses synthetic credentials and loopback/mock upstreams.
Two independent HTTP nodes reupload complete historical input without session
sharing, in streaming and non-streaming modes. Tests inspect the serialized
visible history, tool records, attachment names and uploaded bytes; they do not
establish real Google model comprehension.

Real model acceptance of the new code, independent image-content recognition,
and browser visual testing are pending. The work remained API-only. Windows has
`CGO_ENABLED=0` and no GCC on PATH, so Go's race detector was not run; lifecycle
tests use explicit channels and controllable state instead of timer sleeps.
