# Gemini Enterprise Hydrogen application admission

The OAuth Gemini Enterprise management UI provides Configure Google Hydrogen, Batch test Hydrogen and a separate per-application Hydrogen switch. The normal model list includes Hydrogen after the application is manually enabled and eligible. A closed application's fixed-model test candidate remains visible even when Google returns no Hydrogen catalog entry.

## Configuration and local admission

`POST /v8/management/credentials/gemini-enterprise/hydrogen/configure-google` accepts the same frozen-account and application-revision payload as the retained Carbon endpoint, with batches of at most five imported applications. Snapshot IDs are bound to the target model. Both endpoints share bounded operation coordination and Engine locking.

The Google operation sets only `Engine.modelConfigs["gemini-4-hydrogen"] = MODEL_ENABLED`. If the Assistant already has an explicit allowed-model list, Hydrogen is appended. Empty or missing lists remain implicit. Other model states and application options are preserved and the resource is reread after writing. An uncertain PATCH is reread without automatic replay.

The local switch is `settings.hydrogen_enabled`. Use the existing v8 collection PATCH with only `fields: {"hydrogen_enabled": true}` and the current binding and revision. Missing values default to false. Imported credentials cannot inject an enabled switch or server-owned proof. Reauthorization by a different principal or resource rebinding resets the affected model state. Carbon metadata is independent.

Credential summaries, collections and model snapshots expose `hydrogen_admission_supported` and `hydrogen_admission`. The summary has the existing enablement, eligibility and diagnostic fields, including `verification_required: false`. Account summaries also expose `google_hydrogen_configure_supported`, authorization and registration fences, and enabled/available counts.

Manual eligibility uses a current enabled credential, assigned active subscription, explicit custom allowlists and exclusions. Trial status alone does not grant or deny Hydrogen. No historical test is required to open the switch. Business generation retains normal quota, concurrency, cooldown and cancellation behavior. The shared Argon/Carbon force switch does not admit Hydrogen.

## Diagnostic generation

`POST /v8/management/credentials/models/test` accepts `model: "gemini-4-hydrogen"` with the current selected application and resources. A server-created internal scope pins one credential, application and canonical model, allowing a diagnostic while the business switch is closed. Ordinary requests cannot create this scope.

A verified result requires both a complete independent random code and the same Google session's saved `queryConfig["google.discoveryengine.googleapis.com.Assistant.model_id"]` equal to `gemini-4-hydrogen`. HTTP 200 and a matching code without model proof remain incomplete. Payload configuration is evaluated once at the final business-payload boundary, and a changed model fails the fixed-model test.

Results use `hydrogen_test_id` and `hydrogen_admission`. Evidence is stored under `enterprise_hydrogen_evidence` and projected only into its bound application. Model-specific leases prevent overlapping Carbon and Hydrogen tests from overwriting each other. Failures affect diagnostic history independently of the user's switch.

The two-worker batch queue records each submitted application once, continues after ordinary application failures, and stops scheduling on cancellation or invalidated scope. Configuration, tests and manual enablement remain explicit operations.

## Compatibility

Existing Carbon v8 endpoints, settings and evidence formats remain available. The new UI requires Hydrogen capabilities from the backend and shows unsupported controls when connected to an older backend. Carbon follows the shared Argon group behavior on all applications, including applications created before this change.
