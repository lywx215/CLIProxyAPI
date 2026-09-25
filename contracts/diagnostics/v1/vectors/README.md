# Shared conformance vectors

Each JSON file is an array of `{id,input,expected}` cases. Inputs and outputs are
data, not executable code. Tests must compare their actual implementation output
to the checked-in expected value, not regenerate expected values with the tested
implementation. Object property order is immaterial; arrays preserve order.
`<generated>` means a fresh nonzero random trace ID, different for independent
requests. Runtime tests also assert fresh server/call IDs, concurrency isolation,
and no business response change; this offline oracle cannot prove those effects.

| File | Adapter input/output |
| --- | --- |
| headers.json | Ordered raw `[name,value]` pairs (duplicates preserved), authenticated flag, configuredInboundAdapter flag => extracted context/caller summary. Authentication is supplied by the host app, not inferred from headers |
| outbound.json | Existing raw headers, already evaluated target permission, local IDs/context, response flag or explicit provider-owned trace => lowercase header-to-value-array result. Business header values are preserved; scenario names identify error/stream/redirect paths that runtime tests must exercise |
| peer-response.json | Raw peer response headers => individually validated peer request/trace IDs and first rejection reason (request ID then trace ID); missing values are null, never graph identities |
| peers.json | Parsed array or raw DIAG_PEERS JSON string and actual target URL => configuration status and matching alias/null. Includes shape and semantic constraints |
| source-scope.json | Two caller identity projections => same lookup scope, alias candidate match, scope-unknown and merge=false. Same lookup scope is never proof of a call |
| resources.json | Local configuration/platform identity plus ignored request claims => resource defaults, identity-source precedence and new-per-worker UUID placeholders. Runtime tests must actually generate distinct UUIDs after fork/restart |
| counts.json | Minimal observed call ownership/attempt projections => distinct observed server owners, calls, known attempts and calls with unknown attempts. The two-outer/six-inner case has eight calls in total, with six at the inner service; this does not infer missing server terminal counts |
| graph.json | Minimal terminal projections from controlled/uncontrolled exports => sorted findings, verified span pairs, distinct event count. This is a single-edge evidence oracle, not the DIAG-06 analyzer |
| coverage.json | Observed sequences, declared expected last sequence, terminal count, lifetime captures, per-span losses/truncation, conflict and known export loss => computed DEBUG coverage and terminalMissing |
| aito-mapping.json | Legacy fields, independently observed local finish/cancel/failure, newly allocated logSeq => public projection plus untouched legacy values |
| semantic.json | A schema-valid record => sorted cross-field violation codes, or an empty array. JSON Schema alone cannot compare IDs or counters |

Graph projections require common kind/traceId/spanId/parentSpanId/service/
instanceId/bootId/logSeq/deploymentId/requestId/sourceTrusted fields. Call
projections add peerConfigured/peerService/peerDeploymentId/peerRequestId/
peerTraceId; server projections add callerHeader/callerTrust/callerRequestId.
The sourceTrusted flag is test input from import provenance; it is intentionally
absent from the log record schema. Projections omit timestamps and content.
Each graph case isolates at most one cross-service edge; conflicts conservatively
block that candidate. DIAG-06 must classify independent edges separately, retain
both payloads of conflicting events with provenance, detect missing local owners
and cycles, and bound all processing. The oracle only counts/confirms conflict
existence; it does not store an import database or produce a graph report.

Fixtures under `fixtures/invalid` must fail record.schema.json itself. Valid
fixtures must pass both JSON Schema and the documented cross-field invariants.
Semantic negative vectors pass shape validation before failing their expected
invariant. Header rejection does not mean the model request is rejected.
These inputs are deliberately synthetic, including the FORBIDDEN markers used
to assert strict allowlists; there are no real keys, user content or account IDs.

The full runtime matrix (streaming EOF/Close, response commit, two replicas per
service, workers/restarts, nested retries, proxy equivalence, DEBUG transitions,
bounded sinks and real protocol converters) belongs to DIAG-02 through DIAG-07.
Passing this oracle is a contract-artifact check, not runtime conformance.
