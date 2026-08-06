---
title: Schedules
description: Define a recurring Task in source.
section: Guides
sidebarLabel: Schedules
order: 375
---

# Schedules

Use `schedules.task()` for recurring Task Runs. The declaration must include
cron timing and one per-fire Workspace creation selection.

```ts
import { image, sandbox, schedules } from "@helmr/sdk"

export const maintenanceWorkspace = sandbox({ id: "maintenance" })
  .image(image("maintenance").from("debian:bookworm-slim"))
  .resources({ cpu: 1, memory: "1GiB" })

export const maintenance = schedules.task({
  id: "maintenance",
  cron: { pattern: "0 3 * * *", timezone: "UTC" },
  workspace: { sandbox: maintenanceWorkspace },
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
timezone, pins the exact Task declaration, Sandbox, and Secret identities, and
installs the next fire cursor atomically with the Environment promotion. Each
fire creates a fresh Workspace; inspect the resulting Run's `workspaceId` to
retrieve or reuse it later.

To change timing or execution policy, change source and promote another
Deployment. To stop the Schedule, remove the scheduled Task declaration and
promote. There is intentionally no imperative Schedule mutation path.
