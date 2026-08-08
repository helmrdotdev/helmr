---
title: Tasks
description: One-shot typed work deployed and executed as Runs.
---

# Tasks

A Task is a deployed definition with an ID, optional payload schema, execution
defaults, and a `run` function:

```ts
import { task } from "@helmr/sdk"
import { z } from "zod"

export const reviewPr = task({
  id: "review-pr",
  payload: z.object({
    number: z.number().int().positive(),
  }),
  queue: "reviews",
  maxDuration: "15m",
  retry: {
    maxAttempts: 3,
    backoff: { minDelay: "10s" },
  },
  async run(payload, ctx) {
    return { number: payload.number, runId: ctx.run.id }
  },
})
```

IDs must match the SDK's task identifier contract. A Task with `payload`
accepts and validates JSON input through Standard Schema v1. A Task without
`payload` rejects input. Payload is persisted as Run data and must not contain
credentials.

Task context exposes the current Run, Deployment, Workspace reference, and
abort signal. Runtime operations are available through SDK modules: structured
logging, metadata, child task starts, Workspace operations, timers, Tokens, and
Actor Sessions.

External callers start a Task with an existing Workspace. The CLI requires
`--workspace`; `HelmrClient.tasks.start()` requires a `WorkspaceRef`. The
result is a typed `RunHandle`, not the Task output. Call `runs.wait(handle)` or
retrieve the Run until it reaches a terminal state.

Inside managed code, `task.start()` also returns a handle. `task.call()` waits
for the child result and requires an idempotency key. Task output is one
terminal JSON value. If work needs continuing input or progressive durable
output, model it as an Actor instead.

Run defaults on the definition include queue, maximum duration, queued TTL,
and retry policy. A start can supply queueing, retry, metadata, and tag options,
but it cannot replace the deployed maximum execution duration.
