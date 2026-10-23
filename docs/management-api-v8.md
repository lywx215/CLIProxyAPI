# v8 Management API

The base path is `/v8/management`. Configuration endpoints follow the structure in
[config.example.yaml](../config.example.yaml); operational endpoints are grouped
by function. The existing `/v0/management` API remains supported. Its reference is
available in the [management API guide](https://help.router-for.me/management/api).

## Access

The v8 endpoints use the same management key and remote-access policy as v0.
Send `Authorization: Bearer <management-key>` or `X-Management-Key: <management-key>`.
OAuth callbacks validate a pending login state and do not require a management-key
header. Home mode disables local management endpoints for both versions.

## Configuration endpoints

All paths below are relative to `/v8/management`.

| Path | Methods | Description |
| --- | --- | --- |
| `/config` | GET, PUT, PATCH | Read, replace, or merge the complete configuration as JSON. |
| `/config.yaml` | GET, PUT | Read or replace the complete configuration as YAML. |
| `/config/<section>/<field>` | GET, PUT, PATCH, DELETE | Read or modify a section or field using its path in the v8 tree. |

GET renders the persisted configuration in the v8 layout without migrating the
file or adding omitted runtime defaults. Successful v8 configuration writes save
the latest layout; reads and rejected writes leave the file unchanged.
Historical v8 paths and request bodies remain accepted as aliases.

JSON writes accept the value directly, without a `{ "value": ... }` envelope.
PUT replaces its target. PATCH merges objects and replaces lists and scalars.
Scalar field updates retain existing comments; complete replacements use the submitted document.
DELETE removes a field. Paths identify mapping keys, not array indexes; replace
an entire list to change its entries. Optional per-key `null` overrides inherit
the corresponding group value. Legacy field names are rejected by v8 writes.

| Example path | Value or purpose |
| --- | --- |
| `/config/access/api-keys` | Client authentication keys, for example `["client-key"]`. |
| `/config/api-keys` | All upstream provider groups. |
| `/config/api-keys/codex` | Codex upstream groups. |
| `/config/client/codex/optimize-multi-agent-v2` | Boolean, default `false`; applies to Codex clients across OAuth and API-key routes. |
| `/config/observability/logs/debug` | A boolean, for example `true`. |
| `/config/routing/retry/request-retry` | A number, for example `0`. |
| `/config/plugins/configs/<id>` | A plugin configuration object. |

For example, `PATCH /v8/management/config` with this body disables additional
request retries while preserving other settings:

```json
{
  "routing": {
    "retry": {
      "request-retry": 0
    }
  }
}
```

JSON reads omit TURN usernames and credentials under
`oauth.providers.codex.live-media-relay.ice-servers`; YAML reads include them.
The Home-owned revision fields
`credentials.concurrency.lifecycle-config-revision`,
`credentials.concurrency.observation-barrier-revision`, and `plugins.auth-revision`
cannot be changed through these endpoints.

### Codex multi-agent configuration migration

`client.codex.optimize-multi-agent-v2` is the sole runtime setting. Loading older
YAML accepts `providers.codex.optimize-multi-agent-v2`,
`oauth.providers.codex.optimize-multi-agent-v2`, and the flat
`codex.optimize-multi-agent-v2` path. The client path always wins conflicts,
including explicit `false` or `null`. If it is absent, the historical OAuth path
wins over `providers`, which wins over the flat path.

These aliases now have client-wide semantics, not OAuth-only scope. Loading does
not rewrite legacy-only settings; normalization removes aliases conflicting with
the client path. A successful v8 write migrates the aliases while preserving their
comments. Historical API paths remain usable. Home configuration publishers
need matching client schema support before emitting the new path; older YAML
payloads remain readable.

## Operational endpoints

All paths below are relative to `/v8/management`. Request and response bodies
retain the corresponding business operation's fields.

| Path | Methods | Description |
| --- | --- | --- |
| `/config/upstream/<provider>` | GET, PUT, PATCH, DELETE | Manage shared provider settings. |
| `/server/latest-version` | GET | Get latest release information. |
| `/requests/api-call` | POST | Make an authenticated upstream call. |
| `/routing/cooldown/reset` | POST | Clear credential cooldown. |
| `/routing/model-definitions/<channel>` | GET | Get model definitions. |
| `/observability/logs` | GET, DELETE | Read or clear application logs. |
| `/observability/logs/errors` | GET | List error-log files. |
| `/observability/logs/errors/<name>` | GET | Download an error-log file. |
| `/observability/logs/requests/<id>` | GET | Get a request log. |
| `/observability/usage/api-keys` | GET | Get API-key usage. |
| `/observability/usage/queue` | GET | Get queued usage events. |
| `/credentials` | GET, POST, DELETE | List, upload, or delete credential files. |
| `/credentials/models` | GET | Get credential models. |
| `/credentials/download` | GET | Download a credential file. |
| `/credentials/status` | PATCH | Change credential status. |
| `/credentials/fields` | PATCH | Change credential fields. |
| `/credentials/refresh` | POST | Refresh credentials. |
| `/oauth/import?provider=vertex` | POST | Import a Vertex service account using a multipart `file` upload. |
| `/oauth/auth-url?provider=<provider>` | GET | Start built-in or plugin OAuth. |
| `/oauth/status?state=<state>` | GET | Get login status. |
| `/oauth/session?state=<state>` | DELETE | Cancel a login session. |
| `/oauth/callback` | GET, POST | Submit an OAuth callback. |
| `/plugins` | GET | List installed plugins. |
| `/plugins/<id>` | DELETE | Delete a plugin. |
| `/plugins/store` | GET | List the plugin store. |
| `/plugins/store/<id>/install` | POST | Install or update a plugin. |
| `/plugins/<id>/quota` | GET, POST, DELETE | Read, fetch, or reset plugin quota. |

## OAuth

The login URL is shared by all providers. Set the required `provider` query
parameter to `claude`, `codex`, `antigravity`, `kimi`, `kimi-ai`, `xai`, `devin`,
`meta`, `gemini-enterprise`, or a registered plugin provider ID. For example:

```http
GET /v8/management/oauth/auth-url?provider=codex
Authorization: Bearer <management-key>
```

Keep any provider-specific login parameters in the query string. The login
response includes the authorization URL and a session `state`. Use that state
for status queries and cancellation.

Callbacks accept `provider`, `state`, `code`, and `error` as GET query parameters
or POST JSON fields. POST also accepts `redirect_url` containing the complete
callback URL. If `provider` is omitted, it is inferred from the pending state;
an explicit provider must match that state. Poll until the status is `ok` or
`error`; `wait` means the login is still pending. Callback acceptance alone does
not mean credential exchange and persistence have completed.

Import uses `POST /v8/management/oauth/import?provider=vertex`. The `provider`
query parameter is required; Vertex is currently the supported import provider.

### Gemini Enterprise

Without `auto_discover_resources`, login uses the configured
`oauth.providers.gemini-enterprise` binding: `client-id`, optional
`client-secret-env`, `project-id`, `location`, `collection-id`, `engine-id`,
and `assistant-id`. Other client and resource query parameters are ignored in
this legacy mode. Secret values are resolved only from the named backend
environment variable.

`GET /oauth/auth-url?provider=gemini-enterprise&is_webui=true` returns a PKCE
authorization URL, `state`, `callback_url`, `manual_callback_required`, and
`resource_inputs_supported: true`.
Register the exact callback URL `http://localhost:8788/oauth-callback` in Google.
The backend listens on loopback when the port is available; otherwise submit
the complete redirect URL through `POST /oauth/callback`. Status polling and
cancellation use the shared endpoints above. Enterprise login is available only
under the v8 management API and outside Home mode.

The management page passes session-specific public inputs without changing the
global configuration. `auto_discover_resources=true` requires `client_id`,
`engine_id`, optional `client_secret_env`, and optional `assistant_id` (defaults
to `default_assistant`). After Google authorization, the backend searches visible
projects and probes the supplied engine ID using `Engine.Get`, with the candidate
resource project as `X-Goog-User-Project`. It checks known locations under
`default_collection`, saving credentials only after finding one readable match.
It does not use engine/assistant list APIs or infer the assistant ID. Missing,
ambiguous, or incomplete results return a session error without saving credentials.

For manual resource input, pass `auto_discover_resources=false` and include
`project_id`, optional `location` (defaults to `global`), and optional
`collection_id` (defaults to `default_collection`). Both modes keep inputs local
to the OAuth session. Clients must check `resource_inputs_supported` before
offering the returned authorization link. The global binding PATCH endpoint
remains available for default configuration, with the existing `If-Match`/409
contract; the current authorization form does not call it.

`GET /credentials` includes a `gemini_enterprise` summary for these accounts:
public client and resource identifiers, optional `expiry`, and
`has_refresh_token`. It never includes token or client-secret values. Existing
accounts retain their saved bindings when the default login configuration
changes. See the [Enterprise guide](../GEMINI_ENTERPRISE_CN.md) for configuration
and protocol details.

`GET /quota/gemini-enterprise?auth_index=<credential-index>` reads the saved
credential's daily Assistant/Search quota pools. It requires management
authentication and returns only observations: `status`, `project_id`, `location`,
`observed_at`, nullable `period_start` and `reset_at`, and `pools`. Each pool has
`tier`, `status`, nullable numeric `limit`, `used`, and `remaining`, and nullable
`usage_updated_at`. The status is `ok`, `no_data`, `no_permission`, or
`unavailable`. Optional read failures return HTTP 200 with a soft status, without
upstream error bodies or changes to credential health. Management authentication
failures retain their normal HTTP status.

The backend reads active and grace-period license configurations and Cloud
Monitoring's native Discovery Engine quota metrics. It sums DELTA usage across all pages and methods
in the current `America/Los_Angeles` calendar day and reads the latest limit
GAUGE. `remaining` is calculated from reported usage, which can be delayed; it
is not an account's real-time balance. Empty series remain unknown. Pools are
shared by project, location, and edition. Enable Discovery Engine and Cloud
Monitoring and grant the account subscription/monitoring read access (for
example, the project's `roles/viewer` role). IAM denial is represented by
`no_permission`; API-disabled and other failures use `unavailable`. Responses use
`Cache-Control: no-store`. Pagination failures, overflow, and Monitoring responses
with `executionErrors` or `unreachable` do not establish a remaining balance.
Missing, disabled, and non-Enterprise credentials do not trigger upstream reads.
An expiring token is refreshed only in memory for the optional query; stored
credentials, resource bindings, and routing health remain unchanged.

## v0 compatibility

The `/v0/management` endpoints keep their original paths and payloads, including
provider-specific OAuth login URLs, callbacks, status queries, and cancellation.
For example, `GET /v0/management/codex-auth-url` and
`GET /v8/management/oauth/auth-url?provider=codex` start the same login flow.

Legacy flat configuration endpoints such as `/debug`, `/request-retry`, and
`/codex-api-key` exist under `/v0/management` only.
`/v0/management/api-keys` continues to manage client authentication keys; the v8
equivalent is `/v8/management/config/access/api-keys`.

Legacy-only configuration files keep their layout until a successful v8
configuration write. When both layouts specify a field, the new field takes
precedence and its legacy equivalent is removed. V0 setters keep legacy-only
files in their original layout; existing v8 files are saved in the latest v8
layout. `PUT /v0/management/config.yaml` still replaces the complete file and
accepts legacy, new, or mixed layouts, normalizing v8 documents on save.

Plugin OAuth uses the shared v8 login endpoint. Other plugin-defined HTTP
extensions retain their declared `/v0/management` routes.
