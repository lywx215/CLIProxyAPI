# Enterprise OAuth import, v8 protocol 1

This protocol installs a verified personal Gemini Enterprise OAuth grant without
discovering applications, allocating licenses, or changing model policy. It uses
normal v8 management authentication. The initial implementation supports the
conditional atomic FileTokenStore; other stores advertise `import_supported=false`.

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

Receipts are embedded in the atomically persisted credential. Non-secret intent
journals use `.receipt` under `.enterprise-oauth-imports/`, so credential JSON scans
cannot load journals as auth files. Per-principal OS locks persist an owner marker;
an unresolved prior import stays isolated even after process termination. Separate
`.write-lock` files serialize credential persistence across processes. Lock files
are stable and must not be unlinked while the service is running.

The legacy generic upload, Vertex import and `/v0/management` routes are unchanged.
Manager must not fall back to them when the dedicated capability is missing.

## Verification

Tests use only synthetic OAuth responses. They cover duplicate requests, exact
target/revision rejection, identity/scope/client failures, graph and disabled-state
preservation, lost persistence acknowledgments, process restart, unknown refresh
isolation and cross-process file locking. Production deployment and Google OAuth
acceptance require separately scoped execution.
