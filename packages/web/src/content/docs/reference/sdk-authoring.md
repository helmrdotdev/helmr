---
title: SDK authoring
description: TypeScript task, sandbox, image, source, workspace, and waitpoint APIs.
section: Reference
sidebarLabel: SDK authoring
order: 910
---

# SDK authoring

Import task-authoring APIs from `@helmr/sdk`:

```ts
import { defineConfig, image, sandbox, source, task, workspace } from "@helmr/sdk"
```

`defineConfig({ dirs, project?, ignorePatterns? })` declares task directories. `dirs` must be a non-empty string array. `ignorePatterns` overrides deploy archive defaults.

Task shape:

```ts
export const review = task({
  id: "review-pr",
  sandbox: sb,
  maxDuration: 900,
  secrets: { OPENAI_API_KEY: { env: "OPENAI_API_KEY" } },
  run: async (payload, ctx) => ({ ok: true }),
})
```

Task IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. `maxDuration` is seconds, default `900`, minimum `5`, maximum `86400`.

Image builders support `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, and `user`. `run` can bind cache mounts and build-time secret mounts.

Sandbox builders support `image`, `workspace`, and `resources`. The default workspace mount is `/workspace`.

At runtime, `ctx.wait.approval`, `ctx.wait.message`, `ctx.emit`, `ctx.log`, `ctx.signal`, and `ctx.run.id` are available inside task `run`.
