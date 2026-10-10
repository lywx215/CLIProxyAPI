# Local Gemini inference verification

Verified on 2026-10-10 through `http://127.0.0.1:18317`.
The user authorized real requests using the credential at Gemini group 0, Key 0.
No credential values are included in this record.

## Successful alias requests

During the test, the unified alias `gemini-2.5-pro` mapped to
`gemini-3.1-flash-lite-preview`. The Key's local mapping for this source was
temporarily changed to an identity mapping so the unified rule could apply.
The other 99 Keys were temporarily excluded from all models.

| Request | Proxy status | Upstream status | Result | Duration | Tokens |
| --- | --- | --- | --- | --- | --- |
| Streaming | 200 | 200 | `OK.`; `[DONE]` received | 6.900 s | 5 input, 2 output |
| Non-streaming | 200 | 200 | `OK.`; finish reason `stop` | 9.227 s | 5 input, 2 output |

Both response traces selected the expected credential. Matching local request
logs independently confirmed the masked authentication header and the actual
source model. Upstream paths were:

- `/v1beta/models/gemini-3.1-flash-lite-preview:streamGenerateContent`
- `/v1beta/models/gemini-3.1-flash-lite-preview:generateContent`

Both paths used the `generativelanguage.googleapis.com` HTTPS origin.
Requests used a short prompt, `max_tokens: 16`, and `reasoning_effort: minimal`.

## Earlier attempts

| Source model | Proxy status | Observation |
| --- | --- | --- |
| `gemini-3.1-flash-lite` | 503 | Expected Key selected; outbound request recorded |
| `gemini-3.1-flash-lite` | 503 | No outbound attempt; consistent with local cooldown |
| `gemini-2.5-flash` | 404 | Expected Key selected; outbound request recorded |
| `gemini-3.1-flash-lite-preview` | 500 | Expected Key selected; outbound request recorded |

These earlier attempts ran with request logging disabled. Their detailed
upstream error bodies were unavailable, so the proxy status alone does not
establish the root cause. Later successful preview requests confirm this
credential can perform inference through the local proxy.

## Restoration and limits

All temporary exclusions, alias changes, and request-log changes were restored.
A final management configuration read exactly matched the original YAML text.
No production deployment or permanent credential/configuration change occurred.

The original unified rule remains
`gemini-3.1-flash-lite -> gemini-2.5-pro`. Its source has a non-identity local
mapping on this Key, which takes precedence over the unified rule. Therefore,
the successful test establishes routing for the temporary preview mapping; it
does not establish successful inference for the original stable source, or make
the alias persistently visible after restoration.

No high-concurrency inference test was performed. The ignored local evidence
file is `UnitTest/v8-gemini-integration/real-alias-results.json`.
