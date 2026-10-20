---
title: Startup preloading and hooks
summary: Load selected models at startup and run hooks around lifecycle events.
category: guides
tags: [startup, preload, hooks, operations]
config_keys: [hooks, models.*.ttl, models.*.checkReady, models.*.healthCheckTimeout]
updated: 2026-10-20
---

# Startup preloading and hooks

Use startup hooks when an external action must run as llama-swap starts, and
preload only models your machine can keep resident. A model with `ttl: 0` stays
loaded after it has been requested; preloading does not make an incompatible
model fit in memory.

A preload only succeeds after the model passes both the health check and any
`checkReady` predicate. A server that answers HTTP 200 while it is still loading
therefore keeps the preload waiting instead of announcing it early:

```yaml
models:
  slow-model:
    cmd: llama-server --port ${PORT} -m slow.gguf
    checkEndpoint: /health
    checkReady:
      jsonPath: "$.model"
    healthCheckTimeout: 900
```

A preload failure is logged with the model ID and the request's status. Check
the model's own logs before raising `healthCheckTimeout`: the timeout is a bound
on waiting, not a fix for a wrong port or a missing model file.

Keep hook commands fast and observable. A failing hook or a model that cannot
become healthy should be diagnosed in logs before adding retries.
