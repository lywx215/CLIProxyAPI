# Gemini Enterprise model policies

Each account collection stores its own `model_policy` under collection settings.
The default, including legacy credentials without the field, is `auto`.

| Policy | Allowed upstream models |
| --- | --- |
| `auto` | Trial Plus excludes Argon; trial Business allows only Argon; other confirmed active subscriptions use the catalog. Carbon uses an independent manual switch and the common subscription and execution checks. |
| `catalog` | Use the upstream catalog with existing exclusions. |
| `argon_only` | Only `gemini-4-argon`, subject to subscription and catalog availability. It does not grant or exclude Carbon. |
| `exclude_argon` | The effective catalog except `gemini-4-argon`. It does not grant or exclude Carbon. |
| `custom` | Intersect the catalog with `allowed_models` when supplied. |

An absent or null `allowed_models` does not add a whitelist. An empty array blocks
every model in custom mode. Entries are exact upstream IDs. Existing
`excluded_models` apply in every mode, including provider-wide exclusions.
Switching modes preserves the dormant whitelist and existing exclusions.

Every confirmed, active non-trial subscription has a mandatory model allowlist:
`gemini-4-argon`, `gemini-4-carbon`, `gemini-3-pro-image`, and `veo-3.1`. This applies to every edition
and all five policy modes. Other models are the intersection of the upstream
catalog, this mandatory allowlist, the selected policy and exclusions. Carbon
additionally needs an explicit manual switch; diagnostic model tests are optional.
A custom whitelist and explicit exclusions still constrain it.
Manual settings can narrow this range but cannot expand it. Future text or media
model IDs require an explicit policy update. Confirmed trial subscriptions retain
their existing behavior for Argon and all other models. Carbon is an independent
exception: a trial label alone neither denies nor grants it. Both paid and trial
applications require a valid assigned subscription and manual enablement, subject
to custom whitelists, explicit exclusions, and the common quota and cooldown rules.

Google's catalog may omit Carbon even when generation is available. A paid
subscription, an upstream listing, an Argon template, or a previous successful
ordinary request does not enable Carbon. Only an admitted application receives a
local entry reflecting its manual setting. If Carbon is absent from the raw
catalog, this entry is labeled `catalog_source=manual`, `upstream_listed=false`.
Missing, failed or stale diagnostic records and payload-rule changes do not
revoke a manually enabled application's business eligibility.
The raw observed and persisted catalog remains unchanged, including authoritative
empty catalogs. Default paid fallback catalogs include Argon only. Catalog
fallback settings do not replace Carbon's independent admission decision. See
[Carbon application admission](gemini-enterprise-carbon-admission.md).

Use the existing v8 collection PATCH endpoint, its `fields` object, and the current
`binding_id`, `auth_index` and `expected_revision`. Subscription observations and
`model_policy_state` in credential responses are read-only runtime data.
Its `enforced_allowed_models` field contains the four IDs for paid subscriptions,
null for confirmed trials, or an empty array when subscription verification blocks
generation. `enforced_reason` explains the mandatory restriction. Clients distinguish
an absent field on older backends from null and an empty array; these observations
are never accepted as persisted collection settings.
The trial Carbon exclusion from the earlier paid-only implementation is no longer
emitted. This does not change trial Auto or Argon selection. Frontends retain
compatibility with older backends lacking optional policy fields.

Every policy mode requires a verified assigned subscription. Observations refresh
in the background every five minutes. A temporary failure retains the last
confirmed policy for at most 30 minutes; first-time unknown, conflicting,
unassigned or inactive subscriptions pause generation in all modes. Read-only
resource queries and subscription refreshes remain available. Manual settings do
not grant Google access or override the mandatory paid restriction.
Observation caches are not persisted and are rebuilt after restart.

Subscription reads paginate unfiltered user licenses and match the verified OAuth
principal locally. Numeric project IDs avoid Cloud Resource Manager. Observations
are shared by authorization, project number and location; access-token refreshes
retain them, while replacement authorizations and obsolete asynchronous results
cannot overwrite the current decision. Explicit revocation, invalid bindings and
unknown subscription tiers stop generation immediately.

License fields and effective states follow the [Google LicenseConfig reference](https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1/projects.locations.licenseConfigs).
`DEACTIVATING` remains effective during Google's grace period.

Model registration filters real upstream IDs before aliases and prefixes are
generated. Every generation validates its final model after payload rules,
including streaming, media, pinned model tests and raw management streamAssist
requests. Local refusals do not invoke Google generation or establish quota
cooldowns. No subscription lookup is added to an inference request.
Public Auto requests may resolve to an allowed explicit text model; they cannot
implicitly start image or video generation. Media generation requires an explicit
media model ID.

These restrictions follow the OAuth principal's assigned license, rather than the
mere presence of a paid license configuration in the project. A paid principal's
ordinary text requests require another eligible credential or provider. The
Google project/location/edition quota pool is unchanged: applications do not gain
independent balances, and this policy does not reserve quota for particular models.
