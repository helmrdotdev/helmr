---
title: SDK authoring
description: TypeScript Task, Actor, Workspace, Schedule, and runtime APIs.
section: Reference
sidebarLabel: SDK authoring
order: 910
---

# SDK authoring

Import declaration and runtime APIs from `@helmr/sdk`:

```ts
import {
  actor,
  defineConfig,
  image,
  logger,
  metadata,
  schedules,
  source,
  task,
  timers,
  tokens,
  workspace,
} from "@helmr/sdk"
```

`defineConfig({ project, dirs, ignorePatterns? })` declares the source roots.
Deploy analyzes exported declarations and produces one immutable Program
Artifact.

```ts
const runtime = image("review")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")

export const reviewWorkspace = workspace("review-workspace")
  .image(runtime)
  .resources({ cpu: 2, memory: "4GiB" })

export const review = task({
  id: "review-pr",
  maxDuration: "15m",
  payload,
  run: async (input, ctx) => {
    logger.info("reviewing", { prNumber: input.prNumber })
    await metadata.set("phase", "review")
    return { ok: true, runId: ctx.run.id }
  },
})
```

Task, Actor, and Workspace IDs use
`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Durations use the closed `ms`, `s`, `m`,
`h`, or `d` grammar.

Schedules are source-only:

```ts
export const cleanup = schedules.task({
  id: "cleanup",
  cron: { pattern: "0 2 * * *", timezone: "UTC" },
  workspace: { key: "maintenance" },
  run: async (input) => {
    logger.info("scheduled", { scheduledAt: input.scheduledAt.toISOString() })
    return { ok: true }
  },
})
```

Task Runs produce one terminal result. Stable interactive workflows use an
Actor's fixed `input` and `output` channels. Use `tokens.create()` for external
completion waits and `timers.waitFor()` or `timers.waitUntil()` for durable
time waits. Runtime metadata mutations and `logger.debug/info/warn/error` are
acknowledged before they return.
