---
title: Schedules
description: Source-declared cron triggers reconciled by Deployment promotion.
---

# Schedules

A Schedule is the durable trigger created from a `schedules.task()` declaration.
It is source-controlled: the public API can list and retrieve Schedules but
does not imperatively create, edit, pause, or delete them.

The declaration includes a five-field cron pattern, an IANA timezone, a
Sandbox, optional Secret placements, the Task handler, and ordinary Run
defaults. Promotion validates and reconciles that declaration in the target
Environment.

Each logical fire creates a fresh keyless Workspace from the declared Sandbox
and placements, then creates a Run for the pinned scheduled Task. Retries of
that Run retain its Workspace; the next fire receives another Workspace.

Scheduled handlers receive a Helmr-generated payload:

```ts
type ScheduledTaskPayload = {
  scheduledAt: Date
  lastScheduledAt?: Date
  timezone: string
  scheduleId: string
  upcoming: readonly Date[]
}
```

The Run cause also records schedule identity and timing. Queue, maximum
duration, queued TTL, and retry policy come from the pinned Task declaration,
not from a separate mutable Schedule configuration.

Changing the source and promoting a new Deployment advances the Schedule
generation. Removing the declaration and promoting archives the Schedule;
reintroducing the same declared ID reuses the durable identity with new
declaration authority. Observational status includes active, errored, and
archived states.
