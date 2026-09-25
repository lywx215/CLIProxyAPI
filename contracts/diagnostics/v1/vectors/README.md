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
| headers.json | Ordered framework-parsed `[name,value]` pairs with multiplicity preserved, authenticated flag, configuredInboundAdapter flag => extracted context/caller summary. `boundary=parser_unit` labels synthetic inputs that are not claims about framework HTTP parsing |
| http-ingress.json | Wire header lines plus expected framework fields => actual in-memory h11 parsing must match those fields, then extraction must match expected. No sockets, service startup or raw-socket interception |
| copy-source.json | Proven automatic inbound-copy source fields => filtered fields; never apply this oracle to arbitrary explicit provider extra_headers |
| outbound.json | Final request headers after source filtering, actual target permission, local IDs/context, response flag, and diagnostics module's own local marker => `{headers,diagnosticOwned}`. No inferred provider ownership input |
| redirects.json | Actual target URLs plus peer config and successive calls => preserve and clean the module's own marker across redirect copies, before new provider writes; verify each policy result and hop output |
| peer-response.json | Framework-parsed peer response fields and peerConfigured => individually validated peer IDs and first rejection reason; unconfigured targets always yield null/none |
| peers.json | Parsed array or raw DIAG_PEERS JSON string and final normalized destination/request-target => configuration status and matching alias/null. Pre-normalization dot segments are not observable evidence |
| source-scope.json | Two caller identity projections => same lookup scope, alias candidate match, scope-unknown and merge=false. Same lookup scope is never proof of a call |
| resources.json | Local configuration/platform identity plus ignored request claims => resource defaults, identity-source precedence and new-per-worker UUID placeholders. Runtime tests must actually generate distinct UUIDs after fork/restart |
| counts.json | Minimal observed call ownership/attempt projections => distinct observed server owners, calls, known attempts and calls with unknown attempts. The two-outer/six-inner case has eight calls in total, with six at the inner service; this does not infer missing server terminal counts |
| graph.json | Minimal terminal projections from controlled/uncontrolled exports => sorted findings, verified span pairs, distinct event count. This is a single-edge evidence oracle, not the DIAG-06 analyzer |
| coverage.json | Observed sequences, declared last sequence, full terminalCount/terminalLogSeq and separate terminalStubCount, lifetime captures, losses/truncation/conflict/export loss => DEBUG coverage and terminalMissing. A terminal stub proves existence but yields partial |
| aito-mapping.json | Untouched legacy fields, independently observed local finish/cancel/failure, new logSeq and explicitly new monotonicMeasurements => public projection. Wall-clock/browser values are never copied into timing |
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

HTTP field limits measure only framework-observable values after protocol OWS
handling. `X-Request-Id:  a ` normally yields `a`; a parser-unit input ` a ` tests
the application's no-extra-trim rule, not the same wire request. HTTP may reject
control bytes before diagnostics runs. Runtime adapters use per-field collections
(Node rawHeaders/headersDistinct, including tracestate), never merged req.headers.
The h11 fixtures prove their Python parser boundary only; Go/Node implementations
must exercise the same wire cases through their real framework in DIAG-03/04.

The outgoing ownership flag is set by the diagnostics injection itself and carried
only in local request metadata through redirects. It is never an external header
or a value-based provenance guess. Source-filter cases require an actual automatic
copy site; a shared API merely accepting explicit extra_headers is not such proof.
Marker continuity and final URL selection still need runtime integration tests.

The redirect oracle models consecutive hops that actually carry this module's
injected headers forward; it does not require all clients to copy headers the
same way. Apply each hop to the headers actually reaching its final boundary.
Go http.Client builds redirect headers from the initial request (`ireq.Header`),
not from a RoundTripper's modified request copy. The RoundTripper must clone
before injecting and must never mutate the incoming request/ireq to mimic these
vectors. If the actual next hop already has no module-injected headers, its
ownership marker may be empty. Final non-leakage and per-hop target policy remain
mandatory. On paths that really inherit injected headers (such as the reviewed
httpx path), the existing ownership/cleanup requirements remain unchanged.

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
