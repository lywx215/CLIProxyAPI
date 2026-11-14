# Initialize Gemini Enterprise trial applications

This dependency-free Python 3.10+ command reuses an existing **Gemini Enterprise
OAuth account JSON**. It discovers active Google projects and covers `global`,
`us`, `eu`, `ca`, `au`, and `in`, or a caller-selected subset. It contains no
deployment host, account email, project number, or saved credential reference.

The default is a read-only plan. `--apply` explicitly enables changes:

```bash
# Inspect the plan for an existing account. Run where the credential is stored.
python scripts/gemini-enterprise-init/initialize.py \
  --auth-file /protected/auths/gemini-enterprise-account-example.json \
  --email user@example.com \
  --report /tmp/gemini-enterprise-init-plan.json

# Initialize only the selected project; repeat --project for additional projects.
python scripts/gemini-enterprise-init/initialize.py \
  --auth-file /protected/auths/gemini-enterprise-account-example.json \
  --email user@example.com \
  --project 123456789012 \
  --apply \
  --report /tmp/gemini-enterprise-init-result.json
```

`--location eu --location ca` limits the regions. `--project` accepts a project ID
or canonical number. Without `--project`, the read-only plan discovers all visible
active projects. Apply requires one or more explicit `--project` selectors, or
`--all-projects` to deliberately include all visible active projects. Visibility
does not prove Enterprise API access: inaccessible projects are reported as
failures, without enabling services or treating their resources as empty. Review
the plan and use the scope authorized for the task.

The tool reads a flat CLIProxyAPI OAuth account file containing `type`, `email`,
and `access_token`. The token is reloaded for each request, allowing an existing
CLIProxyAPI process to refresh the file normally. The tool does not rotate tokens,
copy account files, request a password, or perform OAuth login. If authorization
expires, refresh through the existing credential workflow, then rerun. Never pass
token values in command-line arguments or put credentials or reports in Git.

## Behavior

For each project and region, sequentially:

1. Fully paginate the license configs, current user's license assignments,
   collections, and applications.
2. Skip a region with **any non-trial license config**, including mixed paid/trial
   regions. An omitted `freeTrial` means false in protobuf JSON. An unreadable or
   malformed response fails that region; it is never treated as empty.
3. Reuse an active trial. If no trial exists, create `free_trial_gemini` with 50
   trial seats, a one-month term, `freeTrial=true`, and `autoRenew=false`. Use the
   current UTC date and require an ACTIVE readback; never fall back to paid
   licensing or work around an expired trial using another ID.
4. Reuse existing applications. If none exist, create one Gemini Enterprise
   intranet application under `default_collection` with a stable regional ID.
5. Assign exactly the verified OAuth user to the selected trial, retaining other
   users. Check the operation and the actual ASSIGNED user-license readback.
6. Read the application's ADMIN model view. Merge all returned model IDs plus
   `gemini-4-carbon` into the Engine's model overrides as `MODEL_ENABLED`. Use the
   Google Cloud application page to manage the model selector.
7. Set the Assistant's `webGroundingType` to Google Search and
   `defaultWebGroundingToggleOff=false`. Extend a nonempty explicit allowed-model
   list while preserving existing entries; keep an implicit list implicit.
8. Read back the Engine, Assistant, and end-user widget. Verify target settings
   and preserve unrelated fields, including the default model and instructions.
9. Explicitly run the remote Carbon configuration stage, using the project's
   `ConfigureCarbonGoogle` semantics. Verify the Engine override and, when the
   Assistant has a nonempty explicit model allowlist, append Carbon while keeping
   its original range. Implicit ranges stay implicit and are reported as
   `verification_required` / `assistant_defaults_preserved`.
10. With `--enable-proxy-carbon`, separately enable the imported application's
    CLIProxyAPI `settings.carbon_enabled` switch. Verify the current Google trial
    and user assignment, the exact Engine/Assistant Carbon configuration, the
    imported binding and revision, and the server's admission eligibility before
    saving. Read back both the saved setting and `carbon_admission.enabled`.

Google's Engine override and CLIProxyAPI's application switch are separate
settings. Step 9 alone leaves the management page's manual Carbon switch closed.
The `proxy_carbon` report explicitly records `not_requested` when step 10 was not
selected; do not describe that result as business admission enabled.

**Paid applications are never enabled for Carbon by this tool.** Paid and mixed
paid/trial regions are skipped before configuration and checked again before any
write. Existing paid settings are retained; this does not revoke a preexisting
paid Carbon setting.

To reconcile only remote Carbon on existing trial applications:

```bash
python scripts/gemini-enterprise-init/initialize.py \
  --auth-file /protected/auths/gemini-enterprise-account-example.json \
  --email user@example.com \
  --project 123456789012 \
  --carbon-only --apply \
  --report /tmp/gemini-enterprise-carbon-result.json
```

Without `--apply`, this remains a plan. Carbon-only mode does not create licenses
or applications, assign users, modify other model switches, or change search or
selector settings. It requires an existing active trial assigned to the selected
account. Empty regions are reported as `no_applications`; unverified trial
assignments fail closed. Repeating a completed remote Carbon operation does not
write again. Each application's `carbon_remote` report separates configuration
readback from actual generation verification.

To configure Google Carbon **and** enable its imported trial applications on
CLIProxyAPI, add `--enable-proxy-carbon`. The management key must already be
supplied through the named environment variable by an existing secure mechanism;
never place its value in arguments or a script. Run an HTTP loopback connection
on the server through verified SSH, or use the exact server HTTPS origin:

```bash
python scripts/gemini-enterprise-init/initialize.py \
  --auth-file /protected/auths/gemini-enterprise-account-example.json \
  --email user@example.com \
  --project 123456789012 \
  --carbon-only --enable-proxy-carbon \
  --management-url http://127.0.0.1:8317 \
  --management-key-env CLIPROXY_MANAGEMENT_KEY \
  --apply --report /tmp/gemini-enterprise-carbon-complete.json
```

Omit `--carbon-only` to include this admission step in a full initialization.
Omit `--apply` to plan both stages without writes. The application must already be
imported; missing imports are reported as `proxy_application_not_imported`.
Subscription, policy, or admission failures stop that application's change.
Tests are optional diagnostics on supported servers and are never run by this
command. Existing paid applications remain unchanged in both stages. The helper
is [proxy_carbon.py](proxy_carbon.py); it only permits the dedicated v8 collection
PATCH setting `carbon_enabled=true` and uses per-application revision checks.

Every write rechecks the region's paid status. Resource fingerprints are reread
before PATCH to detect concurrent edits. Google does not expose an atomic
transaction across these operations: avoid concurrent administration of the same
region. Failed or uncertain writes are not automatically replayed. A rerun
rediscovers current state, reuses completed resources, and only patches missing
settings. Inspect the report before rerunning after a transport failure.

The application ID defaults to `gemini-enterprise-<location>-init`; customize the
prefix with `--app-prefix`. Existing applications are never deleted. Use repeated
`--exclude-engine <full-resource-name>` for resources that must remain untouched.
If every existing application is excluded, the region fails instead of creating
a replacement behind the exclusion.

## Scope and verification

The command makes **no generation requests**. Enabled model overrides do not
prove that subscription policy permits every model. Carbon may remain absent
from Google's user catalog even with the override enabled. Google Search settings
do not prove that a generated answer contains real sources. Actual generation
and source-citation tests require separate authorization.

The default performs Google-side initialization. The explicit
`--enable-proxy-carbon` option also enables CLIProxyAPI Carbon admission for
existing imported trial bindings. The tool does not import bindings, change New
API routing, restart a service, alter a paid subscription, or update developer-tool
settings. A server admission result is reported separately from actual generation.

Requests use verified HTTPS on a closed set of Google origins, match regional
hosts to resource locations, reject redirects, and omit credentials and raw
provider error bodies from reports. They use direct networking and no
post-connection deadlines. The account JSON stays in place and is read only.
Run the command over an already verified SSH connection when it is on a server.

The report records per-region actions, resource names, model IDs, request methods,
HTTP statuses, and failures. A failed region or incomplete coverage returns exit
code 1; successful planning/apply, including paid skips, returns 0. HTTP 401 stops
further processing rather than repeatedly using an expired authorization.

## Offline validation

```bash
python -m unittest discover -s scripts/gemini-enterprise-init -p 'test_*.py' -v
python scripts/gemini-enterprise-init/initialize.py --help
```

Tests use synthetic metadata and never access Google. They cover paid protection,
paid-state changes, incomplete reads, idempotency, implicit/explicit model ranges,
unrelated settings, concurrent edits, account replacement, and transport guards.

## Paid-seat allocation batches

Use [allocate_paid.py](allocate_paid.py) with an existing Gemini Enterprise
OAuth file for each credential. Keep the input and plan in the operator's
private workspace. Each `accounts` entry in the JSON manifest describes one
credential. Project values are numeric project IDs represented as strings.
The source license ID is the last segment of its regional license resource.
The optional `server` label identifies the credential in plans and results.

```json
{
  "accounts": [
    {
      "server": "c05",
      "auth_file": "/protected/auths/account-example.json",
      "email": "user@example.com",
      "source": {
        "project": "123456789012",
        "location": "global",
        "license_id": "source-license-id",
        "keep_seats": 1
      },
      "destinations": [
        {"project": "123456789012", "location": "us", "seats": 1},
        {"project": "123456789012", "location": "eu", "seats": 1},
        {"project": "987654321098", "location": "global", "seats": 1},
        {"project": "987654321098", "location": "us", "seats": 1}
      ],
      "retain_trial": [{"project": "987654321098", "location": "eu"}]
    }
  ]
}
```

Generate the read-only plan, review its allocations, and record the human
confirmation against its `approval_id`. Apply checks the exact confirmed
digest and rereads live Google state before writing.

```bash
python scripts/gemini-enterprise-init/allocate_paid.py \
  --manifest /protected/paid-batch.json --workers 8 \
  > /protected/paid-plan.json

# Use the approval_id after the operator confirms these allocations.
python scripts/gemini-enterprise-init/allocate_paid.py \
  --manifest /protected/paid-batch.json --workers 8 \
  --apply-plan /protected/paid-plan.json --approved-digest "$CONFIRMED_APPROVAL_ID" \
  > /protected/paid-result.json
```

Apply enables the required Google APIs recorded as missing in the plan,
reclaims vacant seats, distributes them, and verifies each regional paid
license is ACTIVE and the named account is ASSIGNED. The result includes the
retained trials and subscription balance. Complete Google model availability
through the SOP after the paid assignment readback.

The worker limit applies to independent subscriptions and projects. A shared
project is scheduled in order. Accounts sharing a subscription need one
confirmed plan at a time against its latest balance. Conflict and failed
planning rows remain in the report while valid independent rows can proceed.
Exit code 1 indicates at least one incomplete row. A plan has an approval ID
when at least one row can proceed.

Run a batch from one operator machine. Concurrent processes on that machine
use the same default lock directory or the same explicit `--lock-dir`. An
uncertain write is reconciled against Google. For partial completion, generate
a fresh plan from the live balance and have the operator confirm its remaining
actions.

## Explicit real Carbon verification

After current authorization for actual model calls, use the separate
[verify_carbon.py](verify_carbon.py) command. It does not run as part of ordinary
initialization. The management key is injected through the named environment
variable using the same protected mechanism as above.

```bash
python scripts/gemini-enterprise-init/verify_carbon.py \
  --auth-file /protected/auths/gemini-enterprise-account-example.json \
  --email user@example.com \
  --project 123456789012 \
  --management-url http://127.0.0.1:8317 \
  --management-key-env CLIPROXY_MANAGEMENT_KEY \
  --run-real-calls --max-calls 13 \
  --journal /tmp/authorized-carbon-tests.jsonl \
  --report /tmp/authorized-carbon-tests.json
```

Repeat `--project` to cover the authorized projects. Scope and call limit must
match the current request. Before each sequential test, recheck Google's license
configs and current user's active trial assignment. Skip every paid or mixed
region. Freeze the imported binding and revision and submit native
`gemini-4-carbon` to `/v8/management/credentials/models/test` exactly once per
eligible application, without a model or account fallback.

Success requires the API's complete random-code verification, the same saved
Google session's actual Carbon model, matching resources/index/revision, and the
server-owned `verified` test evidence. HTTP 200 by itself is insufficient. The
report records each application's result, elapsed time, test ID, observed model,
and business-switch state. Testing leaves application settings unchanged.

The journal durably records `started` before submission. Reusing the same journal
does not replay completed, failed, or uncertain submissions. A process interrupted
after `started` yields `unknown_no_retry` until its saved evidence is reviewed;
never create a fresh journal merely to retry an uncertain call. The command does
not automatically repair failed applications or make extra generations. Offline
tests use synthetic results and cover proof matching, paid skips, HTTP 200
failures, and no replay after transport uncertainty.

See Google's [license creation](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1/projects.locations.licenseConfigs/create),
[user assignment](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1/projects.locations.userStores/batchUpdateUserLicenses),
[application creation](https://docs.cloud.google.com/gemini/enterprise/docs/create-app),
and [Assistant configuration](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants)
references for the underlying APIs.
