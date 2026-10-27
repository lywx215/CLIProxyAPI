# Gemini Enterprise regional endpoints

## Supported bindings

Every project/location/collection binding selects an explicit HTTPS origin. The resource path must contain the same location as the hostname:

| Location | API origin |
| --- | --- |
| `global` | `https://discoveryengine.googleapis.com` |
| `us` | `https://us-discoveryengine.googleapis.com` |
| `eu` | `https://eu-discoveryengine.googleapis.com` |
| `ca` | `https://ca-discoveryengine.googleapis.com` |
| `au` | `https://au-discoveryengine.googleapis.com` |
| `in` | `https://in-discoveryengine.googleapis.com` |

The documented global alias remains accepted only for compatible global resources. Regional credentials are not sent to another region, a redirect target, an arbitrary derived hostname or a URL with user information or a port override. Previously saved locations outside this supported list retain their legacy behavior; they are not newly advertised as supported.

For example, a Canadian generation request uses:

```text
POST https://ca-discoveryengine.googleapis.com/v1alpha/projects/PROJECT/locations/ca/collections/COLLECTION/engines/ENGINE/assistants/ASSISTANT:streamAssist?alt=sse
```

Use the corresponding `au` or `in` hostname and resource location for Australia or India. The shared global hostname is not a fallback for these bindings.

## Discovery, subscriptions and quota

Automatic discovery checks all six supported locations for each visible active project, with the existing three-request concurrency limit. Only existing readable collections, engines and assistants are added. Empty or unreadable scopes do not cause unrelated saved bindings or their settings to be deleted. Discovery never creates an application, assigns a license, enables an API or generates content.

Use **Refresh projects/collections** after adding regional resources in Google. A collection's identity remains project/location/collection; its editor changes the engine and assistant within that identity. The legacy single-binding resource editor and manual OAuth setup also offer all six supported locations. Switching a legacy binding validates the target application and model catalog before saving.

Resource discovery, model catalogs, assigned-user subscription recognition, inference and media downloads use the same endpoint resolver. Subscription observations and policies remain scoped to the OAuth subject, project and location. Existing trial and paid restrictions continue to apply, and generation adds no synchronous subscription lookup.

Quota licensing metadata uses the regional Discovery Engine endpoint. Cloud Monitoring remains on `monitoring.googleapis.com`, with the exact project and `metric.labels.regional_location` filter. A monitored resource's `location=global` is not evidence that different regional quota pools are shared. Unknown observations remain unknown; permission failures do not become zero. Different applications displaying the same project/region/edition balance must not be summed.

## Direct validation on 2026-10-27

The three country endpoints were validated using one supplied OAuth account with existing regional applications and an assigned active Enterprise Plus trial. Authenticated metadata, paginated user-license lookup, matching license configuration and model catalog requests succeeded on the matching country hosts. The shared hostname with `locations/ca`, `locations/au` or `locations/in` returned `400 INVALID_ARGUMENT` with an explicit incorrect-endpoint explanation.

Short sessionless streaming generation produced nonempty visible text and a `SUCCEEDED` terminal state:

| Model | CA | AU | IN |
| --- | --- | --- | --- |
| `gemini-3.8-flash` | Passed | Passed | Passed |
| `gemini-3.7-flash` | Passed | Passed | Passed |
| `gemini-3.6-flash` | Passed | Passed | Passed |
| `gemini-3.5-flash` | Passed | Passed | Passed |
| `gemini-3.1-pro-preview` | Passed | Passed | Passed |
| `gemini-2.5-pro` | Not listed; not called | Passed | Passed |
| `gemini-4-argon` | Trial refusal | Trial refusal | Trial refusal |

Argon returned `400 FAILED_PRECONDITION`, explicitly stating that the model was unavailable during the free trial. This is separate from endpoint correctness. No retry, region fallback or automatic model substitution was used. Credentials, opaque session data and raw reasoning were not included in the evidence report.

These observations establish availability for this account and time, not every subscription or future catalog. Endpoint success does not establish the physical location of model processing or a data-residency guarantee. Google's [location documentation](https://docs.cloud.google.com/generative-ai-app-builder/docs/locations) describes the published service-specific regional guarantees; do not infer additional guarantees from the hostname alone.
