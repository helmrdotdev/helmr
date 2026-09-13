---
title: Schedules
description: Declare scheduled Tasks and inspect reconciled Schedules.
---

# Schedules

`schedules.task(config)` declares a Task whose schedule is reconciled when its
Deployment is promoted.

```ts
export const cleanup = schedules.task({
  id: "cleanup",
  cron: {
    pattern: "0 2 * * *",
    timezone: "UTC",
  },
  workspace: { sandbox: repo },
  run: async ({ scheduledAt }) => ({
    at: scheduledAt.toISOString(),
  }),
})
```

The handler payload contains `scheduledAt`, optional `lastScheduledAt`, and
`timezone`. The declaration also accepts Task defaults and optional Workspace
secret placements.

External Schedule APIs are read-only: `client.schedules.retrieve(id)` and
`list({ cursor?, limit? })`, or exact lookup with `{ taskId }`. Exact task
lookup cannot be combined with pagination. Status is `active`, `errored`, or
`archived`; an errored record includes `lastFailure`. Timing or lifecycle
changes require another source Deployment promotion.

`lastFailure.code` is an opaque string. Known admission diagnostics include
`task_not_found`, `program_unavailable`, `invalid_definition`,
`secret_selection_mismatch`, and `sandbox_not_found`; cron and schedule validation
use `invalid_cron`, `unsupported_cron_version`, or `invalid_schedule`. Keep a generic
handling path for unfamiliar codes. The SDK preserves the code, message and details;
the Schedule status still determines its lifecycle.
