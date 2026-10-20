---
title: Client compatibility and loading feedback
summary: Let clients discover models and handle loading responses instead of treating a swap as a failure.
category: guides
tags: [clients, loading, models, api]
config_keys: [includeAliasesInList, sendLoadingState, models.*.checkReady]
updated: 2026-10-20
---

# Client compatibility and loading feedback

Clients should read `/v1/models` before choosing a model and be prepared for a
request to wait while llama-swap loads it. Set `sendLoadingState: true` when a
client can display a loading response rather than timing out or retrying
blindly. `includeAliasesInList` also exposes configured aliases in model lists.

```yaml
sendLoadingState: true
includeAliasesInList: true
```

The `status.value` field of each `/v1/models` entry is `loaded`, `starting`,
`stopping`, or `unloaded`. A model that is still passing its health check and
`checkReady` predicate reports `starting`, not `loaded`. While starting, its
`meta.llamaswap` object (and the metadata of its aliases) can include:

- `loadingProgress`: fraction loaded, from `0` to `1`.
- `loadingMessage`: the upstream's current loading step.

```json
"status": {"value": "starting"},
"meta": {"llamaswap": {"type": "model", "loadingProgress": 0.42,
                       "loadingMessage": "loading tensors"}}
```

`/running` reports the same state for local processes, with `loadingProgress`
and `loadingMessage` populated from the same snapshot. A status of `starting`
means the model cannot serve a request yet; clients should keep waiting rather
than treating it as loaded.

This changes feedback, not loading speed. Keep the client timeout longer than
the model's startup time and use a client that understands the selected API.
