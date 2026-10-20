---
title: Model capabilities and model listings
summary: Advertise images, tools and context length in /v1/models, automatically or by hand.
category: guides
tags: [capabilities, voices, speech, models, tools, vision, context, autodetect, llama-server, vllm, halogen, playground, decisions, gguf, metadata]
config_keys: [models.*.capabilities, models.*.capabilities.disableAuto, models.*.capabilities.voices, store.path]
updated: 2026-10-20
---

# Model capabilities and model listings

`capabilities` describes what a model can accept or produce. llama-swap reports
it through `/v1/models` so clients can pick a compatible model. This is listing
metadata only: it does not change routing or how requests are proxied. In
particular `tools: true` makes the listing report
`capabilities.function_calling: true` and advertise `tools` and `tool_choice`
as supported parameters. It does not enable tool calling by itself; the model
still needs a compatible chat template and runtime.

## Automatic discovery

llama-swap asks the upstream server what it supports the first time a model
becomes ready, and caches the answer. Anything you do not set yourself is
filled in from that answer.

```yaml
models:
  vision-model:
    cmd: llama-server -m vision.gguf --mmproj mmproj.gguf --port ${PORT}
    # no capabilities block needed
```

After this model has been loaded once, `/v1/models` reports its context length
and image input on its own.

The context length reported is the window one request can actually use, not
the one the model was trained for. A llama-server started with a smaller
`--ctx-size` than the model supports advertises the smaller number, and a
server whose slots are capped advertises the per-slot window rather than the
total it loaded.

What each server can report:

| | llama-server | vLLM | halogen |
| --- | --- | --- | --- |
| context length | yes, the loaded `n_ctx` | yes, `max_model_len` | yes |
| image input | yes | no | yes |
| audio or video input | yes | no | no |
| tools | yes, from the chat template | no | yes |
| decision model output | yes, from `/v1/models` | no | no |
| reranker | no | no | no |

llama-server is read from `/props`, and halogen-flash-server from its
`/health`, which that project documents as the authoritative report of what
the running build accepts. Both vary per deployment: halogen serves images
only when it was started with a vision tower, and that is read rather than
assumed.

Decision models are the one thing `/props` cannot show. They answer
`/v1/systemone` and generate no text, so llama-server marks them with
`output_modalities: ["decisions"]` in its `/v1/models` instead, and llama-swap
reports `out: [decisions]` and no tools for them, whatever their chat template
says.

vLLM is the thin one. Nothing it serves says whether it was started with
`--enable-auto-tool-choice`, or whether the model takes images, so set those
by hand on vLLM models.

llama-swap picks the right reader from the `owned_by` field in the server's
own `/v1/models`: `llamacpp`, `vllm` or `halogen`. To check what a server
will be detected as, ask it directly:

```bash
curl -s localhost:PORT/v1/models | jq -r '.data[0].owned_by'
```

Servers reporting anything else are left alone, including image, speech and
transcription servers, which have no capability surface to read. A llama.cpp
fork that keeps the upstream API reports `llamacpp` and is read the same way.

## Automatic metadata from cmd and GGUF

Discovery needs a running server. Before a model has ever loaded, llama-swap
also reads what it can from the model's `cmd` and, when the command points at a
local GGUF file, from that file's metadata header. This is best-effort: an
unreadable file or an unrecognized flag leaves the corresponding fields out,
and it never fails config loading.

From the command line it infers:

- the runtime context window: `--ctx-size`, `--context-size` or `-c` for
  llama.cpp, `--max-seq` for hipfire, `--max-model-len` for vLLM and
  `--context-length` for SGLang. For llama.cpp the value is divided by
  `--parallel` only when KV caches are per slot; with `--kv-unified` (or
  `-kvu`) the pool is shared, so the per-slot window is the full
  `--ctx-size`. `--kv-unified-per-slot N` caps it at N. When several flags are
  present, `--max-seq` wins over `--max-model-len`, which wins over
  `--context-length`, which wins over the llama.cpp spellings.
- image input when `--mmproj` or `--mm-proj` is present, reported as
  `in: [text, image]`.
- quantization from the model filename, such as `Q4_K_XL` or `MQ4-XTS`.
- draft and speculative-decoding facts from `--model-draft`, `--spec-type`
  and `--spec-draft-n-max`.

From a local GGUF file it reads the model name, description, architecture,
size label, the context length the model was trained for and the chat
template. Only the header is read, never the weights; the parsed header is
cached per file so a config reload does not re-read it.

Tool support is inferred conservatively from that chat template: it must have
a tools guard such as `{%- if tools %}` and a tool-call representation such as
`tool_call`, `function_call` or `<function=`. Models started with `--embedding`
or `--no-jinja` are skipped. This makes the Function Calling tag appear before
the first load; once the model has been probed, the discovered value is
authoritative even when it says no tools.

```bash
curl -s localhost:8080/v1/models | jq '.data[0] | {
  context_length,
  meta: .meta.llamaswap
}'
```

Those facts appear in `meta.llamaswap` as `modelArchitecture`, `quantization`,
`trainedContext`, `sizeLabel`, `draft`, `spec`, `scanSource` and
`contextSource`. A `metadata` key you write yourself always wins over a scanned
value of the same name.

`context_length` is the window one request can actually use: the runtime
window the server was started with, after `--parallel` is taken into account.
`trainedContext` is the window the model was trained for and is usually
larger. They are deliberately separate; a server started with a small
`--ctx-size` must not advertise the trained maximum.

### Precedence

Capabilities are resolved in this order, field by field:

1. explicit `capabilities` in the config
2. values discovered from the running upstream and cached in the store
3. values inferred from `cmd` and the GGUF header

`capabilities.disableAuto: true` turns off both discovery and pre-load
inference, leaving exactly what the config says. The `contextSource` field in
`meta.llamaswap` reports which of the three supplied the advertised context:
`config`, `discovered` or `cmd`.

Tool support follows the same order, with one extra rule: once a capability
probe has produced a result for the model, its answer is used even when it is
false. A pre-load heuristic therefore cannot keep advertising tools after the
loaded server has said the template does not support them.

## Setting capabilities by hand

Values you write win, field by field, as long as they are not the zero value
for their type. Discovery only fills the gaps, so you can correct one field
without giving up the rest:

```yaml
models:
  vllm-model:
    cmd: vllm serve my-model --port ${PORT} --enable-auto-tool-choice
    capabilities:
      # vLLM cannot report this, so say it here
      tools: true
      # context is still filled in automatically from max_model_len
```

Set only what the model really supports. A false claim makes clients send
requests that fail or get ignored.

## Turning discovery off

A field set to its zero value (`false`, `0`, `[]`) cannot be told apart from
one you never wrote, so `tools: false` will not override a discovered `true`.
To take full control of a model's listing, turn discovery off:

```yaml
models:
  my-model:
    cmd: llama-server -m model.gguf --port ${PORT}
    capabilities:
      disableAuto: true
      in: [text]
      out: [text]
      context: 8192
```

With `disableAuto: true` the model advertises exactly what you wrote, and
nothing else. On its own, with no other fields, it advertises nothing.

## Setting the voice list for a speech model

`GET /v1/audio/voices?model={id}` normally goes to the upstream server.
`capabilities.voices` is an override: set it to answer the request from the
config instead:

```yaml
models:
  tts:
    cmd: my-tts-server --port ${PORT}
    capabilities:
      in: [text]
      out: [audio]
      voices: ["voicea", "voiceb", "voicec"]
```

```bash
curl -s 'localhost:8080/v1/audio/voices?model=tts'
# {"model":"tts","voices":["voicea","voiceb","voicec"]}
```

The response is a JSON object with a `voices` array and the `model` id. OpenAI
has no endpoint for listing voices, so this is a llama-swap convention, not an
OpenAI format. The Playground's speech tab reads it. The model is not started to
answer, and voices are not shown in `/v1/models`.
When `voices` is empty or unset there is no override, and the request goes to
the upstream server as before, which starts the model if it is not running.
This applies to local models only, not peers.

## How the Playground uses this

Each Playground tab looks for the modalities it needs and lists those models
first, under "Matching Capabilities":

| tab | wants |
| --- | --- |
| Chat | `in` has `text` or `image`, `out` has `text` |
| Images | `out` has `image` |
| Speech | `in` has `text`, `out` has `audio` |
| Transcription | `in` has `audio`, `out` has `text` |
| Rerank | `reranker: true` |
| Help's Docs agent | `tools: true` |

Discovery fills most of this in on its own once a model has been loaded, so a
llama-server model usually sorts itself. Models that report nothing are still
listed, further down under "Local", so nothing disappears from the selector for
want of configuration. Image, speech and transcription servers are the ones
worth setting by hand: nothing is read from them, so without an `in`/`out` block
they never sort to the top of their own tab.

## What goes wrong

**A model that has never been loaded shows less.** Upstream discovery needs the
server to be running to be asked, so tools and anything else only the server
knows are absent until the first load. Facts read from `cmd` and the GGUF
header (context, vision, architecture, quantization) are available immediately.
Load the model once and the discovered values appear, including after it
unloads again.

**Discovered values do not survive a restart unless you configure a store.**
The cache lives in llama-swap's SQLite database, which is in memory unless
`store.path` is set:

```yaml
store:
  path: /var/lib/llama-swap/llama-swap.db
```

Without it, every restart starts from an empty cache and each model has to be
loaded again before its capabilities reappear.

**The cache can be stale.** Values are refreshed each time a model starts, so
between loads they describe the last run. Changing a model's `cmd` invalidates
its entry immediately, so shrinking `--ctx-size` will not keep advertising the
old number, but other changes to the same command (a different `--mmproj`, for
example) only take effect the next time the model loads.

**Tool support is read from the chat template.** llama-server reports what the
template can do, not what the model was trained for. A template that renders a
tool list is advertised as supporting tools even if the model is bad at using
them.
