---
title: Tasks
description: The TypeScript unit of work Helmr deploys and runs.
section: Concepts
sidebarLabel: Tasks
order: 130
---

# Tasks

A Task is authored in JavaScript or TypeScript and exported from a project. A
TypeScript project compiles it through its own build; the Managed Runtime loads
only JavaScript. A Task has an ID, optional Run defaults, an optional payload
schema, and a `run` function.

```ts
import { logger, task } from "@helmr/sdk"
import { z } from "zod"

const reviewPayload = z.object({
  prNumber: z.number().int().positive(),
})

export const reviewPr = task({
  id: "review-pr",
  maxDuration: "15m",
  payload: reviewPayload,
  run: async (payload, ctx) => {
    logger.info("reviewing", { prNumber: payload.prNumber })
    return { ok: true }
  },
})
```

## IDs And Payloads

Task IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Tasks without `payload` do not accept payload. Tasks with `payload` validate payload at start time and before `run`.

Payload is audit data. Helmr persists it in plaintext in the database and
telemetry. Do not put tokens, API keys, credentials, or sensitive personal data
in payloads.

## Runtime Context

The Task context provides read-only execution context such as `ctx.signal`,
`ctx.run.id`, `ctx.task.id`, and `ctx.workspace`. Use `logger` for logs,
`timers` for durable time waits, `tokens` for external callback completion,
and metadata APIs for current Run state. A Task's durable application output is
its terminal result; continuing input/output belongs to an Actor.

## Scheduled Tasks

Use `schedules.task()` instead of `task()` for Tasks that should run from cron.
Scheduled Tasks do not declare arbitrary `payload`; Helmr supplies
`scheduledAt`, optional `lastScheduledAt`, `timezone`, `scheduleId`, and
`upcoming`. See [Schedules](/docs/concepts/schedules/) for the model.
