---
title: Schedules
description: Cron-based task automation, declarative schedules, imperative schedules, and scheduled task payloads.
section: Concepts
sidebarLabel: Schedules
order: 155
---

# Schedules

A schedule creates runs for a deployed task from a 5-field cron expression. Each schedule is scoped to a project environment, stores the GitHub workspace to run against, and binds declared task secrets by vault reference.

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

Use `timestamp` as the scheduled slot time. Use `lastTimestamp` to compare with the previous fired slot. `upcoming` contains the next schedule slots after `timestamp`. Put business constants in code or secrets, not in schedule payload.

## Declarative Schedules

Declarative schedules are defined in task source with `schedules.task()`. They are deployed with the task and reconciled when a deployment is promoted.

```ts
import { cache, image, sandbox, schedules, source, workspace } from "@helmr/sdk"

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
  secrets: {
    API_TOKEN: { env: "API_TOKEN" },
  },
  cron: { pattern: "0 2 * * *", timezone: "UTC" },
  workspace: workspace.github("OWNER/REPO", {
    ref: "main",
    subpath: "path/to/task-project",
  }),
  secretBindings: {
    API_TOKEN: "vault:api-token",
  },
  run: async (payload, ctx) => {
    ctx.log.info("scheduled slot", payload.timestamp.toISOString())
  },
})
```

Declarative schedules are owned by the deployment. They cannot be edited, activated, deactivated, or deleted from the schedules UI or imperative API. Change the task source and deploy again.

## Imperative Schedules

Imperative schedules are created through the runtime client or web UI. Use them when a service or operator needs to register schedules outside task source.

```ts
import { HelmrClient, workspace } from "@helmr/sdk"

const client = new HelmrClient()

await client.schedules.create({
  task: "nightly-maintenance",
  deduplicationKey: "nightly-maintenance-main",
  externalId: "main",
  cron: "0 2 * * *",
  timezone: "UTC",
  workspace: workspace.github("OWNER/REPO", {
    ref: "main",
    subpath: "path/to/task-project",
  }),
  secretBindings: {
    API_TOKEN: "vault:api-token",
  },
})
```

Imperative schedules can be listed, retrieved, updated, activated, deactivated, and deleted. `deduplicationKey` is immutable and must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`.

## Execution Model

The database is the durable source of truth for schedule definitions and schedule instances. The dispatcher reconciles upcoming active schedule instances into Redis and leases due entries from Redis to create runs. Each created run uses a schedule-derived idempotency key so the same schedule slot is not duplicated by retries or dispatcher restarts.

When a schedule fires, the run uses the schedule snapshot: task id, workspace, secret bindings, and run options are read from the schedule record. If the trigger fails, the dispatcher retries with backoff up to the configured attempt limit. If the schedule is changed or deleted before a leased slot completes, stale leases are superseded.

Cron expressions use five fields: minute, hour, day of month, month, and day of week. Timezones must be valid IANA timezone names. Omitted timezone defaults to `UTC`.
