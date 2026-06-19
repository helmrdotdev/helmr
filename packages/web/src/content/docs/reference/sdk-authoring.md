---
title: SDK authoring
description: TypeScript task, sandbox, image, source, workspace, and run I/O APIs.
section: Reference
sidebarLabel: SDK authoring
order: 910
---

# SDK authoring

Import task-authoring APIs from `@helmr/sdk`:

```ts
import { channels, defineConfig, image, logger, metadata, sandbox, schedules, source, task, wait } from "@helmr/sdk"
```

`defineConfig({ project, dirs, ignorePatterns? })` declares the deploy target project and task directories. `project` must be a non-empty string, and `dirs` must be a non-empty string array. `ignorePatterns` overrides deploy archive defaults.

Task shape:

```ts
import { z } from "zod"

const payload = z.object({ prNumber: z.number().int().positive() })

export const review = task({
  id: "review-pr",
  sandbox: sb,
  maxDuration: 900,
  secrets: [{ name: "OPENAI_API_KEY", env: "OPENAI_API_KEY" }],
  payload,
  run: async (payload, ctx) => ({ ok: true }),
})
```

Task IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. `maxDuration` is seconds, default `900`, minimum `5`, maximum `86400`.
`payload` is optional. Omit it for no-payload tasks; provide a schema that validates through Standard Schema v1. Zod v4 schemas satisfy this contract and can be passed directly.

Scheduled task shape:

```ts
export const cleanup = schedules.task({
  id: "cleanup",
  sandbox: sb,
  secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }],
  cron: { pattern: "0 2 * * *", timezone: "UTC" },
  run: async (payload, ctx) => {
    logger.info("scheduled", payload.timestamp.toISOString())
  },
})
```

Use `schedules.task()` for declarative cron schedules. It does not accept arbitrary `payload`; Helmr supplies schedule metadata at run time.

Image builders support `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, and `user`. `run` can bind cache mounts and build-time secret mounts.

Sandbox builders support `image`, `workspace`, and `resources`. The default workspace mount is `/workspace`.

At runtime, `ctx` is intentionally small: `ctx.signal`, `ctx.run`, `ctx.task`, `ctx.workspace`, and `ctx.session`. Use `ctx.session.output(channels.output(...)).append(...)` for durable session output, `ctx.session.input(channels.input(...)).wait()` for durable session input, and module-level operations for other side effects: `wait.createToken(...)`, `wait.forToken(...)`, `wait.completeToken(...)`, `metadata.set(...)`, `wait.for(...)`, `wait.until(...)`, and `logger.info(...)`.
