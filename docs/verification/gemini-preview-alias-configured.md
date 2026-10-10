# Persistent local Gemini alias configuration

Verified on 2026-10-10 at `http://127.0.0.1:18317`.

The saved unified rule is:

```yaml
routing:
  api-key-model-alias:
    gemini:
      - name: gemini-3.1-flash-lite-preview
        alias: gemini-2.5-pro
        fork: true
```

The conflicting local display alias on Gemini group 0, Key 0 was changed from
`Gemini 3.1 Flash Lite Preview` to the identity alias
`gemini-3.1-flash-lite-preview`. This preserved the source model scope and model
parameters; the other local model entries were retained. Any force-mapping
property on the neutralized entry was removed.

The v8 management write used the current configuration revision with If-Match.
The final persisted YAML was read back and matched the intended configuration.
The final `/v1/models` response included `gemini-2.5-pro`.

| Actual local proxy request | Status | Content | Duration |
| --- | --- | --- | --- |
| Streaming unified alias | 200 | `OK.`; normal stream completion | 6.882 s |
| Non-streaming unified alias, after final configuration save | 200 | `OK.`; finish reason `stop` | 5.632 s |

Both traces selected the user-designated credential. Each request used 5 input
tokens and 2 output tokens. The other 99 Keys were temporarily isolated for the
streaming test; those temporary exclusions were restored before the final
non-streaming request. Existing logging settings were preserved.

The alias configuration remains saved. No production service was modified.
The local sanitized evidence is
`UnitTest/v8-gemini-integration/configured-preview-alias-results.json`.
