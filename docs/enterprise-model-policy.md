# Gemini Enterprise model policies

Each account collection stores its own `model_policy` under collection settings.
The default, including legacy credentials without the field, is `auto`.

| Policy | Allowed upstream models |
| --- | --- |
| `auto` | Trial Plus excludes Argon and Carbon; trial Business allows the Argon and Carbon group; other confirmed active subscriptions use the catalog. Hydrogen uses its own application switch. |
| `catalog` | Use the upstream catalog with existing exclusions. |
| `argon_only` | Allow Argon and Carbon under the same subscription and catalog rules. |
| `exclude_argon` | Exclude both Argon and Carbon from the effective catalog. |
| `custom` | Intersect the catalog with `allowed_models` when supplied. |

An absent or null `allowed_models` does not add a whitelist. An empty array blocks
every model in custom mode. Entries are exact upstream IDs. Existing
`excluded_models` apply in every mode, including provider-wide exclusions.
Switching modes preserves the dormant whitelist and existing exclusions.

Confirmed paid subscriptions admit Argon, Carbon, Gemini 3 Pro Image and Veo 3.1 through the normal catalog and policy path. Hydrogen is additionally eligible through its independent application switch, an active assigned subscription, explicit custom allowlists, exclusions and the existing execution checks. Tests are optional diagnostics.

The existing application setting `argon_force_enabled` now force-enables both `gemini-4-argon` and `gemini-4-carbon` with the existing Argon precedence. Both models are supplemented when missing from the catalog. The stored keys `argon_only` and `exclude_argon` control the same pair. All existing applications use these rules immediately. A previous `carbon_enabled=true` may supplement Carbon in the local catalog but cannot bypass the new group policy. In particular, an existing exclude-Argon application loses Carbon routing unless the shared force switch overrides the policy.

Carbon remains available through the normal catalog even when the legacy Carbon switch is closed. Existing Carbon configuration and diagnostic APIs remain available for older clients. Carbon diagnostics are stored separately from Hydrogen evidence.

Hydrogen defaults to closed for both paid and trial applications. Once manually admitted it is registered as `catalog_source=manual` and `upstream_listed=false` when absent from Google's catalog. A configured Google Engine state, a successful diagnostic or a group force switch does not open the local Hydrogen switch. See [Hydrogen application admission](gemini-enterprise-hydrogen-admission.md).

Default fallback catalogs include both Argon and Carbon wherever the prior fallback supplied Argon. Raw successful catalogs, including empty catalogs, remain authoritative. The explicit manual model supplements are applied only to the local effective view.

Use the existing v8 collection PATCH endpoint, its `fields` object, and the current
`binding_id`, `auth_index` and `expected_revision`. Subscription observations and
`model_policy_state` in credential responses are read-only runtime data.
Its `enforced_allowed_models` field contains the five IDs, including independently admitted Hydrogen, for paid subscriptions,
null for confirmed trials, or an empty array when subscription verification blocks
generation. `enforced_reason` explains the mandatory restriction. Clients distinguish
an absent field on older backends from null and an empty array; these observations
are never accepted as persisted collection settings.
Hydrogen remains independent from the Argon/Carbon trial policy templates. Frontends retain
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
