---
title: Schedules
description: Define a recurring Task in source.
section: Guides
sidebarLabel: Schedules
order: 375
---

# Schedules

Use `schedules.task()` for recurring Task Runs. The declaration must include
cron timing and one Workspace target.

```ts
import { schedules, workspaces } from "@helmr/sdk"

export const maintenance = schedules.task({
  id: "maintenance",
  cron: { pattern: "0 3 * * *", timezone: "UTC" },
  workspace: workspaces.ref({ key: "maintenance" }),
  run: async ({ scheduledAt, lastScheduledAt, upcoming }, ctx) => {
    if (ctx.run.cause.type !== "schedule") {
      throw new Error("maintenance requires a Schedule cause")
    }
    return {
      scheduledAt: scheduledAt.toISOString(),
      previous: lastScheduledAt?.toISOString(),
      next: upcoming[0]?.toISOString(),
    }
  },
})
```

Deploy and promote the project. Promotion validates the cron expression and
timezone, pins the exact Task declaration and Workspace, and installs the next
fire cursor atomically with the Environment promotion.

To change timing or execution policy, change source and promote another
Deployment. To stop the Schedule, remove the scheduled Task declaration and
promote. There is intentionally no imperative Schedule mutation path.

If a key-addressed Workspace does not exist yet, the Schedule remains
`pending-workspace`. Create the matching Workspace; bounded scheduler
reconciliation pins it and activates a new Schedule generation.
