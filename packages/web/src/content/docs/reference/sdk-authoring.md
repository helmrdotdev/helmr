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

`defineConfig({ project, dirs, ignorePatterns? })` declares the deploy target project and task directories. `project` must be a non-empty string, and `dirs` must be a non-empty string array. `ignorePatterns` overrides deploy archive defaults.

Task shape:

```ts
import { z } from "zod"

const payloadSchema = z.object({ prNumber: z.number().int().positive() })

export const review = task({
  id: "review-pr",
  sandbox: sb,
  maxDuration: 900,
  secrets: { OPENAI_API_KEY: { env: "OPENAI_API_KEY" } },
  payloadSchema,
  run: async (payload, ctx) => ({ ok: true }),
})
```

Task IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. `maxDuration` is seconds, default `900`, minimum `5`, maximum `86400`.
`payloadSchema` is optional. Omit it for no-payload tasks; provide a schema that validates through Standard Schema v1 and exposes `toJSONSchema()` for deployment metadata. Zod v4 schemas satisfy this contract.

Image builders support `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, and `user`. `run` can bind cache mounts and build-time secret mounts.

Sandbox builders support `image`, `workspace`, and `resources`. The default workspace mount is `/workspace`.

At runtime, `ctx.wait.token`, `ctx.wait.for`, `ctx.wait.until`, `ctx.emit`, `ctx.log`, `ctx.signal`, and `ctx.run.id` are available inside task `run`.
