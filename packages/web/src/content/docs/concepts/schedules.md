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
import { image, sandbox, schedules, secrets } from "@helmr/sdk"

export const reporting = sandbox({ id: "reporting" })
  .image(image("reporting").from("debian:bookworm-slim"))
  .resources({ cpu: 1, memory: "1GiB" })

export const dailyReport = schedules.task({
  id: "daily-report",
  cron: { pattern: "0 9 * * *", timezone: "America/New_York" },
  workspace: {
    sandbox: reporting,
    secrets: [{ secret: secrets.fromName("REPORT_TOKEN"), env: "REPORT_TOKEN" }],
  },
  run: async (payload, ctx) => {
    console.log(payload.scheduledAt, ctx.run.cause)
    return { ok: true }
  },
})
```

The Workspace creation selection is required. Promotion validates that the
Sandbox belongs to the candidate Deployment and resolves every Secret name to
its stable identity. It does not create a Workspace. Each logical fire creates
a fresh keyless Workspace with those immutable placements in the same
transaction as its Run. Retries of that Run retain the same Workspace; the next
logical fire receives another Workspace UUID. The Run response exposes that
UUID for later retrieval or reuse.

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
`active`, `errored`, and `archived` status but cannot
mutate Schedule authority.
