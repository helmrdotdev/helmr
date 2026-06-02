---
title: Tasks
description: The TypeScript unit of work Helmr deploys and runs.
section: Concepts
sidebarLabel: Tasks
order: 130
---

# Tasks

A task is a TypeScript unit of work exported from a task project. It has an ID, sandbox, optional max duration, optional secret declarations, and a `run` function.

```ts
import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { z } from "zod"

const runtime = image("review")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("review-bun") }],
  })

const sb = sandbox("review")
  .image(runtime)
  .workspace("/workspace")
  .resources({ cpu: 2, memory: "4Gi" })

const reviewPayload = z.object({
  prNumber: z.number().int().positive(),
})

export const reviewPr = task({
  id: "review-pr",
  sandbox: sb,
  maxDuration: 900,
  secrets: {
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
  },
  payload: reviewPayload,
  run: async (payload, ctx) => {
    ctx.log.info("reviewing", payload.prNumber)
    return { ok: true }
  },
})
```

## IDs And Payloads

Task IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Tasks without `payload` do not accept payload. Tasks with `payload` validate payload at trigger time and before `run`.

Payload is audit data. Helmr persists it in plaintext in the database, run events, and event streams. Do not put tokens, API keys, credentials, or sensitive personal data in payloads.

## Runtime Context

The task context provides `ctx.log`, `ctx.emit`, `ctx.signal`, `ctx.run.id`, and `ctx.wait` for manual and delay waitpoints. The return value becomes run output when the task succeeds.

## Scheduled Tasks

Use `schedules.task()` instead of `task()` for tasks that should run from cron. Scheduled tasks do not declare arbitrary `payload`; Helmr supplies schedule metadata such as `timestamp`, `lastTimestamp`, `scheduleId`, `externalId`, and `upcoming`. See [Schedules](/docs/concepts/schedules/) for the schedule model.
