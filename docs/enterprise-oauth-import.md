# Enterprise OAuth import, v8 protocol 1

This protocol installs a verified personal Gemini Enterprise OAuth grant without
discovering applications, allocating licenses, or changing model policy. It uses
normal v8 management authentication. FileTokenStore supports conditional atomic
imports. The S3-backed ObjectTokenStore can also support them after its endpoint
and writers are verified. Other stores advertise `import_supported=false`.

## Capabilities

`GET /v8/management/credentials/gemini-enterprise/oauth/capabilities`

Returns `protocol_version=1`, `provider=gemini-enterprise`, configured `client_id`,
`client_secret_ready`, `required_scopes`, `import_supported`, `initially_enabled`.
Application secrets are resolved from the server configuration's environment
reference. No secret or environment-variable value is returned.

## Import

`POST /v8/management/credentials/gemini-enterprise/oauth/import`

The JSON request contains `request_id`, `mode` (`create` or `reauthorize`), verified
`email`, Google `subject`, `client_id`, `refresh_token`,
`expected_authorization_revision`, and `target` (`name`, `account_id`, `auth_index`).
Creation requires revision zero and an empty target. Reauthorization requires one
exact existing parent account and its current stable OAuth authorization revision.
Only the configured OAuth client is permitted. Unknown fields are rejected.

The server refreshes the supplied offline grant once, then validates Google
userinfo, token client and all required scopes. It atomically saves the grant and
a non-secret receipt using the existing account reauthorization path. Concurrent
graph/settings changes are rebased, not overwritten. Token refresh does not change
the stable authorization revision. Manual reauthorization does.

Token introspection follows Google's official [OAuth client implementation](https://github.com/googleapis/google-auth-library-nodejs/blob/main/src/auth/oauth2client.ts):
POST to `oauth2.googleapis.com/tokeninfo` with a bearer header, without token query
parameters. Manager uses the same endpoint and identity fields.

The result is one of `saved`, `failed`, `reconcile_required`. `saved` includes the
exact account identifiers, `authorization_revision`, email/subject/client and
`runtime_synced`. A false runtime flag requires separate runtime synchronization;
it does not mean the persistent grant was absent. Import creates no Engine even if
legacy project/Engine hints are present in server configuration.

## Reconcile and concurrency

`GET /v8/management/credentials/gemini-enterprise/oauth/imports/{request_id}`

Request IDs use 16–100 ASCII letters, digits, underscores or hyphens. Reusing an ID
with the same request returns its receipt without another refresh. A different
request with that ID conflicts. Missing or unknown receipts never authorize replay.
The receipt lookup is read-only. Its runtime flag also checks that the exact saved
authorization is current and present in this process.

`POST /v8/management/credentials/gemini-enterprise/oauth/imports/{request_id}/runtime-sync`

Send an empty body to publish the original revision from the durable store and
return the same receipt schema. Repeating publication for the same ready
authorization is idempotent. An absent or superseded grant prevents publication.
Publication preserves the current graph, account settings, and disabled state.

Receipts are embedded in the atomically persisted credential. FileTokenStore
uses separate `.receipt` journals under `.enterprise-oauth-imports/`. Its
per-principal OS locks persist an owner marker across process termination.
Separate `.write-lock` files serialize credential persistence across processes
and need to remain present while the service is running.

Manager uses the dedicated v8 route and fails closed when its capability is missing.

## S3 object storage

`OBJECTSTORE_ENTERPRISE_OAUTH_IMPORT_CAS=true` enables this path. The default is
disabled. Enable it only after testing conditional behavior on the same endpoint
with isolated synthetic objects and upgrading every writer of the auth prefix.
Existing credential object keys are reused.

Request journals and per-account owners are stored outside `auths/` and contain
no OAuth material. Owners are replaced by conditional writes only after the prior
request is conclusively resolved. Both new credentials and updates use conditional
PUTs with SDK retries disabled. The successful receipt is embedded in the same
object as the authorization. Intent updates are advisory once that object commits.

Receipt queries read the exact S3 object. A confirmed save or explicit runtime
recovery can update the local mirror. Runtime synchronization checks the
authoritative object before and after publication and reports readiness for the
responding instance.

With this mode enabled, generic file-based upload, deletion and mirroring require
version protection for Gemini Enterprise credentials. Managed settings and token
refresh use the version of the snapshot on which the edit was based.

## Verification

Tests use only synthetic OAuth responses. They cover duplicate requests, exact
target/revision rejection, identity/scope/client failures, graph and disabled-state
preservation, lost persistence acknowledgments, process restart, unknown refresh
isolation and cross-process file locking. Production deployment and Google OAuth
acceptance require separately scoped execution.
