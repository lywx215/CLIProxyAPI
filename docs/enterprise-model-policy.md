# Gemini Enterprise model policies

Each account collection stores its own `model_policy` under collection settings.
The default, including legacy credentials without the field, is `auto`.

| Policy | Allowed upstream models |
| --- | --- |
| `auto` | Trial Plus excludes Argon and Carbon. Trial Business allows the Argon and Carbon group. Other confirmed active subscriptions use the catalog. |
| `catalog` | Use the upstream catalog with existing exclusions. |
| `argon_only` | Allow the subscription's model group, subject to catalog and policy. |
| `exclude_argon` | Exclude the subscription's model group from the effective catalog. |
| `custom` | Intersect the catalog with `allowed_models` when supplied. |

An absent or null `allowed_models` does not add a whitelist. An empty array blocks
every model in custom mode. Entries are exact upstream IDs. Existing
`excluded_models` apply in every mode, including provider-wide exclusions.
Switching modes preserves the dormant whitelist and existing exclusions.

All Models defaults to on when the OAuth principal has a verified paid subscription
and the collection uses `auto` or `catalog` without a saved
`all_models_enabled` setting. It uses the full
eligible catalog, with existing model exclusions and feature switches still applied.
The default is computed at runtime without writing the credential. A saved
`all_models_enabled: false` restores the collection's configured model policy and
the paid model limit. Trial collections and explicit restricted policies still
default to off.

The restricted paid catalog includes Argon, Carbon, Hydrogen, Gemini 3 Pro Image
and Veo 3.1. The trial group contains Argon and Carbon. Hydrogen requires a confirmed
paid subscription in every mode, including with All Models and a custom whitelist.

The historical application setting `argon_force_enabled` retains its priority and supplements Argon and Carbon when missing from the catalog. A previous `carbon_enabled=true` may supplement Carbon in the local catalog, subject to the effective group policy. The management UI shows the effective policy and the All Models setting.

Carbon remains available through the normal catalog even when the legacy Carbon switch is closed. Existing Carbon configuration and diagnostic APIs remain available for older clients. Carbon diagnostics are stored separately from Hydrogen evidence.

The existing `hydrogen_enabled` preference and verification records remain stored for rollback. Ordinary model registration and generation use the subscription, catalog and effective policy. See [Hydrogen application admission](gemini-enterprise-hydrogen-admission.md).

Successful catalogs, including empty catalogs, take precedence. A failed read uses a retained catalog before applying the application's fallback settings. The default paid fallback includes Hydrogen. The custom fallback uses the configured IDs. Explicit Argon and Carbon supplements apply to the local effective view.

Use the existing v8 collection PATCH endpoint, its `fields` object, and the current
`binding_id`, `auth_index` and `expected_revision`. Subscription observations and
`model_policy_state` in credential responses are read-only runtime data.
Its `all_models_enabled` field reports the effective switch, including the paid
default. The `enforced_allowed_models` field contains the five restricted paid IDs
when the switch is off, null for confirmed trials or when All Models is on, and an
empty array when subscription verification blocks generation. The trial ceiling is
expressed by `enforced_excluded_models`, which includes Hydrogen.
`enforced_reason` explains a paid limit or blocked subscription. Clients distinguish
an absent field on older backends from null and an empty array; these observations
are never accepted as persisted collection settings.
Frontends retain compatibility with older backends lacking optional policy fields.

Every policy mode requires a verified assigned subscription. Observations refresh
in the background every five minutes. A temporary failure retains the last
confirmed policy for at most 30 minutes; first-time unknown, conflicting,
unassigned or inactive subscriptions pause generation in all modes. Read-only
resource queries and subscription refreshes remain available. The eligible models
still depend on the catalog, and Google verifies access to each model at dispatch.
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
implicitly start image or video generation. Trial applications require an
explicit resolved model because upstream Auto cannot enforce the Hydrogen
restriction. Media generation requires an explicit media model ID.

These policies follow the OAuth principal's assigned license. The Google
project/location/edition quota pool stays shared across models and applications.
