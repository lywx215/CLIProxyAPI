# Legacy Gemini Enterprise Carbon management APIs

The current management UI uses [Hydrogen application admission](gemini-enterprise-hydrogen-admission.md). Carbon business routing follows the shared Argon/Carbon group policy documented in [Enterprise model policies](enterprise-model-policy.md). The historical API contract below remains available for older clients. Its switch may supplement a missing Carbon catalog entry, while the effective group policy determines business admission. Existing exclusions and subscription checks apply according to that policy.

The sections below describe the retained legacy configuration, test and switch interfaces.

This feature applies to the OAuth `gemini-enterprise` provider. It does not
change `gemini-enterprise-web` or Argon subscription classification. No Google
Engine or Assistant settings are changed by the model test or the local switch.
The separate credential-level **Configure Google Carbon** action explicitly
updates Google's model configuration for imported applications.

## Application boundaries

Each `enterprise_account.collections[]` application has its own
`settings.carbon_enabled` boolean. Missing values mean `false`; non-booleans are
rejected. Missing fields and new imports default to closed. Existing explicit
switches are retained. Model-test history never gates a manually opened application. Argon-only and
exclude-Argon templates apply to Argon independently of Carbon.

Carbon requires all of:

- An enabled credential and application with a stable OAuth refresh authorization.
- A verified assigned active subscription, including the existing bounded stale
  subscription policy. Paid/trial information remains visible, but the trial flag
  alone does not deny or grant Carbon. Unknown or unassigned subscriptions remain
  blocked; no other model's subscription rules are relaxed.
- Any explicit custom whitelist and credential/provider exclusions allowing
  `gemini-4-carbon`.
- The human opening that application's separate Carbon switch.

Model tests are optional diagnostics. Missing, failed, incomplete, stale or
needs-review records do not block manual enablement or business calls. The
`verification_required: false` capability tells management clients to honor
`can_enable` independently from test history; older backends retain their prior
validation behavior. Model list additions from manual admission are labeled
`catalog_source: manual`, not claimed as upstream-listed or verified.

Existing quota pools, concurrency, priority and selection rules remain in force.
There is no fallback to Argon, Auto, another provider, or a different application
when Carbon has no eligible application; ordinary requests return `503`.
Aliases, prefixes and thinking suffixes resolve to the real upstream model before
admission. Media input retains the existing Web routing contract.

## Management workflow

The management frontend's account applications and model modal show Carbon
availability separately from Argon and the ordinary model catalog. Two workflows
share the same optional diagnostic tests and application switches:

1. **Select credential → configure Google Carbon → open an application's model
   list → manually enable that application. Testing Carbon is optional.**
2. **Select credential → configure Google Carbon → batch-test Carbon → inspect
   per-application diagnostic results and manually enable eligible applications.**

Testing alone never enables an application. There is no automatic or bulk-enable
button. The account card summarizes the number of enabled and currently eligible
Carbon applications.

## Configure Google Carbon

The credential card exposes **Configure Google Carbon** separately from **Batch
test Carbon**. Its help text states that it changes remote Google application
settings. The configuration action never generates model content, changes a local
business switch, or counts as successful model-call evidence.

`POST /v8/management/credentials/gemini-enterprise/carbon/configure-google` uses
the existing management authentication. Each request contains the selected root
credential's `name`, `auth_index`, `account_id`, root `expected_revision`, and up to
five `applications`. Each application carries its `binding_id`, application
`auth_index`, application `expected_revision`, and exact `expected` resources.

The first request freezes the currently imported applications and returns an
opaque `snapshot_id`; subsequent batches send it unchanged. The bounded in-memory
snapshot expires after 15 minutes and checks authorization, revisions, resources
and relevant connection configuration. It contains no token in its response and
is not a persisted task or a new scheduler. Applications imported later cannot
join an old operation. Trusted token refresh is tolerated; replacement or explicit
reauthorization invalidates the operation even if principal or refresh text stays
the same. A credential cannot run duplicate configuration operations, and at most
two foreground configuration requests run across the handler.

For each application the backend reads its Assistant and Engine, then changes
only what is missing:

```text
Engine.modelConfigs["gemini-4-carbon"] = MODEL_ENABLED
Assistant.generationConfig.allowedModelIds += "gemini-4-carbon"
```

Other model overrides, the default model, tools and search settings are preserved.
Empty or omitted `allowedModelIds` does not provide a reliably enumerable original
effective range. The backend leaves that Assistant configuration unchanged and
configures only the Engine override. It returns `verification_required` with
`assistant_defaults_preserved`; it does not claim that the Assistant now allows
Carbon. A separate fixed-model generation test must establish actual access
for that diagnostic test to be marked verified. Malformed fields still fail before
any write. An implicit range is never replaced with a Carbon-only list or a
snapshot of the currently visible catalog. Shared
resources are deduplicated within a request and Engine writes are serialized.
Already configured resources need only reads. This conservative treatment follows
the optional list in Google's [Assistant schema](https://docs.cloud.google.com/generative-ai-app-builder/docs/reference/rest/v1/projects.locations.collections.engines.assistants#GenerationConfig);
the schema does not enumerate the effective range of an omitted list.

Because the investigated resource schemas do not expose a reliable conditional
update mechanism, each write first rereads the resource and checks its complete
fingerprint. The backend then reads back the result and verifies that only Carbon
changed and that the original allowed model set remains. A detected conflict
stops that application. An uncertain PATCH is never replayed automatically; a
single read may confirm it, otherwise the result stays unknown. These checks do
not promise atomic exclusion of independent remote administrators without a
Google-supported conditional update. See the investigated [Engine schema](https://docs.cloud.google.com/generative-ai-app-builder/docs/reference/rest/v1alpha/projects.locations.collections.engines)
and its [model-state enum](https://docs.cloud.google.com/generative-ai-app-builder/docs/reference/rest/v1beta/projects.locations.collections.engines#ModelState).

Responses contain `applications[]` with `configured`, `unchanged`,
`verification_required`, `failed`, or `changed`, a fixed safe reason, the
binding/index/revision, and an optional HTTP
status. The frontend shows simple batch progress and per-application results.
Closing the page leaves any confirmed Google writes in place; another explicit
click rereads actual state and skips completed configuration. Managing remote
settings does not silently enable a disabled local credential or application.

## Batch model tests

The credential-level batch button tests only native Carbon on the credential's
current imported applications. At most two short tests run concurrently, through
the same fixed-application test API and proof persistence used by the original
model modal. It never configures Google, substitutes another model, or replays an
uncertain generation. Existing credential/application enablement, exclusions,
quota, concurrency and cancellation rules still apply.

The result modal distinguishes successful, failed and untested applications and
displays historical diagnostic results separately from business availability.
An eligible application exposes its manual switch using the current binding and
revision, regardless of test history. There is no bulk
enable action. Cancellation stops new tests, and connection, authorization or
binding changes isolate late results. Missing or stale imported application data
is shown separately rather than contaminating a valid configuration batch.

The original application model modal remains available for single tests and
manual switches. Both routes share one backend admission policy.

`GET /v8/management/credentials/models` returns normal registered `models` and a
separate native Carbon `test_candidates` entry. A candidate is not a business
model, token usage, or proof of upstream access. Admission summaries include:

```json
{
  "carbon_admission_supported": true,
  "carbon_admission": {
    "enabled": false,
    "available": false,
    "can_test": true,
    "can_enable": true,
    "verification_required": false,
    "reason": "carbon_disabled"
  }
}
```

The same summaries appear per application in credential responses and under
`model_policy_state`. They are safe observations, not writable settings. Older
backends without support fields are shown as unsupported by the frontend.

`POST /v8/management/credentials/models/test` accepts the existing `name`,
`auth_index`, `binding_id`, `expected_revision`, `expected` resources and native
`model: "gemini-4-carbon"`. It requires management authentication. Internally it
pins one application and bypasses only manual closure and missing business
registration. Subscription, exclusion, quota and concurrency restrictions remain.
It never switches credentials or substitutes a model.

The server generates an independent random verification code. A successful test
requires a complete matching visible answer and a read of the same saved
session's `queryConfig["google.discoveryengine.googleapis.com.Assistant.model_id"]`
equal to `gemini-4-carbon`. A missing session, missing model, mismatch, partial
response or changed final model cannot produce verified evidence. This extra
Google session read occurs only for this explicit management test. Normal
business and Argon calls gain no extra network requests.

User payload rules remain the final semantic barrier and run once. A rule that
changes the fixed model or binding produces a conflict rather than successful
Carbon evidence. A transient generation or proof-read failure preserves an
existing valid proof; a genuine mismatch or incomplete response invalidates it.
Google `MODEL_NOT_ENABLED` marks that application's evidence `needs_review`
without disabling the credential or changing the human's switch.

Open or close Carbon through the existing v8 collection PATCH endpoint:

```json
{
  "name": "account.json",
  "auth_index": "selected-index",
  "binding_id": "selected-binding",
  "expected_revision": 7,
  "fields": { "carbon_enabled": true }
}
```

This dedicated change cannot be mixed with unrelated fields. Opening requires
ordinary credential, assigned-subscription and explicit policy eligibility. No
model test or test ID is required; legacy test IDs are accepted and ignored.
Closing remains possible without a subscription or test. Explicit `false` is
retained. Testing writes server evidence without advancing the collection's edit
revision; toggling advances that revision through existing CAS persistence.
The frontend only changes its switch after the server confirms success. Failed
saves preserve the draft; cancellation, credential edits, logout and connection
changes fence late asynchronous results. Batch tests run the native Carbon
candidate once at the end and never batch-enable applications.

## Persistence and lifecycle

Evidence is server-owned metadata, stored separately from application settings.
Management responses expose only a safe test summary: test ID, status, time,
observed model, HTTP status and fixed reason. They do not expose fingerprints,
prompts, responses, verification codes or OAuth material.

Account and collection revisions, authorization/resource/configuration
fingerprints, runtime epochs, and a current test lease prevent late observations
from overwriting edited or replaced credentials. Evidence survives restart,
normal access-token refresh, trusted refresh-token rotation, notes, priority and
close/reopen. Same-account reauthorization clears diagnostic proof while retaining manual
intent; a new account or application binding defaults to closed. Proxy, header
or Carbon payload-rule changes can make a historical diagnostic record stale,
but never revoke manual business admission.

HTTP credential imports strip supplied proof and reset supplied Carbon switches
to false before physical persistence. Generic edits and collection creation
cannot inject server evidence or enable Carbon. Trusted Store reloads preserve
legitimate server evidence. A single service instance is responsible for each
credential store's mutations under the existing Store CAS rules.

Closing an application blocks new not-yet-submitted Carbon work. Already
submitted work completes normally. A fast close/reopen cannot revive a request
from the old admission window. Unrelated Argon admission epochs, quota balances
and in-flight counters are not reset.

The frontend implements the flow in all six supported languages, with keyboard
labels, separate pending/error states and cancellation. This API-only development
run does not replace browser visual or keyboard interaction acceptance.
