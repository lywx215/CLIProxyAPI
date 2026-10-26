# Gemini Enterprise accounts and collections

One OAuth authorization can own multiple independent project, region and collection bindings. Automatic discovery reads all visible active projects in `global`, `us` and `eu`; it does not create Google resources, change licenses, enable APIs or run a model.

## Authorization and discovery

Automatic OAuth login requires the OAuth application settings, without a project or engine ID. Authorization is saved before discovery. A failed or cancelled discovery retains the saved authorization and existing bindings. A complete explicit manual binding remains compatible with the legacy login flow.

The credential page groups collections under their account. **Refresh projects/collections** discovers later additions and enables newly discovered collections. Existing disablement, application/assistant choices and collection settings are retained. Lists and unreadable scopes are handled separately from successful empty results. Confirmed missing resources are marked unavailable rather than removed.

Each collection keeps one readable engine and assistant. Existing readable choices take precedence. New choices are sorted by resource name, with `default_assistant` preferred. Discovery is bounded to three concurrent metadata requests and uses paginated, size-limited requests to official Google origins with redirects disabled.

Matching legacy credentials are consolidated only after their Google account identity and OAuth client are verified. The selected credential's settings take precedence for duplicate collections. The complete target is saved first; old files are retained as disabled recovery records. Repeated discovery is idempotent. Numeric project canonicalization preserves aliases and does not report an existing project as new.

## Persistence and routing

OAuth token material is persisted once at the account root. Versioned collection settings are stored under `enterprise_account`, with `account_id`, `revision` and `collections`. An account ID depends on the verified account and OAuth client. A binding ID depends on project number, region and collection, excluding engine and assistant.

The SDK projects administrative account parents into independently indexed collection children. Only the children participate in inference. Account disablement does not overwrite individual collection switches. Model mapping, region constraints, explicit credential selection, model cooldowns and request policy continue to apply.

Token acquisition is shared at account scope. Rotated tokens are propagated to all children and persisted once. Reauthorization installs fresh OAuth onto the current graph instead of replaying the constructor's old collection settings. File-backed graph changes use revision checks and atomic replacement. Existing counters, compatible cooldowns and in-flight request attribution survive migration; old application results cannot change a replacement binding's health.

## Management API

Only the maintained `/v8/management` API is extended:

| Operation | Method and path |
| --- | --- |
| Account list, including nested collections | `GET /credentials` |
| Rediscover account resources | `POST /credentials/gemini-enterprise/discover` |
| Update one collection | `PATCH /credentials/gemini-enterprise/collections` |
| Collection model catalog | `GET /credentials/models` |
| Fixed-identity text model test | `POST /credentials/models/test` |
| Collection quota observation | `GET /quota/gemini-enterprise` |

Account operations use the parent's filename and `auth_index`. Collection operations use the parent filename, child `auth_index`, `binding_id` and expected child revision. The collection editor changes engine/assistant within the selected collection; additional project/region/collection bindings are discovered. Ambiguous multi-collection requests and stale revisions return a conflict instead of selecting a first child.

Downloads, deletion, OAuth renewal and the account switch retain their account-level scope. Collection policy editors retain their individual prefix, proxy, weight, headers, exclusions, aliases, retry and error-rule scope. Other providers and `/v0/management` are unchanged.

## Quota and request statistics

Quota observations are selected by the verified user's assigned license edition. If identity is unavailable, only a single unambiguous active pool is a fallback. Multiple unknown pools are neither summed nor guessed. The response retains the pool list and adds nullable `selected_pool` plus account/binding/revision identity.

`remaining = max(0, limit - used)` is computed only when both observations are known. Zero remains zero; missing values remain `null` and display as unavailable. Successful use and limit observations are retained independently on transient failures within the same Pacific day, with stale indication. Conflicting observations, different metric versions and daily resets do not produce a combined balance.

Same-account, same-project, same-region reads share a short cache and in-flight fetch. `refresh=true` forces an update for the shared display pool. Disabled collections remain eligible for this read-only management operation. Quota reads do not change request counters, switches or routing health.

Request statistics are separate from Google quota. Success/failure counters are cumulative; the recent success rate uses the existing twenty ten-minute buckets, matching the request strip. A collection with no recent requests displays an em dash for its recent rate.

## Model testing

Opening a model catalog performs no generation. Explicit model tests send `请只回复 OK。` through the normal credential-pinned executor and usage reporting pipeline. Success requires nonempty visible text and normal completion. Model mapping or payload rules that change the intended actual model cause a configuration conflict.

Tests never fall back to another credential, region or model, or bypass cooldown. A cooling response has no invented inference duration. Batch testing is serial and cancellable; ordinary model failures do not stop remaining entries, while authorization failure, rate limiting and binding changes stop the batch. Closing the dialog or changing connection/binding discards late results. Cancelling a retest retains its previous completed result.

Test results are session observations, not a permanent model availability catalog. Each actual model test consumes upstream quota and enters the selected collection's request and token statistics. Discovery and quota reads do not.
