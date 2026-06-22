---
title: Schedules
description: Cron-based task automation, declarative schedules, imperative schedules, and scheduled task payloads.
section: Concepts
sidebarLabel: Schedules
order: 165
---

# Schedules

A schedule starts task sessions for a deployed task from a 5-field cron expression. The logical schedule is scoped to a project. Each environment gets its own schedule instance, which stores run options, active state, and next-fire cursor state.

Schedules are not arbitrary payload templates. Helmr generates the scheduled task payload at fire time so every scheduled run receives consistent schedule metadata:

```ts
type ScheduledTaskPayload = {
  timestamp: Date
  lastTimestamp?: Date
  timezone: string
  scheduleId: string
  externalId?: string
  upcoming: Date[]
}
```

Use `timestamp` as the scheduled fire time. Use `lastTimestamp` to compare with the previous processed fire. `upcoming` contains future schedule fires from dispatch time, so missed fires after a delayed `timestamp` are not backfilled into the payload. Put business constants in code or secrets, not in schedule payload.

## Declarative Schedules

Declarative schedules are defined once in task source with `schedules.task()`. They are deployed with the task and reconciled into the selected project environment when a deployment is promoted.

```ts
import { cache, image, logger, sandbox, schedules, source } from "@helmr/sdk"

const runtime = image("nightly-maintenance")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("nightly-bun") }],
  })

export const nightlyMaintenance = schedules.task({
  id: "nightly-maintenance",
  sandbox: sandbox("nightly-maintenance").image(runtime),
  secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }],
  cron: { pattern: "0 2 * * *", timezone: "UTC" },
  run: async (payload) => {
    logger.info("scheduled slot", payload.timestamp.toISOString())
  },
})
```

Declarative schedules are owned by task source. They cannot be edited, activated, deactivated, or deleted from the schedules UI or imperative API. Change the task source and deploy again. Removing a declaration removes the selected environment instance; the logical schedule is removed only after no environment instances remain.

## Imperative Schedules

Imperative schedules are created through the runtime client or web UI. Use them when a service or operator needs to register schedules outside task source.

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient()

await client.schedules.create({
  task: "nightly-maintenance",
  externalId: "main",
  cron: "0 2 * * *",
  timezone: "UTC",
})
```

Imperative schedules can be listed, retrieved, updated, activated, deactivated, and deleted. `deduplicationKey` is required on create. It provides the stable public key for upserting the project-level logical schedule and the selected environment instance, preventing repeated create calls from producing duplicate schedules. It must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`.

## Execution Model

The database is the durable source of truth for schedule definitions, schedule instances, and the exact next fire timestamp. Redis/Valkey stores a replaceable one-next-fire entry per schedule instance so dispatchers can lease due entries quickly. The dispatcher repairs Redis from the database, but steady-state create, update, activation, deployment promotion, and successful fires enqueue the next fire directly. Each created run uses a schedule-derived idempotency key so the same schedule fire is not duplicated by retries or dispatcher restarts.

When a schedule fires, the run uses the selected environment instance snapshot:
the task id, cron, and timezone come from the logical schedule; run options,
active state, and cursor state come from the environment instance. Helmr
creates a workspace from the task's deployed sandbox for the scheduled session.
If the scheduled start fails, the dispatcher retries with backoff up to the
configured attempt limit. If the schedule is changed or deleted before a leased
slot completes, stale leases are superseded.

Schedules do not backfill every missed cron slot after downtime or dispatcher backlog. Helmr fires the leased slot once, then advances to the next future cron occurrence. The generated `upcoming` payload contains future slots only.

Cron expressions use five fields: minute, hour, day of month, month, and day of week. Timezones must be valid IANA timezone names. Omitted timezone defaults to `UTC`.
