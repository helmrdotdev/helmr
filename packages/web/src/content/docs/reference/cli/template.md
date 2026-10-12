---
title: helmr template
description: Inspect Templates in the current or a selected Deployment.
sidebarLabel: template
---

# `helmr template`

```text
helmr template list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr template get TEMPLATE [--deployment DEPLOYMENT_ID] [--json]
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
