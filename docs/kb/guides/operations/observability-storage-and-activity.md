---
title: Observability, storage and Activity
summary: Use logs, metrics, captures and the Activity view to diagnose requests and retain useful history.
category: guides
tags: [operations, logs, metrics, activity, captures, stdout, http]
config_keys: [logLevel, logToStdout, metricsMaxInMemory, captureBuffer]
updated: 2026-10-20
---

# Observability, storage and Activity

Use the Activity view for recent request timing and model events, logs for
process and proxy failures, and metrics for trends. `metricsMaxInMemory` and
`captureBuffer` bound retained in-memory data; increase them only when the
memory cost is acceptable.

```yaml
logLevel: debug
logToStdout: "proxy,http"
metricsMaxInMemory: 1000
captureBuffer: 100
```

## Activity timing

The Activity view reads per-request timings from the backend response. llama.cpp
reports `prompt_n`, `predicted_n`, `prompt_per_second` and
`predicted_per_second`; hipfire reports `prefill_tok_s` and `decode_tok_s`.
hipfire's `tok_s` is wall-inclusive and is used only when `decode_tok_s` is
absent. Token counts fall back to `usage`, so a `timings` object that omits the
llama.cpp fields does not zero the Prompt or Generated columns.

## Log streams

llama-swap keeps three log streams. Each has its own tab on the UI's Logs page
and its own endpoint, `GET /logs/stream/<stream>`:

- `proxy`: llama-swap's own messages, such as model swaps, loading and errors.
- `upstream`: a copy of the upstream processes' stdout and stderr.
- `http`: one access log line per HTTP request (client, method, path, status,
  size, duration).

`logToStdout` chooses which streams are also written to stdout. It takes a
comma separated list such as `"proxy,http"` or `"upstream,http"`, or one of
`"both"` (every stream) and `"none"`. The default is `"proxy"`.

What goes wrong:

- HTTP access lines used to be part of the proxy log. With the default
  `"proxy"` they no longer reach stdout. Use `"proxy,http"` to keep them.
- A value outside the list, like `true` or `"proxy,debug"`, fails config
  loading.
- `logToStdout` is read at startup only. Restart llama-swap after changing it.

Do not put secrets in captures or debug logs. Reduce retention after diagnosing
an issue.
