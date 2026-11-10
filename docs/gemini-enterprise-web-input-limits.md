# Gemini Enterprise Web input limits

The native Web channel accepts per-request parser limits under
`upstream.gemini-enterprise-web.input-limits`. These are local resource controls,
independent of Google's model context, file, account and policy limits.

```yaml
upstream:
  gemini-enterprise-web:
    input-limits:
      max-text-bytes: 4194304
      max-user-parts: 256
      max-model-parts: 4096
      max-system-parts: 256
      max-attachments: 12
      max-attachment-bytes: 52428800
      max-request-bytes: 71303168
      max-json-object-fields: 256
      max-tool-depth: 64
      max-tool-nodes: 100000
      max-tool-array-entries: 4096
      max-generation-config-bytes: 1048576
```

Omitted or zero fields select the defaults above. Active limits reject negative
values and values outside their safety bounds. Serialized context has no limit on
the number of messages or fragments. The former `max-contents` and
`max-segments` keys can remain in an existing YAML file. The Web channel ignores
them and omits them from the effective `input_limits` status.

| Key | Meaning | Allowed maximum |
| --- | --- | --- |
| `max-text-bytes` | Cumulative history/system text, signatures and historical tool records; 4 MiB by default | 64 MiB |
| `max-user-parts` | Parts in each user Content | 65,536 |
| `max-model-parts` | Parts in each model Content | 65,536 |
| `max-system-parts` | Parts in `systemInstruction` | 65,536 |
| `max-attachments` | Combined user and model attachments across the request (12 by default) | 128 |
| `max-attachment-bytes` | Combined decoded attachment bytes (50 MiB by default) | 512 MiB |
| `max-request-bytes` | Complete native JSON request, including Base64 | 1 GiB |
| `max-json-object-fields` | Fields in any parsed JSON object | 4,096 |
| `max-tool-depth` | Nesting depth in historical tool data | 256 |
| `max-tool-nodes` | JSON nodes per historical tool record | 1,000,000 |
| `max-tool-array-entries` | Entries in an array inside historical tool data | 65,536 |
| `max-generation-config-bytes` | Serialized `generationConfig` JSON | 64 MiB |

The values can be changed through YAML hot reload or the existing v8 configuration
API at `/v8/management/config/upstream/gemini-enterprise-web/input-limits`.
Use the normal configuration revision/ETag checks when writing. The configuration
panel shows the backend's history capabilities. The YAML editor verifies the
effective limits after saving, using the legacy count settings only when the
connected backend still enforces them.

Each admitted request retains its own mode and limit snapshot. Updates apply to
new requests, while accepted requests finish under their original limits.
Ordinary text-only Enterprise requests keep their original routing and body cap.
Other standalone users of the shared Business parser retain their original
defaults unless they explicitly provide a Web input-limit context.

The provider-wide `max-inflight-bytes` and `max-concurrent` controls remain in
force. Serialization headroom is measured and reserved against that same byte
budget before allocation. Increasing an input limit does not increase the global
memory budget; sufficiently large concurrent requests can still return 429.
Accepted requests retain their original budget ceiling for later history-buffer
growth, charged against all live reservations. Lowering the budget limits new
admissions; it does not revoke a previous request or clear its occupancy, even
after a quick disable/re-enable. Occupancy can temporarily exceed the new limit.

413 diagnostics retain the configured limit name, structural field path,
observed value and effective maximum. `observed_at_least=true` means validation
stopped at a known lower bound rather than counting or decoding all remaining
input. Logs do not require retaining user text, Base64, files or credentials.
