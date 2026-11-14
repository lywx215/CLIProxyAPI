# Gemini Enterprise Hydrogen application admission

Gemini Enterprise OAuth applications with a confirmed paid subscription can
register and use `gemini-4-hydrogen` through the ordinary model path. A valid
cached paid subscription remains eligible during a temporary subscription
refresh failure. The application must be enabled and its effective model policy
and catalog must allow Hydrogen. The paid group includes Argon, Carbon and
Hydrogen, and trial applications cannot use Hydrogen in any policy mode.

A successful Google catalog takes precedence, including an empty catalog. A
catalog read failure uses available cached results before applying the configured
fallback. The default paid fallback includes Hydrogen. A custom fallback uses
the specified model IDs. See [Enterprise model policies](enterprise-model-policy.md).

## Model tests and legacy clients

`POST /v8/management/credentials/models/test` with `hydrogen_direct: true`
runs an ordinary fixed-application text test. Request the model ID from
`GET /v8/management/credentials/models` and include the current credential index,
application binding, revision and resources. Registered aliases are supported.
The response includes the canonical `upstream_model`, the current application
binding and revision, and the standard model test result. Normal usage, quota,
concurrency and cooldown rules apply, including the check on the final model
after payload configuration. An application edit or replacement OAuth grant
invalidates the test.

Ordinary generation can try another eligible application if the first
application rejects the model locally. Local policy rejections do not count as
upstream requests. Raw management generation must specify a single
unambiguous value for each model selector.

A legacy test request that omits `hydrogen_direct` still uses the diagnostic
response with `hydrogen_test_id` and `hydrogen_admission`. The diagnostic checks
the same subscription, catalog and policy before starting. Completing the
diagnostic requires an independent random code and proof that Google's saved
session selected Hydrogen. Historical `hydrogen_enabled` and verification
records remain in credential storage for rollback. Editing the former switch
returns HTTP 409 with the code `hydrogen_managed_by_policy`.

## Explicit Google configuration

`POST /v8/management/credentials/gemini-enterprise/hydrogen/configure-google`
retains the frozen-account and application-revision request format, with a
maximum of five imported applications. Existing clients can explicitly enable
Hydrogen in Google Engine settings and, if configured, append it to the
Assistant's explicit model list. Empty and missing lists keep their implicit
defaults. The endpoint rereads the resource after writing and rereads uncertain
PATCH results without replaying them. Ordinary registration and testing do not
edit Google Engine settings.
