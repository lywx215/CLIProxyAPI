# Gemini Enterprise model policies

Each account collection stores its own `model_policy` under collection settings.
The default, including legacy credentials without the field, is `auto`.

| Policy | Allowed upstream models |
| --- | --- |
| `auto` | Trial Plus excludes `gemini-4-argon`; trial Business allows only Argon; other confirmed active subscriptions use the catalog. |
| `catalog` | Use the upstream catalog with existing exclusions. |
| `argon_only` | Only `gemini-4-argon`, if present upstream. |
| `exclude_argon` | The upstream catalog except Argon. |
| `custom` | Intersect the catalog with `allowed_models` when supplied. |

An absent or null `allowed_models` does not add a whitelist. An empty array blocks
every model in custom mode. Entries are exact upstream IDs. Existing
`excluded_models` apply in every mode, including provider-wide exclusions.
Switching modes preserves the dormant whitelist and existing exclusions.

Every confirmed, active non-trial subscription has a mandatory model allowlist:
`gemini-4-argon`, `gemini-3-pro-image`, and `veo-3.1`. This applies to every edition
and all five policy modes. The effective models are the intersection of the
upstream catalog, this mandatory allowlist, the selected policy and exclusions.
Manual settings can narrow this range but cannot expand it. Future text or media
model IDs require an explicit policy update. Confirmed trial subscriptions retain
their existing automatic and manual behavior.

Use the existing v8 collection PATCH endpoint, its `fields` object, and the current
`binding_id`, `auth_index` and `expected_revision`. Subscription observations and
`model_policy_state` in credential responses are read-only runtime data.
Its `enforced_allowed_models` field contains the three IDs for paid subscriptions,
null for confirmed trials, or an empty array when subscription verification blocks
generation. `enforced_reason` explains the mandatory restriction. Clients distinguish
an absent field on older backends from null and an empty array; these observations
are never accepted as persisted collection settings.

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
