---
title: Upstream API passthrough
summary: Control which /upstream model paths are ignored instead of triggering a model swap.
category: guides
tags: [upstream, proxy, api]
config_keys: [upstream, models.*.uiUrl]
updated: 2026-08-28
---

# Upstream API passthrough

`upstream` controls the `/upstream/<model>/<path>` passthrough endpoint. Its
`ignorePaths` expressions prevent static or UI paths from triggering a model
swap; it does not configure a fallback OpenAI service for unmatched requests.

Use RE2-compatible expressions and keep the default static-asset patterns
unless a client needs different paths. Check logs to distinguish an ignored
path from a model-routing request.

## Linking to a backend's native UI

`/upstream/<model>/` strips its prefix, but it cannot rewrite root-absolute
asset URLs. A backend UI that asks for `/assets/app.js` requests llama-swap's
own root instead of the backend's, so the page breaks. If a backend ships its
own UI, point `models.*.uiUrl` at it directly. The dashboard's external-link
button then opens that URL instead.

```yaml
models:
  hipfire:
    cmd: hipfire --port ${PORT}
    uiUrl: "http://host:19095/ui/"
```

`uiUrl` must be an absolute `http://` or `https://` URL, or a root-relative
path starting with `/`. Other values, including `javascript:` URLs and
protocol-relative `//host` links, are rejected at startup. Leave it unset to
keep linking to the passthrough path.

