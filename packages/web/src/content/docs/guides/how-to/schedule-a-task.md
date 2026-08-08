---
title: Schedule a task
description: Declare a recurring Task and inspect its reconciled Schedule.
---

# Schedule a task

Define recurring work in source with `schedules.task()`:

```ts
import {
  image,
  sandbox,
  schedules,
  secrets,
} from "@helmr/sdk"

export const reportingSandbox = sandbox({
  id: "reporting",
})
  .image(image("reporting").from("node:24-bookworm-slim"))
  .resources({ cpu: 1, memory: "1GiB" })

export const dailyReport = schedules.task({
  id: "daily-report",
  cron: {
    pattern: "0 9 * * *",
    timezone: "America/New_York",
  },
  workspace: {
    sandbox: reportingSandbox,
    secrets: [
      {
        secret: secrets.fromName("REPORT_TOKEN"),
        env: "REPORT_TOKEN",
      },
    ],
  },
  async run({ scheduledAt, lastScheduledAt, upcoming }, ctx) {
    return {
      runId: ctx.run.id,
      scheduledAt: scheduledAt.toISOString(),
      previous: lastScheduledAt?.toISOString() ?? null,
      next: upcoming[0]?.toISOString(),
    }
  },
})
```

Deploy and promote the project. Promotion reconciles the Schedule from the
declaration; there is no imperative create or update command. Each logical fire
creates a fresh Workspace from the declared Sandbox and secret placements.

Inspect the resulting resource:

```sh
helmr schedule list --project agents --env production
helmr schedule get SCHEDULE_ID --project agents --env production
```

Change timing or execution defaults by editing source and promoting a new
Deployment. Remove the scheduled declaration and promote to archive it.
