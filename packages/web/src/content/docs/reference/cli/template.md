---
title: helmr template
description: Inspect Templates and refresh their prepared images.
sidebarLabel: template
---

# `helmr template`

```text
helmr template list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr template get TEMPLATE [--deployment DEPLOYMENT_ID] [--json]
helmr template refresh TEMPLATE [--async] [--idempotency-key KEY] [--json]
```

`TEMPLATE` is the `id` declared in `template({ id: "browser", ... })`. Reads
return `template`, resources and the resolved `deployment_id`. Create a Computer
with `helmr computer create browser`.

Reads use current unless `--deployment` selects an exact Deployment. A missing
Template never falls back to another Deployment. List pages contain at most
`--limit` items (1–100, default 50); continue with the returned cursor, which
preserves the selected Deployment even after promotion.

Commands accept project/environment scope as described in
[Credential selection](/docs/reference/cli/login#credential-selection).

`refresh` requests preparation for the Template in the current Deployment. It
requires a Template with a `prepare` callback and deployment write permission.
Concurrent requests share an eligible unfinished build. Existing Computers keep
their pinned images, and a failed refresh preserves the previous eligible image.

By default, the command waits for the selected build to publish an eligible
image. `--async` returns its receipt immediately, including `deployment_id`,
`image_spec_id`, and `image_build_id`. Waiting only observes that build; a
failure or expired image requires a new explicit refresh request.

Reuse `--idempotency-key` to recover the original request after a lost response.
The same key keeps the original Deployment and build even after promotion.
Use a new key to request preparation for the current Deployment.
