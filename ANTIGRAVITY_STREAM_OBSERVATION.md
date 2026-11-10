# Antigravity execution observations

This implementation shares response framing, raw Gemini observation, stream settlement, and failure facts between `antigravity` and `antigravity-enterprise`. Authentication, subscription metadata, quota, request normalization, thinking translation, payload configuration, and reasoning replay remain provider-owned. It introduces no automatic continuation request.

## Completion contract

| Channel | Semantic completion while a connection stays open | Clean EOF compatibility |
| --- | --- | --- |
| Antigravity | All requested candidate indices have legal finish reasons and a valid, associated usage event was observed in the same or a later frame; a prompt block also completes. | All candidates finished; alternatively at least one complete tool call with official usage, one candidate, no answer/media, and no parsing fragment may infer `compat_tool_eof`. |
| Antigravity Enterprise | All requested candidate indices have legal finish reasons, or a prompt block was observed. | Requires the same legal completion evidence; no tool-only EOF inference. |

A complete read batch is processed before stopping. A subsequent error already present in that batch takes precedence. SSE and JSON transcoding follow this rule. Partial buffered tails are not flushed on local semantic completion. A locally stopped read records `tail_verification=unverified`; only an actual EOF verifies trailers. Nonzero gRPC status fails tail verification. No timer is introduced after an upstream connection is established.

Empty EOF and ordinary text without a finish are incomplete. A missing thought summary, a missing answer on a legally finished response, and `MAX_TOKENS` are not transport failures. Candidate indices are tracked independently. Native Gemini preserves `CONTINUATION`; a translation that cannot represent it fails locally without starting a second request. An unknown finish reason is retained as bounded evidence and fails validation. Native Gemini does not gain a synthetic `[DONE]` frame.

Reasoning replay is committed after response and translation validation. Inferred compatibility endings do not become official Google finish reasons. Existing diagnostics distinguish a completed local read from a verified EOF.

## Observation version 1

The optional `observation` object belongs to one model HTTP attempt, identified by the existing execution UUID and parent trace ID. `attempt_no` is shared across executions in the inbound request context. Metadata and quota reads do not consume this counter. This object enriches the existing usage record; it creates no extra accounting record.

It records the requested model, final outbound model and thinking settings, catalog canonical model, bounded raw `modelVersion` sequence, first response-body byte, first nonempty thought summary, first nonempty answer, and first valid function call. All times are milliseconds from the model transport dispatch, measured before protocol translation. They include proxy read scheduling, transport waiting, and possible downstream backpressure. They differ from end-to-end latency and the historical TTFT field. Whitespace and heartbeats do not create semantic first-output times.

Model comparison is `exact`, `mapped`, `different`, or `not_reported`. Catalog family equality is a declared mapping, not proof of an internal Google model tier or private routing decision. Response-model sequences are limited to eight distinct values, with an explicit truncation flag.

Upstream result, completion basis, tail verification, downstream delivery, and usage provenance are independent. A local translation or downstream-write failure does not overwrite an already observed Google terminal result. The legacy `failed` flag projects the frozen final outcome.

Official usage is cumulative and merged per field. Explicit zeros survive; omitted fields remain unavailable and are not synthesized into official totals. Input includes `toolUsePromptTokenCount` when provided. A fully verified observation with reported input, output, reasoning and total is `official_complete`; other observed official usage is `official_partial`. Client-compatible translated usage may retain its existing shape independently of this provenance.

Historical rows omit `observation` and display “not recorded.” Null values in a versioned object mean “not observed.” Zero is an observed value. The object contains no prompts, answers, thought text, signatures, credentials, or upstream error bodies.

## Delivery and retry boundaries

Only the two observed producers activate the passive HTTP delivery tracker. Gemini native, OpenAI Chat/Completions/Responses, and Claude HTTP handlers capture the initial, terminal, heartbeat, short-write and flush failure paths. The producer closes its output channel before awaiting the HTTP acknowledgment. Accounting settles before the handler returns, independently of potentially blocking transport cleanup. A write/flush acknowledgment is evidence of local delivery, not evidence the remote user consumed the bytes. Internal SDK consumers have no outer HTTP acknowledgment.

Failure facts preserve error causes, HTTP status and retry hints while exposing a bounded safe projection. A confirmed dial/DNS failure can be replayed. Possibly sent transport failures and responses that generated output cannot be replayed, including through configured continue rules, auth refresh and bootstrap retries. Explicit pre-generation rejection retains its request/model/binding/shared-pool scope. A rejected attempt retired before any output does not inherit the next attempt’s HTTP delivery result. Existing subscription-wide quota handling remains separate.

## Persistence and UI compatibility

Observation data is cloned at queue, plugin, storage, import and snapshot boundaries. Local usage statistics and Redis remain independent sinks; the UI reads the local snapshot and never drains Redis to populate its view. Snapshot imports deduplicate by execution ID within API/model scope. A unique legacy fingerprint can be enriched once without recounting. Conflicting frozen observations, malformed objects, incompatible versions and contradictory legacy outcomes are counted as conflicts while preserving the accounting row. Ambiguous legacy matches are not guessed.

The request-details card displays the raw model, final thinking configuration, timings and independent outcomes in six locales. JSON and CSV exports retain execution IDs, trace IDs and valid observations. The optional versioned object can be ignored by an older frontend, and an updated frontend accepts historical rows without it. Management integration continues to use the v8 observability endpoints.

## Local verification boundary

Regression fixtures exercise split terminal usage, batch-tail errors, JSON arrays, Unicode/multiline SSE, malformed/truncated responses, candidate indices, prompt blocks, sparse/zero usage, tool EOF inference, translation failures, cancellation, write/flush failures, retry barriers, replay validation and import ownership. Real-call evidence must be reported separately from fixtures and must identify which binary was exercised. A healthy old instance or an old real-call matrix cannot validate a new executable. Ordinary Antigravity requires an available local credential for live verification.
