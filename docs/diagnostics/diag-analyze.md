# Offline diagnostic analysis

`cmd/diag-analyze` reads local JSONL files or mixed exports with an exact `@diag `
prefix. It never starts a server, resolves peers, calls a model, or accesses a
database. It consumes the frozen `ai-proxy-diagnostics/1` / `1.0.0-rc.1` contract.
Its own report is `diag-analyze/1`, **not** a v1 input bundle or producer record.

```powershell
go build -o diag-analyze.exe ./cmd/diag-analyze
.\diag-analyze.exe -input edge=edge.jsonl -input pool=pool.jsonl `
  -trust edge -trust pool -trace 11111111111111111111111111111111
.\diag-analyze.exe -input edge=edge.jsonl -input pool=pool.jsonl `
  -trust edge -trust pool -format json > report.json
```

Paths are used only to open regular files; reports refer to source numbers and
operator-selected aliases. Choose nonsensitive aliases. Do not put credentials
in paths, command arguments, or aliases. Errors contain fixed reason codes and
generated input numbers, never filesystem errors, input text, or flag values.
The tool does not read export bundle files: pass their constituent local record
files explicitly. No trust fields are added to the frozen bundle schema.

Inputs are **untrusted by default**. `-trust ALIAS` explicitly attests that the
operator controls that export. Trust is never derived from a record's service,
instance, peer configuration, caller trust, or process capabilities. Importing an
identical event from both controlled and uncontrolled sources retains both
locations and conservatively keeps that event untrusted. `-known-loss ALIAS`
records operator evidence of export loss. Neither flag authenticates callers.

To look up a caller alias, provide its local scope:

```powershell
.\diag-analyze.exe -input edge=edge.jsonl -trust edge `
  -caller-request-id caller-1 -environment test -deployment edge `
  -service cliproxyapi -caller-scope deployment -caller-alias principal-1
```

For boot aliases use `-caller-scope boot`, `-instance` and `-boot` as well.
Use `-caller-scope unknown` when no authenticated principal alias is available.
Unknown aliases and unassigned deployments widen the candidate search; the
report marks `caller_scope_unknown`. A caller ID never replaces local request,
span, or attempt identity. Results list all matching candidate traces and the
matching node IDs. Trace selection and caller selection are mutually exclusive.

## Evidence and interpretation

The text output includes a call forest, edge findings, per-event source/line
references, separate upstream/delivered usage observations, and quarantine
summaries. JSON retains every accepted payload, all import locations, resource
identities, process declarations, coverage and counts. Records are ordered only
by sequence within their span. Wall-clock skew never repairs or orders edges.

The reader validates all input before applying the query. Same event identity
and structurally equal payloads deduplicate; unequal payloads remain separate
conflict variants. Numeric equality is exact, so `1` and `1.0` compare equally
without rounding distinct fractional values. Reused spans across resources or
traces remain distinct nodes. Conflicting payloads on another trace and process
identity variants from a selected boot remain visible. Nodes included solely
for conflict context have `contextOnly=true` and do not inflate query totals.

Remote edges require unique complete call/server terminals, matching trace and
parent IDs, configured peer service/deployment, matching optional response IDs,
and matching configured inbound request IDs. Both sources must be controlled.
Conflicts, terminal stubs, unknown sources, and incomplete scans cannot yield
verified edges. Missing local owners are reported separately; a complete remote
pair can still be checked without its caller's server terminal. Unrelated graph
gaps do not overwrite a valid independent edge. Cycles are marked and excluded
from the forest. Ambiguous candidate groups are retained once, not expanded into
a cross product. The forest caps indentation at 32 levels and lists remaining
nodes separately with an explicit depth notice.

Request counts use observed server identities; call counts use call identities;
attempt counts use resource/server/retryScope/attemptId. Conflicted totals are
flagged rather than resolved. Observed owner counts include missing server
terminals and are separate from request counts. HTTP error statuses and explicit
local failure/read/transport failure count at their own request/call level;
attempt error/blocked/empty/incomplete results count at the attempt level.
Cancellations and unknown outcomes are not silently classified as failures.
Observed server `callCount` discrepancies are reported without allocating missing
calls. Counts are observed identities, not estimates of unobserved work.

No current producer requires numeric attempt fallback. If a separately verified
retry owner guarantees uniqueness, the operator can attest with
`-unique-attempt-scope SERVICE/SCOPE`. Without it, a numeric attempt alone remains
unknown. gcli's `[1,2,1,2]` attempts with distinct IDs count as four; CPA compaction
calls sharing one business attempt count as one attempt. Current CPA scope is
`conductor_gemini_family`; older `conductor_executor` records are historical.

Coverage uses the frozen precedence and `diagnostics.AssessCoverage` with
additional observed sequence, source and graph evidence. Known gaps, truncation,
interruption and conflicts yield partial. A missing terminal without known loss
is unknown. A terminal stub proves existence but not success or a verified edge.
Late independent call terminals remain visible; they cannot rewrite an earlier
attempt result. A source with quarantined records or a partial scan conservatively
downgrades its observed spans. Sink-wide counters remain boot-level evidence;
they are never substituted for `droppedForSpan`.

Usage is preserved, never recomputed or summed. gcli conversion can expose
candidate-only output; CPA/Aito conversions may include candidate plus reasoning.
`reasoningIncludedInOutput`, source, basis, raw numeric fields, null and observed
zero remain distinct. All usage snapshots are labelled last-observed and not
proven final: v1 does not carry a universal final-usage guarantee. Upstream and
delivered observations can independently be unknown. A value of 87 is not an
error. Missing conversion events on error attempts do not prove the converter
did not run. Process capabilities describe potential instrumentation, not actual
coverage of each route. In particular Aito's HTTP helper does not imply production
browser HTTP, images, VNC, or version-check coverage.

gcli files at least 16,773,120 bytes receive
`suspected_tail_gap_near_producer_limit`, with source size and boot associations.
This is only a clue: it does not assert loss, a stopped sink, or completeness of
smaller files. Full scan means the reader reached the end of its input, not that
the export contains the producer's complete history.

## Bounds and quarantine

Defaults are also hard maxima; command flags/API options may only lower them:

| Bound | Value |
| --- | ---: |
| Sources | 128 |
| Total scanned bytes | 128 MiB |
| Total lines | 250,000 |
| Accepted record occurrences, including repeated imports | 20,000 |
| Stored quarantine details | 1,000 |
| Record bytes, including prefix and newline if present | 4,096 |
| Scanner buffer / retained line prefix | 8,192 / 4,096 bytes |
| JSON nesting depth | 64 |
| Numeric spelling / exponent magnitude | 64 bytes / 308 |

An overlong line is drained in fixed chunks until its newline or the global
byte limit; scanning can then resume. Limited scans remain explicitly limited,
including a partial line's incomplete length. Reaching a limit exactly can
conservatively leave EOF unproven. Quarantine-detail truncation has its own
omitted count; source quarantine totals remain exact for scanned lines. Sequence
ranges use observed cardinality and comparisons, never `1..expectedLastLogSeq`
allocation or iteration. Graph endpoints and conflict context are bounded by
observed nodes, including adversarial repeated identities.

Unknown versions, malformed JSON, duplicate keys, invalid UTF-8, closed-schema
violations and conditional/cross-field violations are quarantined. Bad records
are never projected into trusted v1 records. Unstructured legacy lines are
counted and omitted, not exported. Schema validation does not prove that an
ID-shaped value is nonsensitive; producers/operators remain responsible for safe
field origins. Only schema-valid payloads can appear in evidence.

Exit 0 means the scan completed; it does not mean a complete or verified trace.
Exit 1 means quarantine or an incomplete/limited scan, with a usable report.
Exit 2 means invalid configuration, an unreadable/nonregular input, or output
failure. No original paths or raw exceptions are included in these diagnostics.

## Reproduction

```powershell
go test -timeout 120s ./internal/diagnosticanalyzer ./cmd/diag-analyze
go test -timeout 10m ./...
```

The package tests consume all frozen offline graph, source-scope, count,
coverage, semantic and record-schema cases. The separate frozen Python oracle
covers all 242 artifact vectors (including producer-only header/peer/mapping
vectors); oracle success is not a claim that this reader implements producers.
Tests also execute the built command and check error redaction, source trust,
quarantine exits, real producer fixtures, cross-trace conflicts, delayed owner
conflicts, cycles, bounded scans, giant counters and linear ambiguity groups.
Fixture origins and exact hashes are in
`internal/diagnosticanalyzer/testdata/sources.json`. They are actual producer
outputs for synthetic requests, not captures of production traffic. Cross-service
live multi-instance integration remains DIAG-07 work.
