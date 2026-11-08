---
title: Route to remote peers
summary: Expose models from another llama-swap host through a peer connection.
category: guides
tags: [peers, remote, networking]
config_keys: [peers, peers.*.proxy, peers.*.apiKey, peers.*.models]
updated: 2026-11-08
---

# Route to remote peers

Declare a peer with the other llama-swap `proxy` URL and its model list. Its models are addressed as
`peer-name/model-id`, so a peer named `sippy` serving `gemma-4-12B` appears as
`sippy/gemma-4-12B`.

```yaml
peers:
  sippy:
    proxy: http://sippy:8080
    apiKey: ${env.SIPPY_API_KEY}
    models: [gemma-4-12B]
```

The peer must be reachable from this host and its API key must match the remote
server. See `guides/api-integration/api-keys-and-auth` for keeping it out of
the committed file.

The per-model Logs tab works across the peer. This instance resolves
`peer-name/model-id` and proxies `GET /logs/stream/model-id` to the peer, so the
peer's process output appears in the local UI. Only the model's own log stream
is forwarded; the peer's `proxy`, `upstream` and `http` streams are not merged
into this instance's Logs page.

Peer models can also be loaded and unloaded from the local UI. The power button
posts to this instance's `/api/models/load/<peer>/<model>` or
`/api/models/unload/<peer>/<model>`, which forwards the action to the peer. The
local model list mirrors each peer's lifecycle state by reading the peer's
`/v1/models` listing about every 10 seconds; load and unload actions trigger an
immediate refresh. A peer that cannot be reached is shown as `unknown` while it
is offline. "Unload all" still affects local models only.
