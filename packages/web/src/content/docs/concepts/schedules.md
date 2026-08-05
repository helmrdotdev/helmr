---
title: Schedules
description: Source-declared cron automation for Task Runs.
section: Concepts
sidebarLabel: Schedules
order: 165
---

# Schedules

A Schedule is a source-declared timed source of Task Runs. It is not an
imperatively managed resource: there is no create, update, activate,
deactivate, or delete API.

Define a Schedule with `schedules.task()`. Promotion reconciles one durable
Schedule per Task declared ID in each Environment. Removing the declaration
from source archives that Schedule; reintroducing it reuses the same identity
with a new generation.

```ts
import { schedules, workspaces } from "@helmr/sdk"

export const dailyReport = schedules.task({
  id: "daily-report",
  cron: { pattern: "0 9 * * *", timezone: "America/New_York" },
  workspace: workspaces.fromKey("reporting"),
  run: async (payload, ctx) => {
    console.log(payload.scheduledAt, ctx.run.cause)
    return { ok: true }
  },
})
```

The Workspace Address is required. An ID Address must exist when the Deployment
is promoted. A missing key Address leaves the Schedule in
`pending-workspace`; the scheduler binds it when a matching Workspace appears.
The Schedule has no Secret input. Create the matching Workspace separately with
its key and Secret placements; later fires inherit that immutable Workspace
baseline and resolve current active Secret versions at admission.
Read responses preserve the declared Workspace Address and expose
`workspaceId` after it has been bound to a canonical Workspace UUID. An
unresolved `pending-workspace` response has no `workspaceId`.

Scheduled Tasks receive a Helmr-generated payload:

```ts
type ScheduledTaskPayload = {
  scheduledAt: Date
  lastScheduledAt?: Date
  timezone: string
  scheduleId: string
  upcoming: readonly Date[]
}
```

Queue, concurrency, TTL, maximum duration, and retry policy come from the exact
pinned Task declaration. The Schedule row does not duplicate them.

Cron uses the five-field parser from `robfig/cron/v3` v3.0.1: minute, hour,
day of month, month, and day of week. Timezones are exact, case-sensitive IANA
identifiers. Helmr stores the submitted cron expression without defining a
second canonical grammar.

The Console and authenticated APIs are observational. They expose
`pending-workspace`, `active`, `errored`, and `archived` status but cannot
mutate Schedule authority.
